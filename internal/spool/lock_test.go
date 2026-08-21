package spool

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

func TestLockBasic(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".drain.lock")

	lk, err := OpenLock(lockPath)
	if err != nil {
		t.Fatalf("OpenLock: %v", err)
	}

	if err := lk.Lock(); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	if err := lk.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
}

func TestTryLockConflict(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".drain.lock")

	lk1, err := OpenLock(lockPath)
	if err != nil {
		t.Fatalf("OpenLock 1: %v", err)
	}
	defer lk1.Unlock()

	if err := lk1.Lock(); err != nil {
		t.Fatalf("Lock 1: %v", err)
	}

	lk2, err := OpenLock(lockPath)
	if err != nil {
		t.Fatalf("OpenLock 2: %v", err)
	}
	defer lk2.Unlock()

	if err := lk2.TryLock(); err != ErrLockHeld {
		t.Fatalf("TryLock 2: got %v, want ErrLockHeld", err)
	}

	// Release first lock; second try should succeed.
	if err := lk1.Unlock(); err != nil {
		t.Fatalf("Unlock 1: %v", err)
	}

	if err := lk2.TryLock(); err != nil {
		t.Fatalf("TryLock 2 after unlock: %v", err)
	}
}

func TestLockUnblocksWaiter(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".drain.lock")

	lk1, err := OpenLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lk1.Unlock()

	if err := lk1.Lock(); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	acquired := make(chan struct{})
	go func() {
		defer wg.Done()
		lk2, err := OpenLock(lockPath)
		if err != nil {
			t.Errorf("OpenLock 2: %v", err)
			return
		}
		defer lk2.Unlock()
		close(acquired) // signal we're about to block on Lock
		if err := lk2.Lock(); err != nil {
			t.Errorf("Lock 2: %v", err)
			return
		}
	}()

	<-acquired // waiter goroutine is ready
	// Release; the goroutine should unblock.
	if err := lk1.Unlock(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}

// TestLockStressConcurrentAppend verifies that 100 goroutines appending
// through the Spool with lock serialization produce no torn writes.
func TestLockStressConcurrentAppend(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))
	lockPath := filepath.Join(dir, ".drain.lock")

	const goroutines = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	// Each goroutine acquires the lock, appends one entry, releases.
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()

			lk, err := OpenLock(lockPath)
			if err != nil {
				t.Errorf("OpenLock goroutine %d: %v", id, err)
				return
			}
			defer lk.Unlock()

			if err := lk.Lock(); err != nil {
				t.Errorf("Lock goroutine %d: %v", id, err)
				return
			}

			payload := fmt.Sprintf(`{"id":"bd-%d","status":"in_progress"}`, id)
			_, err = s.Append(context.Background(), "update", []byte(payload), false, "test")
			if err != nil {
				t.Errorf("Append goroutine %d: %v", id, err)
			}
		}(i)
	}

	wg.Wait()

	// Verify: exactly goroutines entries, all parseable, no torn lines.
	entries, err := readJSONLEntries(s.QueueFile())
	if err != nil {
		t.Fatalf("readJSONLEntries: %v", err)
	}
	if len(entries) != goroutines {
		t.Fatalf("got %d entries, want %d", len(entries), goroutines)
	}

	// Extract all IDs and verify uniqueness (no torn writes).
	ids := make([]string, goroutines)
	for i, e := range entries {
		var payload struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("unmarshal payload %d: %v", i, err)
		}
		ids[i] = payload.ID
	}
	sort.Strings(ids)
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			t.Fatalf("duplicate ID at index %d: %s (torn write or lost entry)", i, ids[i])
		}
	}
}
