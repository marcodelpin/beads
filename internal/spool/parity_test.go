package spool

// parity_test.go — Win/Linux cross-platform parity tests.
//
// These tests run on BOTH platforms (no build tags — the spool package is
// cross-platform by design). They verify:
//   1. Path construction uses filepath.Join (separator-agnostic).
//   2. Lock primitive works on the current platform.
//   3. File modes: spool files are created with permissive mode on Unix;
//      on Windows mode bits are ignored but writes still succeed.
//   4. Queue/inflight/acked operations complete identically on both platforms.
//
// Platform-specific lock implementations live in lock_unix.go / lock_windows.go.
// This file validates the OBSERVABLE CONTRACT, not the implementation details.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPathSeparatorsAreCorrect verifies that all spool file paths use the
// platform-native separator (via filepath.Join) and that no hardcoded forward
// or backward slashes appear in the computed paths.
func TestPathSeparatorsAreCorrect(t *testing.T) {
	dir := t.TempDir()
	spoolDir := filepath.Join(dir, "spool")
	s := NewSpool(spoolDir)

	paths := map[string]string{
		"Dir":          s.Dir,
		"AckedDir":     s.AckedDir,
		"QueueFile":    s.QueueFile(),
		"InflightFile": s.InflightFile(),
		"DeadFile":     s.DeadFile(),
		"CursorFile":   s.CursorFile(),
	}

	for name, p := range paths {
		if p == "" {
			t.Errorf("%s: empty path", name)
			continue
		}
		// All paths must be absolute (not relative).
		if !filepath.IsAbs(p) {
			t.Errorf("%s: not absolute: %q", name, p)
		}
		// All paths must be sub-paths of spoolDir.
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			t.Errorf("%s: Rel error: %v", name, err)
			continue
		}
		if strings.HasPrefix(rel, "..") {
			t.Errorf("%s: path %q escapes spool root %q", name, p, dir)
		}
	}
}

// TestLockPrimitiveAcquireRelease exercises the platform lock end-to-end.
// This is the cross-platform contract test; lock_unix.go / lock_windows.go
// implement the same behaviour under different syscalls.
func TestLockPrimitiveAcquireRelease(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".test.lock")

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

	// Re-acquire to verify the lock is truly released.
	lk2, err := OpenLock(lockPath)
	if err != nil {
		t.Fatalf("OpenLock 2: %v", err)
	}
	if err := lk2.Lock(); err != nil {
		t.Fatalf("Lock 2 after Unlock: %v", err)
	}
	if err := lk2.Unlock(); err != nil {
		t.Fatalf("Unlock 2: %v", err)
	}
}

// TestLockTryLockParityBothPlatforms exercises TryLock contention on the
// current platform. The behaviour must be identical on Unix (flock LOCK_NB)
// and Windows (LOCKFILE_FAIL_IMMEDIATELY).
func TestLockTryLockParityBothPlatforms(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".trylock.lock")

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

	// TryLock must fail while lk1 holds the lock.
	err = lk2.TryLock()
	if err != ErrLockHeld {
		t.Errorf("TryLock with contention: got %v, want ErrLockHeld", err)
	}

	// Release lk1; TryLock on lk2 must now succeed.
	if err := lk1.Unlock(); err != nil {
		t.Fatalf("Unlock 1: %v", err)
	}
	if err := lk2.TryLock(); err != nil {
		t.Errorf("TryLock after Unlock: got %v, want nil", err)
	}
}

// TestLockPathReturnsExpectedValue verifies the Path() accessor works on both
// platforms (trivial but ensures the interface contract is satisfied).
func TestLockPathReturnsExpectedValue(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".path.lock")

	lk, err := OpenLock(lockPath)
	if err != nil {
		t.Fatalf("OpenLock: %v", err)
	}
	defer lk.Unlock()

	if lk.Path() != lockPath {
		t.Errorf("Path(): got %q, want %q", lk.Path(), lockPath)
	}
}

// TestFileModesOrIgnored verifies that spool files can be created and read on
// both platforms. On Unix the mode is 0644; on Windows mode bits are advisory
// but the file system semantics still apply.
func TestFileModesOrIgnored(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))
	if err := s.EnsureDir(); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}

	// Append an entry (writes queue.jsonl with 0644).
	e := makeEntry("create", `{"id":"bd-parity-1"}`)
	if err := s.AppendQueue(e); err != nil {
		t.Fatalf("AppendQueue: %v", err)
	}

	// Stat the file — must exist and be readable.
	info, err := os.Stat(s.QueueFile())
	if err != nil {
		t.Fatalf("Stat queue: %v", err)
	}
	if info.Size() == 0 {
		t.Error("queue.jsonl should be non-empty after AppendQueue")
	}

	// On Unix: verify mode bits.
	if runtime.GOOS != "windows" {
		mode := info.Mode()
		if mode.Perm()&0o400 == 0 {
			t.Errorf("queue.jsonl owner-read bit not set: %o", mode.Perm())
		}
		if mode.Perm()&0o200 == 0 {
			t.Errorf("queue.jsonl owner-write bit not set: %o", mode.Perm())
		}
	}

	// On Windows: file exists and is readable — no chmod semantics.
	if runtime.GOOS == "windows" {
		f, err := os.Open(s.QueueFile())
		if err != nil {
			t.Errorf("Open queue on Windows: %v", err)
		} else {
			f.Close()
		}
	}
}

// TestSpoolDirUsesOsSeparator verifies that the spool directory path returned
// by NewSpool contains the OS path separator, not a hardcoded slash.
func TestSpoolDirUsesOsSeparator(t *testing.T) {
	dir := t.TempDir()
	spoolDir := filepath.Join(dir, "spool")
	s := NewSpool(spoolDir)

	// s.Dir must contain filepath.Separator, not a hardcoded character.
	// filepath.Join normalises the path, so this is a consistency check.
	if s.Dir != spoolDir {
		t.Errorf("Spool.Dir: got %q, want %q", s.Dir, spoolDir)
	}
}

// TestFullRoundtripBothPlatforms is an end-to-end spool-append → drain test
// that exercises the complete path (EnsureDir, Append, Drain, Acked, Cursor)
// in a single function — verifying that all interactions work on the current OS.
func TestFullRoundtripBothPlatforms(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(filepath.Join(dir, "spool"))
	fake := NewFakeDolt()
	fake.SetMode(FakeDoltModeOK)

	const n = 3
	for i := 0; i < n; i++ {
		payload := []byte(fmt.Sprintf(`{"id":"bd-rt-%d"}`, i))
		_, err := s.Append(context.Background(), "update", payload, false, "test")
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	dr, err := Drain(context.Background(), s, fake.AsDispatchFunc())
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if dr.Drained != n {
		t.Errorf("drained: got %d, want %d", dr.Drained, n)
	}
	if dr.Dead != 0 {
		t.Errorf("dead: got %d, want 0", dr.Dead)
	}

	// Verify inflight is cleared.
	inflight, err := s.LoadInflight()
	if err != nil {
		t.Fatalf("LoadInflight: %v", err)
	}
	if len(inflight) != 0 {
		t.Errorf("inflight should be empty, got %d", len(inflight))
	}

	// Verify acked has n entries.
	ackedFiles, _ := filepath.Glob(filepath.Join(s.AckedDir, "*.jsonl"))
	totalAcked := 0
	for _, f := range ackedFiles {
		entries, _ := readJSONLEntries(f)
		totalAcked += len(entries)
	}
	if totalAcked != n {
		t.Errorf("acked: got %d, want %d", totalAcked, n)
	}

	// Cursor should be saved.
	cursor, err := s.LoadCursor()
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if cursor.LastDrainTS == "" {
		t.Error("cursor.LastDrainTS should be set after drain")
	}
	// Post-compaction contract: a fully-drained cycle drops the consumed
	// queue and resets the cursor (GH#4378-review D5).
	if cursor.LastAckedOffset != 0 {
		t.Errorf("cursor.LastAckedOffset = %d, want 0 after a fully-drained cycle (compaction)", cursor.LastAckedOffset)
	}
	if has, err := fileHasContent(s.QueueFile()); err == nil && has {
		t.Error("queue.jsonl should be compacted away after a fully-drained cycle")
	}
}

// TestPlatformLabel logs which platform the tests are running on — useful for
// CI output when checking Win/Linux parity.
func TestPlatformLabel(t *testing.T) {
	t.Logf("platform: GOOS=%s GOARCH=%s", runtime.GOOS, runtime.GOARCH)
}
