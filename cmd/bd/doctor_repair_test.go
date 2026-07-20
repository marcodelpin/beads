package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// buildBDForTest returns the path to a bd binary for subprocess tests.
// Delegates to buildBDForInitTests (test_helpers_pure_test.go), which shares
// ONE sync.Once-cached binary across every cmd/bd test helper using the same
// build config (gms_pure_go, -buildvcs=false, ambient env) instead of each
// helper rebuilding its own copy (bda-9l1).
func buildBDForTest(t *testing.T) string {
	t.Helper()
	return buildBDForInitTests(t)
}

func mkTmpDirInTmp(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		// Fallback for platforms without /tmp (e.g. Windows).
		dir, err = os.MkdirTemp("", prefix)
		if err != nil {
			t.Fatalf("failed to create temp dir: %v", err)
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func runBDSideDB(t *testing.T, exe, dir, dbPath string, args ...string) (string, error) {
	t.Helper()
	fullArgs := []string{"--db", dbPath}
	fullArgs = append(fullArgs, args...)

	cmd := exec.Command(exe, fullArgs...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"BEADS_DIR="+filepath.Join(dir, ".beads"),
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestDoctorRepair_CorruptDatabase_RebuildFromJSONL(t *testing.T) {
	// SQLite file corruption repair test. Dolt backend uses server connections,
	// not .db files, so corruption/repair scenarios are fundamentally different.
	t.Skip("SQLite file corruption repair; not applicable to Dolt backend (bd-o0u)")
}
