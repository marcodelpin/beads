package spool

import (
	"encoding/json"
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
