package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/utils"
)

func TestResolveWhereBeadsDir_UsesInitializedDBPath(t *testing.T) {
	originalDBPath := dbPath
	originalCmdCtx := cmdCtx
	defer func() {
		dbPath = originalDBPath
		cmdCtx = originalCmdCtx
	}()

	resetCommandContext()

	repoDir := t.TempDir()
	beadsDir := filepath.Join(repoDir, ".beads")
	dbDir := filepath.Join(beadsDir, "dolt")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir db dir: %v", err)
	}

	cfg := &configfile.Config{
		Database: "dolt",
		Backend:  configfile.BackendDolt,
	}
	if err := cfg.Save(beadsDir); err != nil {
		t.Fatalf("save metadata: %v", err)
	}

	cwd := t.TempDir()
	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(originalWD)
	})

	dbPath = dbDir

	if got := resolveWhereBeadsDir(); !utils.PathsEqual(got, beadsDir) {
		t.Fatalf("resolveWhereBeadsDir() = %q, want %q", got, beadsDir)
	}
}

func TestResolveWhereDatabasePath_PrefersInitializedDBPath(t *testing.T) {
	originalDBPath := dbPath
	originalCmdCtx := cmdCtx
	defer func() {
		dbPath = originalDBPath
		cmdCtx = originalCmdCtx
	}()

	resetCommandContext()

	repoDir := t.TempDir()
	beadsDir := filepath.Join(repoDir, ".beads")
	dbDir := filepath.Join(beadsDir, "dolt")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir db dir: %v", err)
	}

	cfg := &configfile.Config{
		Database: "dolt",
		Backend:  configfile.BackendDolt,
	}
	if err := cfg.Save(beadsDir); err != nil {
		t.Fatalf("save metadata: %v", err)
	}

	dbPath = dbDir
	t.Setenv("BEADS_DIR", "")

	prepareSelectedNoDBContext(beadsDir)

	if got := resolveWhereDatabasePath(); !utils.PathsEqual(got, dbDir) {
		t.Fatalf("resolveWhereDatabasePath() = %q, want %q", got, dbDir)
	}
}

func TestIsSelectedNoDBCommand_Where(t *testing.T) {
	cmd := &cobra.Command{Use: "where"}

	if !isSelectedNoDBCommand(cmd) {
		t.Fatal("isSelectedNoDBCommand(where) = false, want true")
	}
}
