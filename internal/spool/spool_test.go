package spool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenAndClose(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))
	if s.Dir == "" {
		t.Fatal("Dir is empty")
	}
	if s.QueueFile() == "" {
		t.Fatal("QueueFile is empty")
	}
	if err := s.EnsureDir(); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if _, err := os.Stat(s.Dir); err != nil {
		t.Fatalf("Dir not created: %v", err)
	}
	if _, err := os.Stat(s.AckedDir); err != nil {
		t.Fatalf("AckedDir not created: %v", err)
	}
}

func TestAppendAndReadQueue(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	e := makeEntry("create", `{"title":"test"}`)
	if err := s.AppendQueue(e); err != nil {
		t.Fatalf("AppendQueue: %v", err)
	}

	entries, err := readJSONLEntries(s.QueueFile())
	if err != nil {
		t.Fatalf("readJSONLEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].OpID != e.OpID {
		t.Errorf("OpID: got %q, want %q", entries[0].OpID, e.OpID)
	}
}

func TestDiskCapErrSpoolFull(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))
	if err := s.EnsureDir(); err != nil {
		t.Fatal(err)
	}

	// Write a file larger than MaxQueueBytes to simulate a full queue.
	big := make([]byte, MaxQueueBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(s.QueueFile(), big, 0o644); err != nil {
		t.Fatal(err)
	}

	// QueueDiskBytes should report the size.
	size, err := s.QueueDiskBytes()
	if err != nil {
		t.Fatal(err)
	}
	if size < MaxQueueBytes {
		t.Fatalf("size %d < MaxQueueBytes %d", size, MaxQueueBytes)
	}
}

func TestAtomicTempRename(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	e := makeEntry("update", `{"id":"bd-1","status":"closed"}`)
	if err := s.WriteInflight([]Entry{e}); err != nil {
		t.Fatalf("WriteInflight: %v", err)
	}

	// Verify no .tmp file left behind.
	matches, _ := filepath.Glob(filepath.Join(s.Dir, "*.tmp"))
	if len(matches) > 0 {
		t.Errorf("leftover .tmp files: %v", matches)
	}

	// Read back.
	entries, err := s.LoadInflight()
	if err != nil {
		t.Fatalf("LoadInflight: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].OpID != e.OpID {
		t.Errorf("OpID: got %q, want %q", entries[0].OpID, e.OpID)
	}
}

func TestMalformedLineSkip(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))
	if err := s.EnsureDir(); err != nil {
		t.Fatal(err)
	}

	// Write queue.jsonl with a mix of valid and malformed lines.
	valid := makeEntry("create", `{"title":"ok"}`)
	validData, _ := json.Marshal(valid)

	var buf strings.Builder
	buf.WriteString("THIS IS NOT JSON\n")
	buf.WriteString(string(validData) + "\n")
	buf.WriteString("{\"partial\":\n")
	buf.WriteString(string(validData) + "\n")

	if err := os.WriteFile(s.QueueFile(), []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := readJSONLEntries(s.QueueFile())
	if err != nil {
		t.Fatalf("readJSONLEntries: %v", err)
	}
	// Should get exactly 2 valid entries, skipping 2 malformed.
	if len(entries) != 2 {
		t.Errorf("got %d entries, want 2", len(entries))
	}
}

func TestCursorRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	c := &Cursor{
		LastAckedOffset: 42,
		QueueSize:       1024,
		DeadLetterCount: 3,
	}
	if err := s.SaveCursor(c); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}

	loaded, err := s.LoadCursor()
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if loaded.LastAckedOffset != 42 {
		t.Errorf("LastAckedOffset: got %d, want 42", loaded.LastAckedOffset)
	}
	if loaded.QueueSize != 1024 {
		t.Errorf("QueueSize: got %d, want 1024", loaded.QueueSize)
	}
	if loaded.DeadLetterCount != 3 {
		t.Errorf("DeadLetterCount: got %d, want 3", loaded.DeadLetterCount)
	}
}

func TestCursorMissingFileReturnsZero(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	c, err := s.LoadCursor()
	if err != nil {
		t.Fatalf("LoadCursor on missing: %v", err)
	}
	if c.LastAckedOffset != 0 || c.QueueSize != 0 {
		t.Errorf("expected zero cursor, got %+v", c)
	}
}

func TestDeadLetterRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	e := makeEntry("note", `{"id":"bd-x","notes":"bad"}`)
	if err := s.AppendDeadLetter(e); err != nil {
		t.Fatalf("AppendDeadLetter: %v", err)
	}

	count, err := s.DeadLetterCount()
	if err != nil {
		t.Fatalf("DeadLetterCount: %v", err)
	}
	if count != 1 {
		t.Errorf("count: got %d, want 1", count)
	}

	entries, err := s.LoadDeadLetter()
	if err != nil {
		t.Fatalf("LoadDeadLetter: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].OpID != e.OpID {
		t.Errorf("OpID: got %q, want %q", entries[0].OpID, e.OpID)
	}
}

func TestWriteDeadLetterEmptyRemoves(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	if err := s.AppendDeadLetter(makeEntry("close", `{}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteDeadLetter(nil); err != nil {
		t.Fatalf("WriteDeadLetter(nil): %v", err)
	}
	if _, err := os.Stat(s.DeadFile()); !os.IsNotExist(err) {
		t.Error("expected dead-letter.jsonl to be removed")
	}
}

func TestCleanupAcked(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))
	if err := s.EnsureDir(); err != nil {
		t.Fatal(err)
	}

	// Create a fake acked file with an old date.
	oldFile := filepath.Join(s.AckedDir, "2020-01-01.jsonl")
	if err := os.WriteFile(oldFile, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deleted, errs := s.CleanupAcked(7)
	if len(errs) > 0 {
		t.Errorf("unexpected errors: %v", errs)
	}
	if deleted != 1 {
		t.Errorf("deleted: got %d, want 1", deleted)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Error("old acked file should be deleted")
	}
}

func makeEntry(op, payload string) Entry {
	return Entry{
		OpID:          "test-" + op,
		TS:            "2026-05-13T10:00:00Z",
		Op:            op,
		Payload:       json.RawMessage(payload),
		SchemaVersion: 1,
	}
}

// TestCompactFullyConsumed: after a full drain the queue file is dropped and
// the cursor resets, so the disk cap measures backlog instead of lifetime
// volume (GH#4378-review D5).
func TestCompactFullyConsumed(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(dir)
	for i := 0; i < 3; i++ {
		if _, err := s.Append(context.Background(), "note", []byte(fmt.Sprintf(`{"id":"c-%d"}`, i)), false, "test"); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	size, err := s.QueueDiskBytes()
	if err != nil || size == 0 {
		t.Fatalf("queue size = %d err=%v", size, err)
	}
	cur := &Cursor{LastAckedOffset: size}
	if err := s.Compact(cur); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if cur.LastAckedOffset != 0 {
		t.Fatalf("cursor offset = %d, want 0", cur.LastAckedOffset)
	}
	if size, _ := s.QueueDiskBytes(); size != 0 {
		t.Fatalf("queue size after compact = %d, want 0", size)
	}
	reloaded, err := s.LoadCursor()
	if err != nil || reloaded.LastAckedOffset != 0 {
		t.Fatalf("reloaded cursor offset = %d err=%v, want persisted 0", reloaded.LastAckedOffset, err)
	}
}

// TestCompactPreservesUnconsumedTail: a partial drain keeps the unconsumed
// entries, byte-identical, at offset 0.
func TestCompactPreservesUnconsumedTail(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(dir)
	var entries []Entry
	for i := 0; i < 3; i++ {
		e, err := s.Append(context.Background(), "note", []byte(fmt.Sprintf(`{"id":"t-%d"}`, i)), false, "test")
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		entries = append(entries, e)
	}
	// Consume exactly the first entry.
	batch, off, _, err := s.PullBatch(0, 1)
	if err != nil || len(batch) != 1 {
		t.Fatalf("PullBatch: n=%d err=%v", len(batch), err)
	}
	cur := &Cursor{LastAckedOffset: off}
	if err := s.Compact(cur); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	tail, _, _, err := s.PullBatch(0, 10)
	if err != nil {
		t.Fatalf("PullBatch post-compact: %v", err)
	}
	if len(tail) != 2 || tail[0].OpID != entries[1].OpID || tail[1].OpID != entries[2].OpID {
		t.Fatalf("tail = %d entries (want the 2 unconsumed, in order)", len(tail))
	}
}

// TestPullBatchLeavesTornTailUnconsumed: a final line WITHOUT its newline is
// a producer's append still in flight -- the offset must stop BEFORE it.
func TestPullBatchLeavesTornTailUnconsumed(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(dir)
	e, err := s.Append(context.Background(), "note", []byte(`{"id":"whole"}`), false, "test")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	// Simulate a torn/in-flight append: half an entry, no newline.
	f, err := os.OpenFile(s.QueueFile(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	if _, err := f.WriteString(`{"op_id":"half`); err != nil {
		t.Fatalf("write torn tail: %v", err)
	}
	_ = f.Close()

	stat, _ := os.Stat(s.QueueFile())
	batch, off, _, err := s.PullBatch(0, 10)
	if err != nil {
		t.Fatalf("PullBatch: %v", err)
	}
	if len(batch) != 1 || batch[0].OpID != e.OpID {
		t.Fatalf("batch = %d entries, want only the complete one", len(batch))
	}
	if off >= stat.Size() {
		t.Fatalf("offset %d consumed the torn tail (file size %d) -- in-flight append would be lost", off, stat.Size())
	}
}

// TestPullBatchQuarantinesMalformedLine: a newline-terminated but unparseable
// line is moved to poison.jsonl and the cursor advances past it.
func TestPullBatchQuarantinesMalformedLine(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(dir)
	if err := s.EnsureDir(); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	// Hand-write: malformed line, then a valid entry.
	e := Entry{OpID: "aa11", Op: "note", Payload: []byte(`{}`), SchemaVersion: 1}
	f, err := os.OpenFile(s.QueueFile(), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("create queue: %v", err)
	}
	if _, err := f.WriteString("this is not json\n"); err != nil {
		t.Fatalf("write poison: %v", err)
	}
	if _, err := f.Write(append(marshalEntryLineSpoolTest(t, e), '\n')); err != nil {
		t.Fatalf("write entry: %v", err)
	}
	_ = f.Close()

	batch, _, _, err := s.PullBatch(0, 10)
	if err != nil {
		t.Fatalf("PullBatch: %v", err)
	}
	if len(batch) != 1 || batch[0].OpID != "aa11" {
		t.Fatalf("batch = %d entries, want the 1 valid one", len(batch))
	}
	poison, err := os.ReadFile(filepath.Join(dir, "poison.jsonl"))
	if err != nil {
		t.Fatalf("poison.jsonl missing: %v", err)
	}
	if !strings.Contains(string(poison), "this is not json") {
		t.Fatalf("poison.jsonl does not contain the quarantined line: %q", poison)
	}
}

func marshalEntryLineSpoolTest(t *testing.T, e Entry) []byte {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestRequeueMovesDeadLetterBack: dead-letter entries return to the live
// queue with reset attempt counters; unselected ones stay dead-lettered.
func TestRequeueMovesDeadLetterBack(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(dir)
	if err := s.EnsureDir(); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	dead := []Entry{
		{OpID: "d1", Op: "note", Payload: []byte(`{"id":"x"}`), Attempts: 3, LastError: "constraint", SchemaVersion: 1, TS: "2026-07-05T00:00:00Z", ContentHash: "h1"},
		{OpID: "d2", Op: "close", Payload: []byte(`{"id":"y"}`), Attempts: 1, LastError: "constraint", SchemaVersion: 1, TS: "2026-07-05T00:00:00Z", ContentHash: "h2"},
	}
	if err := s.WriteDeadLetter(dead); err != nil {
		t.Fatalf("WriteDeadLetter: %v", err)
	}

	moved, err := s.Requeue("d1", false)
	if err != nil || moved != 1 {
		t.Fatalf("Requeue: moved=%d err=%v", moved, err)
	}
	queued, _, _, err := s.PullBatch(0, 10)
	if err != nil || len(queued) != 1 || queued[0].OpID != "d1" || queued[0].Attempts != 0 || queued[0].LastError != "" {
		t.Fatalf("queued = %+v err=%v, want d1 with reset counters", queued, err)
	}
	remaining, err := s.LoadDeadLetter()
	if err != nil || len(remaining) != 1 || remaining[0].OpID != "d2" {
		t.Fatalf("dead-letter after requeue = %+v err=%v, want only d2", remaining, err)
	}
}
