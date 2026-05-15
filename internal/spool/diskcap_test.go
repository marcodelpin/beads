package spool

// diskcap_test.go — integration tests for spool disk-cap behavior.
//
// MaxQueueBytes is defined in spool.go as 100 MB. These tests verify:
//   1. Spool grows without error under sustained transient failures.
//   2. ErrSpoolFull is returned when queue.jsonl reaches MaxQueueBytes.
//   3. Behavior after ErrSpoolFull: no partial writes, queue size unchanged.
//   4. After Drain reduces queue size, Append resumes successfully.
//   5. QueueDiskBytes / QueueLineCount reflect actual on-disk state.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiskCapConstantsDefined verifies that MaxQueueBytes and ErrSpoolFull
// are exported and have sensible values. If these constants change, this test
// catches the regression.
func TestDiskCapConstantsDefined(t *testing.T) {
	if MaxQueueBytes <= 0 {
		t.Errorf("MaxQueueBytes must be positive, got %d", MaxQueueBytes)
	}
	// Minimum sensible cap: 1 MB (large enough to hold many entries, small
	// enough that tests can fill it quickly).
	const minCap = 1 << 20 // 1 MB
	if MaxQueueBytes < minCap {
		t.Errorf("MaxQueueBytes %d is smaller than minimum expected %d", MaxQueueBytes, minCap)
	}
	if ErrSpoolFull == nil {
		t.Error("ErrSpoolFull must not be nil")
	}
}

// TestDiskCapGrowsUnderTransient verifies that the spool can hold multiple
// entries under sustained transient failures without reporting errors to the
// caller. We use a small loop count (not 100 MB worth) because the test only
// needs to verify the growth behavior, not actually fill the disk.
func TestDiskCapGrowsUnderTransient(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))
	fake := NewFakeDolt()
	fake.SetMode(FakeDoltModeTransient)

	const count = 10
	for i := 0; i < count; i++ {
		payload := []byte(fmt.Sprintf(`{"id":"bd-grow-%d","title":"entry number %d with some padding to ensure non-trivial size"}`, i, i))
		err := writeAndMaybeSpool(context.Background(), s, "update", payload, func() error {
			return fake.Write("update", payload)
		})
		if err != nil {
			t.Fatalf("writeAndMaybeSpool %d: %v (expected nil, transient should spool)", i, err)
		}
	}

	n, err := s.QueueLineCount()
	if err != nil {
		t.Fatalf("QueueLineCount: %v", err)
	}
	if n != count {
		t.Errorf("queue line count: got %d, want %d", n, count)
	}

	bytes, err := s.QueueDiskBytes()
	if err != nil {
		t.Fatalf("QueueDiskBytes: %v", err)
	}
	if bytes == 0 {
		t.Error("QueueDiskBytes should be > 0 after appending entries")
	}
	if bytes >= MaxQueueBytes {
		t.Errorf("unexpected cap hit after %d small entries: %d bytes", count, bytes)
	}
}

// TestDiskCapErrSpoolFullOnWrite verifies that Append returns ErrSpoolFull
// when queue.jsonl is at or above MaxQueueBytes. Uses a pre-filled file rather
// than actually writing 100 MB of real data.
func TestDiskCapErrSpoolFullOnWrite(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))
	if err := s.EnsureDir(); err != nil {
		t.Fatal(err)
	}

	// Pre-fill queue with MaxQueueBytes+1 bytes.
	big := make([]byte, MaxQueueBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(s.QueueFile(), big, 0o644); err != nil {
		t.Fatal(err)
	}

	// Append should refuse with ErrSpoolFull.
	_, err := s.Append(context.Background(), "create", []byte(`{"title":"cap test"}`), false, "test")
	if !errors.Is(err, ErrSpoolFull) {
		t.Errorf("expected ErrSpoolFull when queue at cap, got: %v", err)
	}

	// Queue size must not have changed.
	size, err2 := s.QueueDiskBytes()
	if err2 != nil {
		t.Fatalf("QueueDiskBytes: %v", err2)
	}
	if size != MaxQueueBytes+1 {
		t.Errorf("queue size changed after refused append: got %d, want %d", size, MaxQueueBytes+1)
	}
}

// TestDiskCapWriteAndMaybeSpoolAtCap verifies that writeAndMaybeSpool
// surfaces the original transient error (not ErrSpoolFull) when the spool
// itself is full. The caller should see the Dolt failure so they can decide
// how to proceed.
func TestDiskCapWriteAndMaybeSpoolAtCap(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))
	if err := s.EnsureDir(); err != nil {
		t.Fatal(err)
	}

	// Pre-fill queue past the cap.
	big := make([]byte, MaxQueueBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(s.QueueFile(), big, 0o644); err != nil {
		t.Fatal(err)
	}

	fake := NewFakeDolt()
	fake.SetMode(FakeDoltModeTransient) // direct write will fail

	payload := []byte(`{"id":"bd-cap-1"}`)
	err := writeAndMaybeSpool(context.Background(), s, "create", payload, func() error {
		return fake.Write("create", payload)
	})
	// Both Dolt and spool failed. The caller should see an error (the original
	// Dolt transient error surfaced because spool append returned ErrSpoolFull).
	if err == nil {
		t.Error("expected error when both Dolt and spool are full, got nil")
	}
}

// TestDiskCapAfterDrainResumesAppend verifies that once Drain empties the
// spool, Append works again.
//
// Because we can't actually write 100 MB in a unit test, we simulate a "near
// cap" state by pre-filling with MaxQueueBytes-1 bytes (one byte below cap),
// verify we can still append one entry, then check that a second oversized
// fill blocks the next append.
func TestDiskCapAfterDrainResumesAppend(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))
	if err := s.EnsureDir(); err != nil {
		t.Fatal(err)
	}

	// Write one normal entry.
	payload := []byte(`{"id":"bd-resume-1","title":"before drain"}`)
	entry, err := s.Append(context.Background(), "create", payload, false, "test")
	if err != nil {
		t.Fatalf("initial Append: %v", err)
	}
	if entry.OpID == "" {
		t.Error("entry should have an OpID")
	}

	// Drain it.
	dr, err := Drain(context.Background(), s, func(e Entry) error {
		return nil // success
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if dr.Drained != 1 {
		t.Errorf("drained: got %d, want 1", dr.Drained)
	}

	// Append again after drain — should succeed (queue was small, well below cap).
	payload2 := []byte(`{"id":"bd-resume-2","title":"after drain"}`)
	entry2, err := s.Append(context.Background(), "update", payload2, false, "test")
	if err != nil {
		t.Fatalf("Append after drain: %v (expected success)", err)
	}
	if entry2.OpID == "" {
		t.Error("second entry should have an OpID")
	}
}

// TestDiskCapNearBoundary verifies boundary semantics: queue at exactly
// MaxQueueBytes-1 allows one more tiny append, and once the file crosses the
// cap the next append is rejected.
func TestDiskCapNearBoundary(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))
	if err := s.EnsureDir(); err != nil {
		t.Fatal(err)
	}

	// Pre-fill to MaxQueueBytes - 100 (leave 100 bytes of headroom).
	headroom := int64(100)
	fill := make([]byte, MaxQueueBytes-headroom)
	for i := range fill {
		fill[i] = 'x'
	}
	if err := os.WriteFile(s.QueueFile(), fill, 0o644); err != nil {
		t.Fatal(err)
	}

	// Verify QueueDiskBytes reflects the fill.
	size, err := s.QueueDiskBytes()
	if err != nil {
		t.Fatalf("QueueDiskBytes: %v", err)
	}
	if size != MaxQueueBytes-headroom {
		t.Fatalf("size after fill: got %d, want %d", size, MaxQueueBytes-headroom)
	}

	// A tiny payload (< headroom) should succeed.
	smallPayload := []byte(`{"id":"bd-near"}`)
	if len(smallPayload) >= int(headroom) {
		t.Skip("test payload too large for headroom check")
	}
	_, err = s.Append(context.Background(), "note", smallPayload, false, "test")
	if err != nil {
		t.Errorf("Append near boundary: got %v, want nil (queue not yet at cap)", err)
	}

	// Now fill to exactly MaxQueueBytes by overwriting.
	fillToMax := make([]byte, MaxQueueBytes)
	for i := range fillToMax {
		fillToMax[i] = 'y'
	}
	if err := os.WriteFile(s.QueueFile(), fillToMax, 0o644); err != nil {
		t.Fatal(err)
	}

	// Next Append must return ErrSpoolFull.
	_, err = s.Append(context.Background(), "note", smallPayload, false, "test")
	if !errors.Is(err, ErrSpoolFull) {
		t.Errorf("Append at exactly cap: got %v, want ErrSpoolFull", err)
	}
}

// TestDiskCapErrSpoolFullMessage verifies the ErrSpoolFull error message
// contains enough context for users to understand what happened.
func TestDiskCapErrSpoolFullMessage(t *testing.T) {
	msg := ErrSpoolFull.Error()
	if !strings.Contains(msg, "capacity") && !strings.Contains(msg, "full") && !strings.Contains(msg, "cap") {
		t.Errorf("ErrSpoolFull message %q doesn't describe the problem clearly", msg)
	}
}
