//go:build cgo

// This test drives a child "bd init", which opens an EMBEDDED Dolt store and so
// needs a CGO build. It used to live in init_noninteractive_test.go, which is
// NOT cgo-gated because its other two tests are pure-Go -- so under the
// CGO_ENABLED=0 pure-Go suite this one failed with "embedded Dolt requires a
// CGO build", an environment error rather than a product defect (bda-a2l).
//
// A file-level tag on the original file would have excluded those two passing
// pure-Go tests as well, so the cgo-requiring test moves here instead. Matches
// the ~229 other cgo-gated test files in this package.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInitNonInteractiveAutoExportDefaultOffAndOptIn(t *testing.T) {
	if testing.Short() {
		t.Skip("builds+spawns the bd binary repeatedly; skipped in -short (bda-9l1)")
	}
	bd := buildBDForInitTests(t)
	dir := t.TempDir()

	runBDForAutoExportInitTest(t, bd, dir, "init", "--prefix", "test", "--quiet", "--non-interactive", "--skip-hooks", "--skip-agents")

	if got := strings.TrimSpace(runBDStdoutForAutoExportInitTest(t, bd, dir, "config", "get", "export.auto")); got != "false" {
		t.Fatalf("export.auto default = %q, want false", got)
	}
	if got := strings.TrimSpace(runBDStdoutForAutoExportInitTest(t, bd, dir, "config", "get", "export.git-add")); got != "false" {
		t.Fatalf("export.git-add default = %q, want false", got)
	}

	runBDForAutoExportInitTest(t, bd, dir, "create", "default-off issue", "-p", "2")
	jsonlPath := filepath.Join(dir, ".beads", "issues.jsonl")
	if _, err := os.Stat(jsonlPath); !os.IsNotExist(err) {
		t.Fatalf("default-off create wrote %s; stat err=%v", jsonlPath, err)
	}

	runBDForAutoExportInitTest(t, bd, dir, "config", "set", "export.interval", "1ms")
	runBDForAutoExportInitTest(t, bd, dir, "config", "set", "export.auto", "true")
	if got := strings.TrimSpace(runBDStdoutForAutoExportInitTest(t, bd, dir, "config", "get", "export.auto")); got != "true" {
		t.Fatalf("explicit export.auto = %q, want true", got)
	}
	time.Sleep(10 * time.Millisecond)
	runBDForAutoExportInitTest(t, bd, dir, "create", "explicit export issue", "-p", "2")
	data, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatalf("explicit export.auto did not write %s: %v", jsonlPath, err)
	}
	if !strings.Contains(string(data), "explicit export issue") {
		t.Fatalf("JSONL export did not contain created issue:\n%s", data)
	}
}

func runBDStdoutForAutoExportInitTest(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bd, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "BD_NON_INTERACTIVE=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bd %v failed: %v", args, err)
	}
	return string(out)
}

func runBDForAutoExportInitTest(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bd, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "BD_NON_INTERACTIVE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bd %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}
