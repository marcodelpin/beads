package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageNotDirAncestor creates a regular file named "not-dir" in a fresh temp dir
// and returns both that file and a path that traverses it.
//
// A path traversing a regular file is the case that reports ENOTDIR on POSIX but
// ERROR_PATH_NOT_FOUND on Windows, which Go maps to fs.ErrNotExist. Any guard
// that treats a not-exists as benign therefore fails OPEN on Windows (bda-8by).
func stageNotDirAncestor(t *testing.T) (notDir, through string) {
	t.Helper()
	dir := t.TempDir()
	notDir = filepath.Join(dir, "not-dir")
	if err := os.WriteFile(notDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("stage sentinel file: %v", err)
	}
	return notDir, filepath.Join(notDir, "child", "target.md")
}

func TestAncestorNotDirErrDetectsRegularFileAncestor(t *testing.T) {
	notDir, through := stageNotDirAncestor(t)

	err := ancestorNotDirErr(through)
	if err == nil {
		t.Fatalf("expected an error for a path traversing the regular file %s", notDir)
	}
	if !strings.Contains(err.Error(), notDir) {
		t.Errorf("expected the error to name the offending ancestor %s, got: %v", notDir, err)
	}
}

func TestAncestorNotDirErrAllowsGenuinelyAbsentPath(t *testing.T) {
	dir := t.TempDir()

	// Nothing is staged: every EXISTING ancestor is a directory and the leaf is
	// simply absent, which is the benign case callers must still fall through on.
	if err := ancestorNotDirErr(filepath.Join(dir, "missing", "target.md")); err != nil {
		t.Errorf("expected nil for a genuinely absent path, got: %v", err)
	}
}

// TestEnsureDirRejectsRegularFileAncestor pins the upstream protection that the
// codex config/hooks read guards depend on.
//
// Those guards treat a not-exists read as benign, which would be the same
// Windows fail-open as bda-8by if reached with an unusable parent path. It is
// safe there only because ensureDir runs FIRST and rejects such a path. This
// test exists so that ordering cannot be removed silently: drop the ensureDir
// call and the guards below it become fail-open again.
func TestEnsureDirRejectsRegularFileAncestor(t *testing.T) {
	notDir, through := stageNotDirAncestor(t)

	if err := EnsureDir(filepath.Dir(through), 0o755); err == nil {
		t.Fatalf("expected EnsureDir to reject a path under the regular file %s", notDir)
	}
}
