//go:build !windows

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitRepairsPermissiveBeadsDir is the init-path regression test for
// GH#3391: a pre-existing .beads/ directory with world-accessible bits
// (e.g. 0755 from a permissive umask) must be repaired to strip those
// bits during bd init.
//
// The test creates a real git repo with a pre-existing .beads/ at 0755,
// runs bd init, and asserts that world bits are stripped. The init may
// fail later (e.g. no Dolt server), but the permission fix happens early
// enough that the assertion is valid regardless of exit code.
func TestInitRepairsPermissiveBeadsDir(t *testing.T) {
	bdBin := buildBDForInitTests(t)

	repoDir := newGitRepo(t)

	beadsDir := filepath.Join(repoDir, ".beads")
	if err := os.Mkdir(beadsDir, 0755); err != nil {
		t.Fatalf("failed to create .beads: %v", err)
	}
	if err := os.Chmod(beadsDir, 0755); err != nil {
		t.Fatalf("failed to chmod .beads: %v", err)
	}

	// Verify starting permissions.
	info, err := os.Stat(beadsDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got&0007 == 0 {
		t.Fatalf("precondition failed: .beads should have world bits, got %04o", got)
	}

	cmd := exec.Command(bdBin, "init", "--prefix", "bd",
		"--non-interactive", "--skip-hooks", "--skip-agents")
	cmd.Dir = repoDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	// We don't check the exit code — init may fail later for reasons
	// unrelated to permissions (no Dolt, etc.). The permission fix runs
	// before database creation.
	_ = cmd.Run()

	// Assert: world-accessible bits must be stripped.
	info, err = os.Stat(beadsDir)
	if err != nil {
		t.Fatalf("Stat(.beads) after init: %v", err)
	}
	perm := info.Mode().Perm()
	if perm&0007 != 0 {
		t.Errorf(".beads permissions after init = %04o; world bits should have been stripped", perm)
	}

	// Assert: the fix was announced on stderr.
	if !strings.Contains(stderr.String(), "Stripped world-accessible bits") {
		t.Errorf("expected permission-fix message on stderr, got:\n%s", stderr.String())
	}
}

// TestInitPreservesSecureBeadsDir verifies that bd init does NOT touch a
// .beads/ directory that already has secure permissions (0700).
func TestInitPreservesSecureBeadsDir(t *testing.T) {
	bdBin := buildBDForInitTests(t)

	repoDir := newGitRepo(t)

	beadsDir := filepath.Join(repoDir, ".beads")
	if err := os.Mkdir(beadsDir, 0700); err != nil {
		t.Fatalf("failed to create .beads: %v", err)
	}

	cmd := exec.Command(bdBin, "init", "--prefix", "bd",
		"--non-interactive", "--skip-hooks", "--skip-agents")
	cmd.Dir = repoDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run()

	// Permissions should remain 0700.
	info, err := os.Stat(beadsDir)
	if err != nil {
		t.Fatalf("Stat(.beads) after init: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0700 {
		t.Errorf(".beads permissions after init = %04o, want 0700", perm)
	}

	// No fix message expected.
	if strings.Contains(stderr.String(), "Stripped world-accessible bits") {
		t.Errorf("unexpected permission-fix message for already-secure .beads/:\n%s", stderr.String())
	}
}
