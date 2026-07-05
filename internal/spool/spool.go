// File-system layer for the spool. Owns ALL os.* calls so callers (both
// producers and the drainer) can swap implementations or mock for tests.
package spool

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MaxQueueBytes caps queue.jsonl growth. Append refuses new entries past this
// size and returns ErrSpoolFull so the producer can surface the failure.
const MaxQueueBytes int64 = 100 * 1024 * 1024 // 100 MB

// ErrSpoolFull is returned by Append when queue.jsonl >= MaxQueueBytes.
var ErrSpoolFull = errors.New("spool: queue at capacity (100 MB), refusing append")

// Spool is the file-system view of the spool directory. All paths are derived
// from Dir at construction time so callers never assemble them by hand.
type Spool struct {
	Dir          string
	AckedDir     string
	queueFile    string
	inflightFile string
	deadFile     string
	cursorFile   string
	appendLock   string
	poisonFile   string
}

// NewSpool prepares a Spool rooted at dir. The directory tree is created
// lazily on first write so a missing dir does not block start-up.
func NewSpool(dir string) *Spool {
	return &Spool{
		Dir:          dir,
		AckedDir:     filepath.Join(dir, "acked"),
		queueFile:    filepath.Join(dir, "queue.jsonl"),
		inflightFile: filepath.Join(dir, "inflight.jsonl"),
		deadFile:     filepath.Join(dir, "dead-letter.jsonl"),
		cursorFile:   filepath.Join(dir, "cursor.json"),
		appendLock:   filepath.Join(dir, ".append.lock"),
		poisonFile:   filepath.Join(dir, "poison.jsonl"),
	}
}

// withAppendLock runs fn while holding the producer-side append lock. It
// serializes concurrent producers (cap check + append become atomic, closing
// the stat-then-write TOCTOU on MaxQueueBytes) and lets the drainer's
// Compact swap queue.jsonl without racing an in-flight append.
func (s *Spool) withAppendLock(fn func() error) error {
	lk, err := OpenLock(s.appendLock)
	if err != nil {
		return fmt.Errorf("open append lock: %w", err)
	}
	if err := lk.Lock(); err != nil {
		return fmt.Errorf("acquire append lock: %w", err)
	}
	defer func() { _ = lk.Unlock() }()
	return fn()
}

// QueueFile / InflightFile / DeadFile / CursorFile expose the resolved paths
// for diagnostics, status output, and tests.
func (s *Spool) QueueFile() string    { return s.queueFile }
func (s *Spool) InflightFile() string { return s.inflightFile }
func (s *Spool) DeadFile() string     { return s.deadFile }
func (s *Spool) CursorFile() string   { return s.cursorFile }

// EnsureDir makes Dir + AckedDir if missing. Idempotent.
func (s *Spool) EnsureDir() error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return fmt.Errorf("mkdir spool: %w", err)
	}
	if err := os.MkdirAll(s.AckedDir, 0o755); err != nil {
		return fmt.Errorf("mkdir acked: %w", err)
	}
	return nil
}

// LoadCursor reads cursor.json. Missing -> zero value (start from offset 0).
func (s *Spool) LoadCursor() (*Cursor, error) {
	c := &Cursor{}
	data, err := os.ReadFile(s.cursorFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return c, fmt.Errorf("read cursor: %w", err)
	}
	if err := json.Unmarshal(data, c); err != nil {
		return c, fmt.Errorf("parse cursor: %w", err)
	}
	return c, nil
}

// SaveCursor atomically writes cursor.json (write-temp + rename).
func (s *Spool) SaveCursor(c *Cursor) error {
	if err := s.EnsureDir(); err != nil {
		return err
	}
	c.LastDrainTS = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cursor: %w", err)
	}
	tmp := s.cursorFile + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 - internal spool path
	if err != nil {
		return fmt.Errorf("create cursor tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write cursor tmp: %w", err)
	}
	// fsync BEFORE the rename (see WriteInflight): a torn cursor after a
	// hard crash would silently reset or corrupt the drain position.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("sync cursor tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close cursor tmp: %w", err)
	}
	if err := os.Rename(tmp, s.cursorFile); err != nil {
		return fmt.Errorf("rename cursor: %w", err)
	}
	return nil
}

// readJSONLEntries reads every line of path as Entry. Empty/missing -> empty slice.
// Malformed lines are skipped (drainer must not block on a poison row).
func readJSONLEntries(path string) ([]Entry, error) {
	f, err := os.Open(path) // #nosec G304 - internal spool path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 1<<20)
	sc.Buffer(buf, 16<<20) // 16MB max line
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // skip poison rows
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("scan %s: %w", path, err)
	}
	return out, nil
}

// LoadInflight returns the entries left from a previous (possibly crashed)
// drain cycle. Recovery contract: these MUST be retried before pulling new
// items from queue.jsonl.
func (s *Spool) LoadInflight() ([]Entry, error) {
	return readJSONLEntries(s.inflightFile)
}

// WriteInflight overwrites inflight.jsonl atomically. An empty entries slice
// removes the file.
func (s *Spool) WriteInflight(entries []Entry) error {
	if err := s.EnsureDir(); err != nil {
		return err
	}
	if len(entries) == 0 {
		if err := os.Remove(s.inflightFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove inflight: %w", err)
		}
		return nil
	}
	tmp := s.inflightFile + ".tmp"
	f, err := os.Create(tmp) // #nosec G304 - internal spool path
	if err != nil {
		return fmt.Errorf("create inflight tmp: %w", err)
	}
	w := bufio.NewWriter(f)
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("marshal entry %s: %w", e.OpID, err)
		}
		_, _ = w.Write(b)
		_ = w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("flush inflight: %w", err)
	}
	// fsync BEFORE the rename: Flush only reaches the page cache; on a
	// hard crash the rename can become durable with empty/partial DATA
	// (journaling fs) -- exactly the crash class the spool must survive.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("sync inflight: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close inflight: %w", err)
	}
	if err := os.Rename(tmp, s.inflightFile); err != nil {
		return fmt.Errorf("rename inflight: %w", err)
	}
	return nil
}

// PullBatch reads up to batchSize entries from queue.jsonl starting at the
// cursor's offset. Returns (entries, newOffset, queueSize, error).
func (s *Spool) PullBatch(startOffset int64, batchSize int) ([]Entry, int64, int64, error) {
	f, err := os.Open(s.queueFile) // #nosec G304 - internal spool path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, startOffset, 0, nil
		}
		return nil, startOffset, 0, fmt.Errorf("open queue: %w", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, startOffset, 0, fmt.Errorf("stat queue: %w", err)
	}
	queueSize := stat.Size()

	if startOffset > queueSize {
		startOffset = 0
	}

	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return nil, startOffset, queueSize, fmt.Errorf("seek queue: %w", err)
	}

	r := bufio.NewReader(f)
	var out []Entry
	var poisoned int
	cur := startOffset
	for len(out) < batchSize {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			// A tail WITHOUT its newline is a producer's append still in
			// flight (or a torn final write): do NOT consume it -- leave
			// the offset before it so the next pull re-reads the completed
			// line. Consuming half an entry would silently lose the write.
			if err != nil && !strings.HasSuffix(line, "\n") {
				break
			}
			cur += int64(len(line))
			trim := strings.TrimSpace(line)
			if trim != "" {
				var e Entry
				if jerr := json.Unmarshal([]byte(trim), &e); jerr == nil {
					out = append(out, e)
				} else {
					// Newline-terminated but unparseable (corruption,
					// hand-edit): quarantine with a visible trace instead
					// of silently discarding. The cursor advances past it.
					s.quarantinePoison(trim)
					poisoned++
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return out, cur, queueSize, fmt.Errorf("read queue: %w", err)
		}
	}
	if poisoned > 0 {
		fmt.Fprintf(os.Stderr, "Warning: spool: %d malformed queue line(s) moved to %s\n", poisoned, s.poisonFile)
	}
	return out, cur, queueSize, nil
}

// quarantinePoison appends a malformed queue line to poison.jsonl.
// Best-effort: a quarantine failure must not block the drain; the warning
// in PullBatch is the operator's signal either way.
func (s *Spool) quarantinePoison(line string) {
	f, err := os.OpenFile(s.poisonFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) // #nosec G304 - internal spool path
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(f, line)
	_ = f.Sync()
	_ = f.Close()
}

// Compact drops the consumed prefix of queue.jsonl (everything before
// cur.LastAckedOffset) and resets the cursor, so the MaxQueueBytes cap and
// `bd spool status` measure the actual BACKLOG instead of lifetime appended
// volume (GH#4378-review D5). The caller must hold the DRAIN lock; Compact
// takes the APPEND lock for the swap so no concurrent producer append is
// lost. A crash between the queue swap and the cursor save leaves the
// cursor past EOF -- PullBatch resets that to 0 and the SeenSet dedups any
// re-read entries.
func (s *Spool) Compact(cur *Cursor) error {
	if cur == nil || cur.LastAckedOffset <= 0 {
		return nil
	}
	return s.withAppendLock(func() error {
		f, err := os.Open(s.queueFile) // #nosec G304 - internal spool path
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				cur.LastAckedOffset = 0
				return s.SaveCursor(cur)
			}
			return fmt.Errorf("open queue for compact: %w", err)
		}
		stat, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return fmt.Errorf("stat queue for compact: %w", err)
		}
		size := stat.Size()
		off := cur.LastAckedOffset
		if off >= size {
			// Fully consumed: drop the file (Append recreates it).
			_ = f.Close()
			if err := os.Remove(s.queueFile); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove consumed queue: %w", err)
			}
			cur.LastAckedOffset = 0
			return s.SaveCursor(cur)
		}
		// Partially consumed: rewrite the unconsumed tail atomically.
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			_ = f.Close()
			return fmt.Errorf("seek queue for compact: %w", err)
		}
		tmp := s.queueFile + ".tmp"
		out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) // #nosec G304 - internal spool path
		if err != nil {
			_ = f.Close()
			return fmt.Errorf("create queue tmp: %w", err)
		}
		if _, err := io.Copy(out, f); err != nil {
			_ = out.Close()
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("copy queue tail: %w", err)
		}
		_ = f.Close()
		if err := out.Sync(); err != nil {
			_ = out.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("sync queue tmp: %w", err)
		}
		if err := out.Close(); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("close queue tmp: %w", err)
		}
		if err := os.Rename(tmp, s.queueFile); err != nil {
			return fmt.Errorf("rename compacted queue: %w", err)
		}
		cur.LastAckedOffset = 0
		return s.SaveCursor(cur)
	})
}

// fullyEmpty reports whether the spool has nothing left to replay: no
// unconsumed queue backlog and no inflight batch. Missing files count as
// empty.
func (s *Spool) fullyEmpty() bool {
	if has, err := fileHasContent(s.queueFile); err == nil && has {
		return false
	}
	if has, err := fileHasContent(s.inflightFile); err == nil && has {
		return false
	}
	return true
}

// Requeue moves dead-letter entries back into the live queue -- the
// recovery path for misclassified or since-fixed permanent failures
// (previously only a hand-edit could resurrect them). opID selects one
// entry; all=true moves everything valid. Returns the number requeued.
// The CLI takes the drain lock around this so a concurrent drain never
// observes dead-letter.jsonl mid-rewrite.
func (s *Spool) Requeue(opID string, all bool) (int, error) {
	entries, err := s.LoadDeadLetter()
	if err != nil {
		return 0, fmt.Errorf("load dead-letter: %w", err)
	}
	if len(entries) == 0 {
		return 0, nil
	}
	var keep []Entry
	var moved int
	for _, e := range entries {
		if !all && e.OpID != opID {
			keep = append(keep, e)
			continue
		}
		if err := ValidateEntry(e); err != nil {
			// An invalid entry can never replay; keep it dead-lettered.
			fmt.Fprintf(os.Stderr, "Warning: spool requeue: %s invalid, kept in dead-letter: %v\n", e.OpID, err)
			keep = append(keep, e)
			continue
		}
		e.Attempts = 0
		e.LastError = ""
		if err := s.AppendQueue(e); err != nil {
			return moved, fmt.Errorf("requeue %s: %w", e.OpID, err)
		}
		moved++
	}
	if moved > 0 {
		if err := s.WriteDeadLetter(keep); err != nil {
			return moved, fmt.Errorf("rewrite dead-letter: %w", err)
		}
	}
	return moved, nil
}

// AppendAcked writes one entry to acked/YYYY-MM-DD.jsonl (today's UTC date).
func (s *Spool) AppendAcked(e Entry) error {
	if err := s.EnsureDir(); err != nil {
		return err
	}
	day := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(s.AckedDir, day+".jsonl")
	return appendJSONL(path, e)
}

// AppendDeadLetter writes one entry to dead-letter.jsonl.
func (s *Spool) AppendDeadLetter(e Entry) error {
	if err := s.EnsureDir(); err != nil {
		return err
	}
	return appendJSONL(s.deadFile, e)
}

// appendJSONL appends one JSON-marshaled entry + newline to path.
func appendJSONL(path string, e Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 - internal spool path
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return f.Sync()
}

// QueueLineCount returns the current line count of queue.jsonl. Missing -> 0.
func (s *Spool) QueueLineCount() (int64, error) {
	f, err := os.Open(s.queueFile) // #nosec G304 - internal spool path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 1<<20)
	sc.Buffer(buf, 16<<20)
	var n int64
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	return n, sc.Err()
}

// QueueDiskBytes returns queue.jsonl size in bytes (0 if missing).
func (s *Spool) QueueDiskBytes() (int64, error) {
	stat, err := os.Stat(s.queueFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	return stat.Size(), nil
}

// DeadLetterCount returns the number of entries in dead-letter.jsonl.
func (s *Spool) DeadLetterCount() (int64, error) {
	f, err := os.Open(s.deadFile) // #nosec G304 - internal spool path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 1<<20)
	sc.Buffer(buf, 16<<20)
	var n int64
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	return n, sc.Err()
}

// LoadDeadLetter returns every entry in dead-letter.jsonl.
func (s *Spool) LoadDeadLetter() ([]Entry, error) {
	return readJSONLEntries(s.deadFile)
}

// WriteDeadLetter overwrites dead-letter.jsonl with the given entries.
// Empty slice removes the file. Atomic via temp-file + rename.
func (s *Spool) WriteDeadLetter(entries []Entry) error {
	if err := s.EnsureDir(); err != nil {
		return err
	}
	if len(entries) == 0 {
		if err := os.Remove(s.deadFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove dead-letter: %w", err)
		}
		return nil
	}
	tmp := s.deadFile + ".tmp"
	f, err := os.Create(tmp) // #nosec G304 - internal spool path
	if err != nil {
		return fmt.Errorf("create dead-letter tmp: %w", err)
	}
	w := bufio.NewWriter(f)
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("marshal: %w", err)
		}
		_, _ = w.Write(b)
		_ = w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	// fsync BEFORE the rename (see WriteInflight): dead-letter is the
	// forensic record of failed writes -- it must survive a hard crash.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, s.deadFile)
}

// AppendQueue writes one entry back to queue.jsonl. Does NOT enforce
// MaxQueueBytes -- callers using this directly know what they're doing.
func (s *Spool) AppendQueue(e Entry) error {
	if err := s.EnsureDir(); err != nil {
		return err
	}
	// Same append-lock discipline as Append: never write while Compact is
	// swapping the file.
	return s.withAppendLock(func() error {
		return appendJSONL(s.queueFile, e)
	})
}

// InflightOldestAge returns seconds since the oldest inflight entry's TS.
// Empty inflight -> 0.
func (s *Spool) InflightOldestAge() (float64, error) {
	entries, err := s.LoadInflight()
	if err != nil {
		return 0, err
	}
	if len(entries) == 0 {
		return 0, nil
	}
	now := time.Now().UTC()
	var oldest time.Time
	for _, e := range entries {
		t, err := time.Parse(time.RFC3339, e.TS)
		if err != nil {
			continue
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	if oldest.IsZero() {
		return 0, nil
	}
	return now.Sub(oldest).Seconds(), nil
}

// CleanupAcked deletes acked/<YYYY-MM-DD>.jsonl files older than retainDays.
// Returns count deleted. Errors on individual files are non-fatal.
func (s *Spool) CleanupAcked(retainDays int) (int, []error) {
	if retainDays <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(s.AckedDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, []error{fmt.Errorf("read acked dir: %w", err)}
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retainDays)
	deleted := 0
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".jsonl")
		t, err := time.Parse("2006-01-02", base)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			full := filepath.Join(s.AckedDir, e.Name())
			if err := os.Remove(full); err != nil {
				errs = append(errs, fmt.Errorf("remove %s: %w", full, err))
				continue
			}
			deleted++
		}
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return deleted, errs
}
