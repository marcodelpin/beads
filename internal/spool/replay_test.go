package spool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestReplayAfterCrash verifies that entries left in inflight.jsonl from a
// crashed drain are retried before pulling new items from queue.jsonl.
//
// Scenario:
//  1. Write 3 entries to queue.jsonl.
//  2. Simulate crash: move entries 0+1 to inflight.jsonl, leave entry 2 in queue.
//  3. Drain with a dispatcher that always succeeds.
//  4. Verify: all 3 entries dispatched, inflight empty, queue empty, acked has 3.
func TestReplayAfterCrash(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	// Write 3 entries to queue.
	for i := range 3 {
		payload := fmt.Sprintf(`{"id":"bd-crash-%d","title":"entry %d"}`, i, i)
		e := makeEntry("create", payload)
		e.OpID = fmt.Sprintf("crash-op-%04d", i)
		if err := s.AppendQueue(e); err != nil {
			t.Fatalf("AppendQueue %d: %v", i, err)
		}
	}

	// Simulate crash: read first 2 entries from queue into inflight.
	allEntries, err := readJSONLEntries(s.QueueFile())
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	if len(allEntries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(allEntries))
	}
	// Write entries 0+1 to inflight (simulating "pulled but not yet acked").
	if err := s.WriteInflight(allEntries[:2]); err != nil {
		t.Fatalf("write inflight: %v", err)
	}
	// Rewrite queue with only entry 2 (simulating cursor advanced past 0+1).
	if err := os.WriteFile(s.QueueFile(), marshalEntryLine(allEntries[2]), 0o644); err != nil {
		t.Fatalf("rewrite queue: %v", err)
	}

	// Drain.
	var dispatched []string
	drainResult, err := Drain(context.Background(), s, func(e Entry) error {
		dispatched = append(dispatched, e.OpID)
		return nil
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// All 3 should be drained (2 from inflight + 1 from queue).
	if drainResult.Drained != 3 {
		t.Errorf("drained: got %d, want 3", drainResult.Drained)
	}
	if drainResult.Dead != 0 {
		t.Errorf("dead: got %d, want 0", drainResult.Dead)
	}

	// Inflight should be empty.
	inflight, err := s.LoadInflight()
	if err != nil {
		t.Fatalf("LoadInflight: %v", err)
	}
	if len(inflight) != 0 {
		t.Errorf("inflight should be empty, got %d", len(inflight))
	}

	// Queue file is append-only (never truncated); verify cursor advanced
	// past all entries by checking acked count instead.

	// Acked should have 3 entries.
	ackedDir := s.AckedDir
	files, _ := filepath.Glob(filepath.Join(ackedDir, "*.jsonl"))
	totalAcked := 0
	for _, f := range files {
		entries, _ := readJSONLEntries(f)
		totalAcked += len(entries)
	}
	if totalAcked != 3 {
		t.Errorf("acked: got %d entries, want 3", totalAcked)
	}
}

// TestPartialReplay verifies that when op-N succeeds but op-N+1 fails
// permanently, the tail is preserved correctly: op-N is acked, op-N+1 is
// dead-lettered, and op-N+2+ remain in queue.
//
// Scenario:
//  1. Write 3 entries to queue: op-0, op-1, op-2.
//  2. Dispatcher: op-0 succeeds, op-1 returns permanent error, op-2 never reached.
//  3. Verify: op-0 acked, op-1 dead-lettered, op-2 still in queue.
func TestPartialReplay(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	entries := []Entry{
		makeEntry("create", `{"id":"bd-p0","title":"first"}`),
		makeEntry("note", `{"id":"bd-p1","notes":"will-fail"}`),
		makeEntry("update", `{"id":"bd-p2","status":"open"}`),
	}
	entries[0].OpID = "partial-op-0000"
	entries[1].OpID = "partial-op-0001"
	entries[2].OpID = "partial-op-0002"

	for _, e := range entries {
		if err := s.AppendQueue(e); err != nil {
			t.Fatalf("AppendQueue: %v", err)
		}
	}

	permErr := errors.New("duplicate entry: constraint violation")
	var callCount int

	drResult, err := Drain(context.Background(), s, func(e Entry) error {
		callCount++
		if e.OpID == "partial-op-0001" {
			return permErr
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// op-0 drained, op-1 dead-lettered, op-2 not reached (inflight cleared
	// after batch, but op-1 permanent → dead-letter, then batch ends because
	// remaining entries are 0 — op-2 was in same batch as op-1 but after
	// permanent failure it's not automatically processed).
	if drResult.Drained != 1 {
		t.Errorf("drained: got %d, want 1", drResult.Drained)
	}
	if drResult.Dead != 1 {
		t.Errorf("dead: got %d, want 1", drResult.Dead)
	}

	// Dead-letter should have op-1.
	dl, err := s.LoadDeadLetter()
	if err != nil {
		t.Fatalf("LoadDeadLetter: %v", err)
	}
	if len(dl) != 1 {
		t.Fatalf("dead-letter: got %d, want 1", len(dl))
	}
	if dl[0].OpID != "partial-op-0001" {
		t.Errorf("dead-letter op_id: got %q, want partial-op-0001", dl[0].OpID)
	}
	if dl[0].LastError != permErr.Error() {
		t.Errorf("dead-letter last_error: got %q, want %q", dl[0].LastError, permErr.Error())
	}

	// Queue should have remaining entries (op-2 at minimum, possibly more
	// depending on how the batch was structured).
	remaining, err := readJSONLEntries(s.QueueFile())
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	// At least op-2 should remain.
	found := false
	for _, e := range remaining {
		if e.OpID == "partial-op-0002" {
			found = true
			break
		}
	}
	if !found {
		t.Error("op-2 (partial-op-0002) should still be in queue")
	}

	// Acked should have op-0.
	ackedFiles, _ := filepath.Glob(filepath.Join(s.AckedDir, "*.jsonl"))
	totalAcked := 0
	for _, f := range ackedFiles {
		entries, _ := readJSONLEntries(f)
		totalAcked += len(entries)
	}
	if totalAcked != 1 {
		t.Errorf("acked: got %d, want 1", totalAcked)
	}
}

// TestDedupOnRetry verifies that the same op_id dispatched twice is only
// executed once (SeenSet dedup).
//
// Scenario:
//  1. Write 2 entries to queue with distinct op_ids.
//  2. Drain — both dispatched.
//  3. Write same 2 entries again to queue (simulating a retry scenario).
//  4. Drain again — both skipped (SeenSet), dispatched count = 0.
func TestDedupOnRetry(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	e1 := makeEntry("create", `{"id":"bd-dedup-1","title":"first"}`)
	e1.OpID = "dedup-op-0001"
	e2 := makeEntry("note", `{"id":"bd-dedup-2","notes":"second"}`)
	e2.OpID = "dedup-op-0002"

	if err := s.AppendQueue(e1); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendQueue(e2); err != nil {
		t.Fatal(err)
	}

	var callCount int32

	// First drain: both should execute.
	dr1, err := Drain(context.Background(), s, func(e Entry) error {
		atomic.AddInt32(&callCount, 1)
		return nil
	})
	if err != nil {
		t.Fatalf("Drain 1: %v", err)
	}
	if dr1.Drained != 2 {
		t.Errorf("drain 1 drained: got %d, want 2", dr1.Drained)
	}

	// Re-append same entries to queue (simulating external re-enqueue).
	if err := s.AppendQueue(e1); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendQueue(e2); err != nil {
		t.Fatal(err)
	}

	// Reset cursor so Drain reads from start.
	if err := s.SaveCursor(&Cursor{}); err != nil {
		t.Fatal(err)
	}

	// Second drain: both should be skipped (SeenSet).
	dr2, err := Drain(context.Background(), s, func(e Entry) error {
		atomic.AddInt32(&callCount, 1)
		return nil
	})
	if err != nil {
		t.Fatalf("Drain 2: %v", err)
	}
	// All 4 entries processed: 2 dispatched + 2 deduped (SeenSet).
	// Deduped entries still count as drained.
	if dr2.Drained != 4 {
		t.Errorf("drain 2 drained: got %d, want 4 (2 dispatched + 2 deduped)", dr2.Drained)
	}

	totalCalls := atomic.LoadInt32(&callCount)
	if totalCalls != 2 {
		t.Errorf("dispatch called %d times, want 2 (only first drain)", totalCalls)
	}
}

// TestFIFOOrder verifies that entries arriving out of order on disk are still
// dispatched in op_id order (FIFO).
//
// Scenario:
//  1. Write 5 entries to queue in reverse op_id order (op-4, op-3, ..., op-0).
//  2. Drain with a dispatcher that records op_ids.
//  3. Verify dispatch order is op-0, op-1, ..., op-4 (sorted by op_id).
func TestFIFOOrder(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	// Write entries in reverse order.
	for i := 4; i >= 0; i-- {
		payload := fmt.Sprintf(`{"id":"bd-fifo-%d","title":"entry %d"}`, i, i)
		e := makeEntry("create", payload)
		e.OpID = fmt.Sprintf("fifo-op-%04d", i)
		if err := s.AppendQueue(e); err != nil {
			t.Fatalf("AppendQueue %d: %v", i, err)
		}
	}

	var order []string
	dr, err := Drain(context.Background(), s, func(e Entry) error {
		order = append(order, e.OpID)
		return nil
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if dr.Drained != 5 {
		t.Errorf("drained: got %d, want 5", dr.Drained)
	}

	// Verify FIFO order.
	expected := []string{
		"fifo-op-0000", "fifo-op-0001", "fifo-op-0002",
		"fifo-op-0003", "fifo-op-0004",
	}
	if len(order) != len(expected) {
		t.Fatalf("order length: got %d, want %d", len(order), len(expected))
	}
	for i, id := range expected {
		if order[i] != id {
			t.Errorf("order[%d]: got %q, want %q", i, order[i], id)
		}
	}
}

// TestDrainContextCanceled verifies that Drain respects context cancellation.
func TestDrainContextCanceled(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	for i := range 5 {
		payload := fmt.Sprintf(`{"id":"bd-cancel-%d"}`, i)
		e := makeEntry("create", payload)
		e.OpID = fmt.Sprintf("cancel-op-%04d", i)
		if err := s.AppendQueue(e); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := Drain(ctx, s, func(e Entry) error {
		t.Error("dispatch should not be called with canceled context")
		return nil
	})
	if err != nil {
		t.Fatalf("Drain with canceled ctx: %v", err)
	}
}

// TestDrainTransientRetry verifies that transient errors keep entries in
// inflight for retry on next Drain cycle.
func TestDrainTransientRetry(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	e := makeEntry("update", `{"id":"bd-trans","status":"closed"}`)
	e.OpID = "trans-op-0001"
	if err := s.AppendQueue(e); err != nil {
		t.Fatal(err)
	}

	transErr := errors.New("connection refused")
	_, err := Drain(context.Background(), s, func(e Entry) error {
		return transErr
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// Entry should be in inflight for next cycle.
	inflight, err := s.LoadInflight()
	if err != nil {
		t.Fatal(err)
	}
	if len(inflight) != 1 {
		t.Fatalf("inflight: got %d, want 1", len(inflight))
	}
	if inflight[0].OpID != "trans-op-0001" {
		t.Errorf("inflight op_id: got %q, want trans-op-0001", inflight[0].OpID)
	}
	if inflight[0].Attempts != 1 {
		t.Errorf("inflight attempts: got %d, want 1", inflight[0].Attempts)
	}
	if inflight[0].LastError != transErr.Error() {
		t.Errorf("inflight last_error: got %q, want %q", inflight[0].LastError, transErr.Error())
	}
	if inflight[0].FirstFailedAt == "" {
		t.Error("inflight first_failed_at should be set")
	}
}

// TestMaybeDrainLockHeld verifies that MaybeDrain returns nil (not an error)
// when the lock is held by another process.
func TestMaybeDrainLockHeld(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	// Write an entry so MaybeDrain doesn't early-return.
	e := makeEntry("create", `{"id":"bd-maybe"}`)
	if err := s.AppendQueue(e); err != nil {
		t.Fatal(err)
	}

	// Acquire lock externally.
	lockPath := filepath.Join(s.Dir, ".drain.lock")
	lk, err := OpenLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Unlock()
	if err := lk.Lock(); err != nil {
		t.Fatal(err)
	}

	// MaybeDrain should return nil (not ErrLockHeld).
	err = MaybeDrain(context.Background(), s, func(e Entry) error {
		t.Error("should not dispatch while lock is held")
		return nil
	})
	if err != nil {
		t.Errorf("MaybeDrain with lock held: got %v, want nil", err)
	}
}

// TestMaybeDrainEmptySpool verifies that MaybeDrain returns nil cheaply when
// the spool directory is empty.
func TestMaybeDrainEmptySpool(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	err := MaybeDrain(context.Background(), s, func(e Entry) error {
		t.Error("should not dispatch on empty spool")
		return nil
	})
	if err != nil {
		t.Errorf("MaybeDrain empty: %v", err)
	}
}

// TestSeenSetRoundtrip verifies that SeenSet persists to disk and reloads.
func TestSeenSetRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seen.set")

	ss := loadSeenSet(path)
	if ss.Size() != 0 {
		t.Fatalf("initial size: got %d, want 0", ss.Size())
	}

	ss.Add("abc123")
	ss.Add("def456")
	if err := ss.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload.
	ss2 := loadSeenSet(path)
	if !ss2.Contains("abc123") {
		t.Error("should contain abc123 after reload")
	}
	if !ss2.Contains("def456") {
		t.Error("should contain def456 after reload")
	}
	if ss2.Size() != 2 {
		t.Errorf("size: got %d, want 2", ss2.Size())
	}
}

// TestSeenSetPrune verifies that Prune clears the set.
func TestSeenSetPrune(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seen.set")

	ss := loadSeenSet(path)
	ss.Add("op1")
	ss.Add("op2")
	if err := ss.Save(); err != nil {
		t.Fatal(err)
	}

	ss.Prune()
	if ss.Size() != 0 {
		t.Errorf("after prune: got %d, want 0", ss.Size())
	}
	if err := ss.Save(); err != nil {
		t.Fatal(err)
	}

	// Reload should also be empty.
	ss2 := loadSeenSet(path)
	if ss2.Size() != 0 {
		t.Errorf("after prune+reload: got %d, want 0", ss2.Size())
	}
}

// TestDrainStress1000 verifies that a 1000-entry spool drains in reasonable
// time and all entries are processed.
func TestDrainStress1000(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	const n = 1000
	for i := range n {
		payload := fmt.Sprintf(`{"id":"bd-stress-%d","title":"entry %d"}`, i, i)
		e := makeEntry("create", payload)
		e.OpID = fmt.Sprintf("stress-op-%06d", i)
		if err := s.AppendQueue(e); err != nil {
			t.Fatalf("AppendQueue %d: %v", i, err)
		}
	}

	start := time.Now()
	var count int
	dr, err := Drain(context.Background(), s, func(e Entry) error {
		count++
		return nil
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if dr.Drained != n {
		t.Errorf("drained: got %d, want %d", dr.Drained, n)
	}
	if count != n {
		t.Errorf("dispatch count: got %d, want %d", count, n)
	}

	t.Logf("Drained %d entries in %v (%.0f entries/sec)", n, elapsed, float64(n)/elapsed.Seconds())

	// Verify acked.
	ackedFiles, _ := filepath.Glob(filepath.Join(s.AckedDir, "*.jsonl"))
	totalAcked := 0
	for _, f := range ackedFiles {
		entries, _ := readJSONLEntries(f)
		totalAcked += len(entries)
	}
	if totalAcked != n {
		t.Errorf("acked: got %d, want %d", totalAcked, n)
	}
}

// marshalEntryLine marshals a single entry and appends a newline. Helper for
// test setup (not production code).
func marshalEntryLine(e Entry) []byte {
	b, _ := json.Marshal(e)
	return append(b, '\n')
}

// TestSeenSetDurableJournal verifies the per-dispatch journal: an op_id
// recorded via AppendDurable survives a process cut BEFORE Save (a fresh
// loadSeenSet must see it via seen.set.log), and Save folds the journal
// into seen.set and removes it.
func TestSeenSetDurableJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seen.set")

	ss := loadSeenSet(path)
	ss.Add("op-aaa")
	if err := ss.AppendDurable("op-aaa"); err != nil {
		t.Fatalf("AppendDurable: %v", err)
	}
	// Simulate a cut before Save: a fresh load must still dedup op-aaa.
	ss2 := loadSeenSet(path)
	if !ss2.Contains("op-aaa") {
		t.Fatal("journaled op_id lost without Save -- crash-window dedup broken")
	}

	// Save folds the journal into seen.set and removes the log.
	if err := ss2.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path + ".log"); !os.IsNotExist(err) {
		t.Fatalf("seen.set.log should be removed after Save, stat err=%v", err)
	}
	ss3 := loadSeenSet(path)
	if !ss3.Contains("op-aaa") {
		t.Fatal("op_id lost after Save compaction")
	}
}

// TestReplayEntriesJournalsSeenPerDispatch verifies that a successful
// dispatch is journaled durably IMMEDIATELY (not only at end-of-drain
// Save): a fresh SeenSet loaded mid-batch already dedups the entry.
func TestReplayEntriesJournalsSeenPerDispatch(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(dir)
	if err := s.EnsureDir(); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}

	seenPath := filepath.Join(dir, "seen.set")
	seen := loadSeenSet(seenPath)

	e := Entry{OpID: "11aa", Op: "create", Payload: []byte(`{}`)}
	dispatched := 0
	remaining, drained, dead, err := replayEntries(context.Background(), []Entry{e},
		func(Entry) error { dispatched++; return nil }, seen, s)
	if err != nil || len(remaining) != 0 || drained != 1 || dead != 0 || dispatched != 1 {
		t.Fatalf("replayEntries: remaining=%d drained=%d dead=%d dispatched=%d err=%v",
			len(remaining), drained, dead, dispatched, err)
	}

	// WITHOUT calling seen.Save(): a fresh load must already contain the op.
	fresh := loadSeenSet(seenPath)
	if !fresh.Contains("11aa") {
		t.Fatal("successful dispatch not durably journaled per-dispatch")
	}
}
