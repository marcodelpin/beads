package spool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestAppendRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	payload := []byte(`{"title":"test issue","type":"task"}`)
	e, err := s.Append(context.Background(), "create", payload, false, "test")
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	if e.OpID == "" {
		t.Error("OpID should be filled")
	}
	if e.SchemaVersion != 1 {
		t.Errorf("SchemaVersion: got %d, want 1", e.SchemaVersion)
	}
	if e.Op != "create" {
		t.Errorf("Op: got %q, want %q", e.Op, "create")
	}
	if e.ContentHash == "" {
		t.Error("ContentHash should be filled")
	}
	if e.Origin != "test" {
		t.Errorf("Origin: got %q, want %q", e.Origin, "test")
	}

	// Read back from queue.
	entries, err := readJSONLEntries(s.QueueFile())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].OpID != e.OpID {
		t.Errorf("OpID mismatch: got %q, want %q", entries[0].OpID, e.OpID)
	}
}

func TestAppendRejectsUnknownOp(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	_, err := s.Append(context.Background(), "delete", []byte(`{}`), false, "test")
	if err == nil {
		t.Fatal("expected error for unknown op")
	}
}

func TestAppendDiskCapFull(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))
	if err := s.EnsureDir(); err != nil {
		t.Fatal(err)
	}

	// Fill queue past MaxQueueBytes.
	big := make([]byte, MaxQueueBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(s.QueueFile(), big, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := s.Append(context.Background(), "create", []byte(`{"title":"nope"}`), false, "test")
	if !errors.Is(err, ErrSpoolFull) {
		t.Errorf("expected ErrSpoolFull, got: %v", err)
	}
}

func TestIsTransientErr(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		transient bool
	}{
		{"nil", nil, false},
		{"context.DeadlineExceeded", context.DeadlineExceeded, true},
		{"context.Canceled", context.Canceled, true},
		{"HTTPStatusErr 500", &HTTPStatusErr{Status: 500, Body: "bad gateway"}, true},
		{"HTTPStatusErr 503", &HTTPStatusErr{Status: 503, Body: "unavailable"}, true},
		{"HTTPStatusErr 400", &HTTPStatusErr{Status: 400, Body: "bad request"}, false},
		{"HTTPStatusErr 404", &HTTPStatusErr{Status: 404, Body: "not found"}, false},
		{"i/o timeout", fmt.Errorf("dial tcp: i/o timeout"), true},
		{"connection refused", fmt.Errorf("dial tcp: connection refused"), true},
		{"EOF", fmt.Errorf("unexpected EOF"), true},
		{"duplicate entry", fmt.Errorf("Error 1062: Duplicate entry 'x' for key 'PRIMARY'"), false},
		{"foreign key", fmt.Errorf("cannot add or update a child row: a foreign key constraint fails"), false},
		{"UNIQUE constraint", fmt.Errorf("UNIQUE constraint failed: issues.id"), false},
		{"unknown error", fmt.Errorf("something weird happened"), true}, // conservative default
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsTransientErr(tt.err)
			if got != tt.transient {
				t.Errorf("IsTransientErr(%v) = %v, want %v", tt.err, got, tt.transient)
			}
		})
	}
}

func TestIsTransientErrNetTimeout(t *testing.T) {
	// Simulate a net.Error with Timeout()=true.
	err := &net.OpError{Err: &mockTimeoutErr{}}
	if !IsTransientErr(err) {
		t.Error("expected net timeout to be transient")
	}
}

type mockTimeoutErr struct{}

func (e *mockTimeoutErr) Error() string   { return "timeout" }
func (e *mockTimeoutErr) Timeout() bool   { return true }
func (e *mockTimeoutErr) Temporary() bool { return true }

func TestAppendMultipleEntries(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))

	for i := range 5 {
		payload, _ := json.Marshal(map[string]any{"title": fmt.Sprintf("issue-%d", i)})
		_, err := s.Append(context.Background(), "create", payload, false, "test")
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	count, err := s.QueueLineCount()
	if err != nil {
		t.Fatalf("QueueLineCount: %v", err)
	}
	if count != 5 {
		t.Errorf("count: got %d, want 5", count)
	}
}

func TestNewIDUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		id, err := newID()
		if err != nil {
			t.Fatalf("newID: %v", err)
		}
		if len(id) != 32 {
			t.Errorf("len(id) = %d, want 32", len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate id: %s", id)
		}
		seen[id] = true
	}
}
