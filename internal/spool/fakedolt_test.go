package spool

import (
	"fmt"
	"sync"
)

// FakeDoltMode controls what FakeDolt returns for write calls.
type FakeDoltMode int

const (
	// FakeDoltModeOK — all writes succeed (return nil).
	FakeDoltModeOK FakeDoltMode = iota

	// FakeDoltModeTransient — writes return a transient error (connection
	// refused), causing the spool write-with-spool path to enqueue the entry.
	FakeDoltModeTransient

	// FakeDoltModePermanent — writes return a permanent error (UNIQUE
	// constraint), causing the caller to surface the error directly.
	FakeDoltModePermanent
)

// FakeDolt is an in-process test double for the Dolt/storage backend. It
// records every write attempt and returns errors according to its current mode.
// Thread-safe (all fields protected by mu).
type FakeDolt struct {
	mu      sync.Mutex
	mode    FakeDoltMode
	calls   []FakeDoltCall
	latency int // reserved for future latency injection
}

// FakeDoltCall records a single write attempt.
type FakeDoltCall struct {
	Op      string // "create" | "update" | "note" | "close"
	Payload []byte // raw JSON bytes passed to the fake
}

// NewFakeDolt returns a FakeDolt in MODE_OK.
func NewFakeDolt() *FakeDolt {
	return &FakeDolt{mode: FakeDoltModeOK}
}

// SetMode changes the fake's response mode. Safe to call concurrently.
func (f *FakeDolt) SetMode(m FakeDoltMode) {
	f.mu.Lock()
	f.mode = m
	f.mu.Unlock()
}

// Calls returns a snapshot of all recorded calls (oldest first).
func (f *FakeDolt) Calls() []FakeDoltCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeDoltCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// CallCount returns the number of write attempts recorded so far.
func (f *FakeDolt) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// ResetCalls clears the recorded call log.
func (f *FakeDolt) ResetCalls() {
	f.mu.Lock()
	f.calls = f.calls[:0]
	f.mu.Unlock()
}

// Write is the generic write entry point. It records the call and returns an
// error according to the current mode:
//   - FakeDoltModeOK → nil
//   - FakeDoltModeTransient → "connection refused" (classified as transient by IsTransientErr)
//   - FakeDoltModePermanent → "UNIQUE constraint failed: issues.id" (classified as permanent)
func (f *FakeDolt) Write(op string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, FakeDoltCall{Op: op, Payload: payload})
	switch f.mode {
	case FakeDoltModeOK:
		return nil
	case FakeDoltModeTransient:
		return fmt.Errorf("dial tcp 127.0.0.1:3306: connection refused")
	case FakeDoltModePermanent:
		return fmt.Errorf("UNIQUE constraint failed: issues.id")
	default:
		return fmt.Errorf("fakedolt: unknown mode %d", f.mode)
	}
}

// AsDispatchFunc returns a spool.DispatchFunc that delegates to Write(e.Op, e.Payload).
// Use this with Drain / MaybeDrain in tests.
func (f *FakeDolt) AsDispatchFunc() DispatchFunc {
	return func(e Entry) error {
		return f.Write(e.Op, e.Payload)
	}
}
