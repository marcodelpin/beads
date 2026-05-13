package spool

import (
	"encoding/json"
	"testing"
)

func TestEntryJSONRoundtrip(t *testing.T) {
	e := Entry{
		OpID:          "abcdef0123456789abcdef0123456789",
		TS:            "2026-05-13T10:00:00Z",
		Op:            "create",
		Payload:       json.RawMessage(`{"title":"test","type":"task"}`),
		Attempts:      0,
		SchemaVersion: 1,
		ContentHash:   "deadbeef",
		Origin:        "bd-cli",
	}

	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var e2 Entry
	if err := json.Unmarshal(data, &e2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if e2.OpID != e.OpID {
		t.Errorf("OpID: got %q, want %q", e2.OpID, e.OpID)
	}
	if e2.SchemaVersion != 1 {
		t.Errorf("SchemaVersion: got %d, want 1", e2.SchemaVersion)
	}
	if e2.Op != e.Op {
		t.Errorf("Op: got %q, want %q", e2.Op, e.Op)
	}
	if string(e2.Payload) != string(e.Payload) {
		t.Errorf("Payload: got %s, want %s", e2.Payload, e.Payload)
	}
}

func TestEntrySchemaVersionExplicit(t *testing.T) {
	// v:1 must be set explicitly, not defaulted by JSON zero-value.
	data := []byte(`{"op_id":"abc","ts":"2026-05-13T10:00:00Z","op":"update","payload":{},"v":1}`)
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("unmarshal v:1: %v", err)
	}
	if e.SchemaVersion != 1 {
		t.Errorf("SchemaVersion: got %d, want 1", e.SchemaVersion)
	}
}

func TestEntrySchemaVersionForwardTolerant(t *testing.T) {
	// A future v:2 entry should decode without error (forward compat).
	// The drainer will refuse it, but the JSON layer must not break.
	data := []byte(`{"op_id":"abc","ts":"2026-05-13T10:00:00Z","op":"update","payload":{},"v":2}`)
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("unmarshal v:2: %v", err)
	}
	if e.SchemaVersion != 2 {
		t.Errorf("SchemaVersion: got %d, want 2", e.SchemaVersion)
	}
}

func TestValidateEntryRejectsUnknownOp(t *testing.T) {
	e := Entry{
		OpID:          "abc",
		SchemaVersion: 1,
		Op:            "delete",
		Payload:       json.RawMessage(`{}`),
	}
	if err := ValidateEntry(e); err == nil {
		t.Error("expected error for unknown op 'delete'")
	}
}

func TestValidateEntryRejectsMissingOpID(t *testing.T) {
	e := Entry{
		SchemaVersion: 1,
		Op:            "create",
		Payload:       json.RawMessage(`{}`),
	}
	if err := ValidateEntry(e); err == nil {
		t.Error("expected error for missing op_id")
	}
}

func TestValidateEntryAcceptsValid(t *testing.T) {
	e := Entry{
		OpID:          "abcdef0123456789abcdef0123456789",
		SchemaVersion: 1,
		Op:            "close",
		Payload:       json.RawMessage(`{"id":"bd-xyz"}`),
	}
	if err := ValidateEntry(e); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
