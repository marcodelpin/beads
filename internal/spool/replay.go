package spool

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ackedRetainDays is the acked/ history retention the package doc promises;
// CleanupAcked enforces it at the end of every successful drain.
const ackedRetainDays = 7

// DrainResult holds counters from a Drain cycle.
type DrainResult struct {
	Drained int // entries successfully dispatched
	Dead    int // entries moved to dead-letter
}

// DispatchFunc is the callback the drainer invokes per entry. It must return
// nil on success, a transient error (retry later), or a permanent error
// (dead-letter). The caller classifies via IsTransientErr.
type DispatchFunc func(Entry) error

// Drain is the high-level replay orchestrator. It:
//  1. Acquires an exclusive lock on .drain.lock (returns ErrLockHeld on contention).
//  2. Retries any entries left in inflight.jsonl from a previous crashed drain.
//  3. Pulls batches from queue.jsonl and dispatches each entry.
//  4. On success: moves entry to acked/ + records op_id in SeenSet.
//  5. On permanent failure: moves entry to dead-letter.jsonl.
//  6. On transient failure: rewrites remaining to inflight.jsonl for next cycle.
//
// FIFO order is preserved: entries are sorted by OpID (monotonic hex) before
// dispatch, ensuring deterministic replay even if disk reads arrive out of order.
func Drain(ctx context.Context, s *Spool, dispatch DispatchFunc) (DrainResult, error) {
	return drainInternal(ctx, s, dispatch, false)
}

// MaybeDrain is the opportunistic entrypoint for bd's PersistentPreRun. It
// attempts a non-blocking try-lock; if the lock is held (another process is
// draining), it returns nil immediately. If the spool directory is missing or
// both queue.jsonl and inflight.jsonl are empty/missing, it returns nil
// cheaply (<1ms).
func MaybeDrain(ctx context.Context, s *Spool, dispatch DispatchFunc) error {
	// Quick check: is there anything to drain?
	queueHasContent, err := fileHasContent(s.QueueFile())
	if err != nil {
		return nil // spool dir missing -> nothing to do
	}
	inflightHasContent, err := fileHasContent(s.InflightFile())
	if err != nil {
		inflightHasContent = false
	}
	if !queueHasContent && !inflightHasContent {
		return nil // nothing to drain
	}

	_, err = drainInternal(ctx, s, dispatch, true)
	if errors.Is(err, ErrLockHeld) {
		return nil // another process is draining -- not an error
	}
	return err
}

// drainInternal is the shared implementation. When tryLock is true, it uses
// TryLock (non-blocking) instead of Lock (blocking).
func drainInternal(ctx context.Context, s *Spool, dispatch DispatchFunc, tryLock bool) (DrainResult, error) {
	var result DrainResult

	if err := s.EnsureDir(); err != nil {
		return result, fmt.Errorf("spool ensure dir: %w", err)
	}

	// Acquire drain lock.
	lockPath := filepath.Join(s.Dir, ".drain.lock")
	lk, err := OpenLock(lockPath)
	if err != nil {
		return result, fmt.Errorf("open lock: %w", err)
	}
	defer func() { _ = lk.Unlock() }()

	if tryLock {
		if err := lk.TryLock(); err != nil {
			return result, ErrLockHeld
		}
	} else {
		if err := lk.Lock(); err != nil {
			return result, fmt.Errorf("acquire lock: %w", err)
		}
	}

	// Load SeenSet for dedup.
	seen := loadSeenSet(filepath.Join(s.Dir, "seen.set"))
	defer func() {
		_ = seen.Save()
	}()

	// Phase 1: retry inflight entries from a previous crashed drain.
	inflight, err := s.LoadInflight()
	if err != nil {
		return result, fmt.Errorf("load inflight: %w", err)
	}
	if len(inflight) > 0 {
		remaining, dr, dead, err := replayEntries(ctx, inflight, dispatch, seen, s)
		result.Drained += dr
		result.Dead += dead
		if err != nil {
			return result, err
		}
		if err := s.WriteInflight(remaining); err != nil {
			return result, fmt.Errorf("write inflight: %w", err)
		}
	}

	// Phase 2: pull batches from queue.jsonl.
	cursor, err := s.LoadCursor()
	if err != nil {
		return result, fmt.Errorf("load cursor: %w", err)
	}

	const batchSize = 10
	for {
		if ctx.Err() != nil {
			break
		}

		entries, newOffset, queueSize, err := s.PullBatch(cursor.LastAckedOffset, batchSize)
		if err != nil {
			return result, fmt.Errorf("pull batch: %w", err)
		}
		cursor.QueueSize = queueSize

		if len(entries) == 0 {
			break
		}

		// Write pulled entries to inflight (crash recovery).
		if err := s.WriteInflight(entries); err != nil {
			return result, fmt.Errorf("write inflight before dispatch: %w", err)
		}

		remaining, dr, dead, err := replayEntries(ctx, entries, dispatch, seen, s)
		result.Drained += dr
		result.Dead += dead

		// On transient error, compute how far we actually advanced.
		// Entries past the failure stay in queue for next cycle.
		if err != nil {
			_ = s.WriteInflight(remaining)
			cursor.LastAckedOffset = computeProcessedOffset(s.QueueFile(), cursor.LastAckedOffset, entries, remaining)
			_ = s.SaveCursor(cursor)
			return result, err
		}

		// On permanent error (dead > 0, err == nil), cursor advances
		// only past entries that were actually processed before the failure.
		if dead > 0 {
			_ = s.WriteInflight(remaining)
			cursor.LastAckedOffset = computeProcessedOffset(s.QueueFile(), cursor.LastAckedOffset, entries, remaining)
			_ = s.SaveCursor(cursor)
			return result, nil
		}

		// Write remaining entries to inflight. If replayEntries hit a transient
		// failure without returning err (entry appended to remaining + break),
		// `remaining` contains the failed entry -- writing it back to inflight
		// preserves the retry state. If `remaining` is empty (all dispatched
		// successfully), this writes nil = clears inflight.
		if err := s.WriteInflight(remaining); err != nil {
			return result, fmt.Errorf("write inflight after dispatch: %w", err)
		}

		// If all entries were skipped (dedup), don't advance cursor.
		if len(remaining) < len(entries) {
			cursor.LastAckedOffset = newOffset
		}
		if err := s.SaveCursor(cursor); err != nil {
			return result, fmt.Errorf("save cursor: %w", err)
		}

		// Zero forward progress: a transient dispatch failure keeps the
		// whole batch in remaining (replayEntries returns nil err for it),
		// so with a full batch the loop would re-pull the same entries
		// forever while the server stays down (GH#4378-review D2). Stop
		// this cycle -- inflight already holds the batch, the next drain
		// retries it.
		if dr == 0 && dead == 0 && len(remaining) == len(entries) {
			break
		}

		if len(entries) < batchSize {
			break // last batch
		}
	}

	// Final cursor save.
	if err := s.SaveCursor(cursor); err != nil {
		return result, fmt.Errorf("save cursor final: %w", err)
	}

	// Storage hygiene (GH#4378-review D5): drop the consumed queue prefix
	// so MaxQueueBytes and `bd spool status` measure BACKLOG, not lifetime
	// appended volume. Runs under the drain lock; Compact takes the append
	// lock for the file swap.
	if err := s.Compact(cursor); err != nil {
		return result, fmt.Errorf("compact queue: %w", err)
	}

	// Retention: enforce the 7-day acked/ contract the package doc
	// promises (was never wired). Best-effort -- retention failures must
	// not fail a successful drain.
	if _, errs := s.CleanupAcked(ackedRetainDays); len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "Warning: spool: acked cleanup: %d error(s)\n", len(errs))
	}
	// The seen-set's dedup window only matters while entries can still
	// replay: once queue AND inflight are empty nothing it remembers can
	// recur, so reset it before it grows unbounded.
	if s.fullyEmpty() {
		seen.Prune()
		_ = seen.Save()
	}

	return result, nil
}

// replayEntries dispatches each entry via dispatch. On success the entry is
// acked + added to SeenSet. On permanent failure it's moved to dead-letter.
// Transient failures cause the entry (and all subsequent) to be returned as
// remaining (to be written to inflight).
//
// Entries are sorted by OpID for FIFO order before processing.
// Returns: remaining entries (for inflight), drained count, dead count, error.
func replayEntries(ctx context.Context, entries []Entry, dispatch DispatchFunc, seen *SeenSet, s *Spool) ([]Entry, int, int, error) {
	// Sort by OpID for FIFO order (monotonic hex ensures temporal order).
	sorted := make([]Entry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].OpID < sorted[j].OpID
	})

	var remaining []Entry
	var drained, dead int

	for i, e := range sorted {
		if ctx.Err() != nil {
			remaining = append(remaining, sorted[i:]...)
			break
		}

		// Dedup: skip already-seen op_ids.
		if seen.Contains(e.OpID) {
			drained++ // count as drained (already applied)
			continue
		}

		err := dispatch(e)
		if err == nil {
			// Success: ack + mark seen.
			if ackErr := s.AppendAcked(e); ackErr != nil {
				return nil, drained, dead, fmt.Errorf("ack entry %s: %w", e.OpID, ackErr)
			}
			seen.Add(e.OpID)
			// Make the dedup durable per-dispatch, not only at end-of-drain:
			// a drain cut between this dispatch and the final seen.Save must
			// not replay the entry. Best-effort -- on failure we degrade to
			// the pre-journal behavior (end-of-drain Save only).
			_ = seen.AppendDurable(e.OpID)
			drained++
			continue
		}

		// Classify error. A canceled drain context (process teardown via
		// joinSpoolDrain, or Ctrl-C during a drain) is ALWAYS the transient
		// path regardless of how the wrapped SQL error classifies:
		// context.Canceled is no longer in IsTransientErr's transient set
		// (a canceled FOREGROUND write must not be spooled), but a dispatch
		// aborted by drain-cancel must stay queued, never dead-letter.
		if IsTransientErr(err) || ctx.Err() != nil {
			// Transient: keep this + all subsequent in remaining.
			e.Attempts++
			e.LastError = err.Error()
			if e.FirstFailedAt == "" {
				e.FirstFailedAt = time.Now().UTC().Format(time.RFC3339)
			}
			remaining = append(remaining, e)
			if i+1 < len(sorted) {
				remaining = append(remaining, sorted[i+1:]...)
			}
			break
		}

		// Permanent: dead-letter. Stop processing this batch -- entries
		// after the failed one stay in queue for the next drain cycle.
		e.LastError = err.Error()
		if dlErr := s.AppendDeadLetter(e); dlErr != nil {
			return nil, drained, dead, fmt.Errorf("dead-letter entry %s: %w", e.OpID, dlErr)
		}
		dead++
		break
	}

	return remaining, drained, dead, nil
}

// computeProcessedOffset returns the byte offset after the entries that were
// actually processed (acked or dead-lettered). This is the batch entries minus
// the remaining entries -- i.e., the entries at the start of sorted that were
// handled before a failure or completion.
func computeProcessedOffset(queuePath string, startOffset int64, batch, remaining []Entry) int64 {
	processed := len(batch) - len(remaining)
	if processed <= 0 {
		return startOffset
	}
	f, err := os.Open(queuePath) // #nosec G304 - internal spool path
	if err != nil {
		return startOffset
	}
	defer f.Close()
	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return startOffset
	}
	r := bufio.NewReader(f)
	cur := startOffset
	for range processed {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			cur += int64(len(line))
		}
		if err != nil {
			break
		}
	}
	return cur
}

// SeenSet is an in-memory set of op_ids persisted to a file for crash recovery.
// The file format is one op_id per line (plain text, not JSONL).
type SeenSet struct {
	mu   sync.RWMutex
	ids  map[string]bool
	path string
}

// loadSeenSet reads the seen-set file into memory, then unions the durable
// per-dispatch journal (seen.set.log) written by AppendDurable: after a crash
// or an unwaited early exit the journal carries op_ids acked after the last
// full Save, so replay never re-applies them. Missing files -> empty set.
func loadSeenSet(path string) *SeenSet {
	ss := &SeenSet{
		ids:  make(map[string]bool),
		path: path,
	}
	for _, p := range []string{path, path + ".log"} {
		f, err := os.Open(p) // #nosec G304 - internal spool path
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			id := strings.TrimSpace(sc.Text())
			if id != "" {
				ss.ids[id] = true
			}
		}
		_ = f.Close()
	}
	return ss
}

// Contains reports whether op_id is in the set.
func (ss *SeenSet) Contains(opID string) bool {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.ids[opID]
}

// Add inserts op_id into the set.
func (ss *SeenSet) Add(opID string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.ids[opID] = true
}

// AppendDurable records op_id in the fsync'd journal (seen.set.log) so the
// dedup survives a process exit BEFORE the end-of-drain Save. Without this,
// a drain cut mid-cycle (crash, or a fast command exiting under the old
// un-awaited goroutine) would replay already-dispatched entries on the next
// run. The journal is folded back into seen.set (and removed) by Save.
func (ss *SeenSet) AppendDurable(opID string) error {
	if ss.path == "" {
		return nil
	}
	f, err := os.OpenFile(ss.path+".log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) // #nosec G304 - internal spool path
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, opID); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Save persists the set to disk atomically (write-temp + rename), then
// removes the AppendDurable journal: its op_ids are in memory (Add precedes
// AppendDurable) and therefore in the freshly-written set. A crash between
// rename and remove is harmless -- loadSeenSet unions both files.
func (ss *SeenSet) Save() error {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	if ss.path == "" {
		return nil
	}

	dir := filepath.Dir(ss.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir seen: %w", err)
	}

	// Sort for deterministic output.
	ids := make([]string, 0, len(ss.ids))
	for id := range ss.ids {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	tmp := ss.path + ".tmp"
	f, err := os.Create(tmp) // #nosec G304 - internal spool path
	if err != nil {
		return fmt.Errorf("create seen tmp: %w", err)
	}
	w := bufio.NewWriter(f)
	for _, id := range ids {
		fmt.Fprintln(w, id)
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("flush seen: %w", err)
	}
	// fsync BEFORE the rename (see WriteInflight): losing seen.set to a
	// torn write would re-open the duplicate-replay window the durable
	// journal exists to close.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("sync seen: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, ss.path); err != nil {
		return err
	}
	_ = os.Remove(ss.path + ".log")
	return nil
}

// Size returns the number of entries in the set.
func (ss *SeenSet) Size() int {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return len(ss.ids)
}

// Prune removes op_ids older than maxAge from the seen set by rewriting the
// file without them. Since SeenSet entries don't carry timestamps, this is a
// full reset -- the caller should only invoke this during CleanupAcked cycles
// when acked/ retention guarantees all seen op_ids have durable history.
func (ss *SeenSet) Prune() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.ids = make(map[string]bool)
}

// fileHasContent returns true if the file exists and has at least one
// non-empty line. Used by MaybeDrain for a cheap pre-check.
func fileHasContent(path string) (bool, error) {
	f, err := os.Open(path) // #nosec G304 - internal spool path
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			return true, nil
		}
	}
	return false, sc.Err()
}
