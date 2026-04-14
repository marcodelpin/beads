package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/utils"
)

func TestResolveWhereBeadsDir_FallsBackToFindBeadsDir(t *testing.T) {
	saveAndRestoreGlobals(t)
	ensureCleanGlobalState(t)

	originalCmdCtx := cmdCtx
	defer func() {
		cmdCtx = originalCmdCtx
	}()

	resetCommandContext()

	repoDir := t.TempDir()
	beadsDir := filepath.Join(repoDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue-prefix: fallback\n"), 0o644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(repoDir); err != nil {
		t.Fatalf("chdir(%q): %v", repoDir, err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(originalWD)
	})

	t.Setenv("BEADS_DIR", "")
	t.Setenv("BEADS_DB", "")
	t.Setenv("BD_DB", "")
	setDBPath("")

	dbFlag := rootCmd.PersistentFlags().Lookup("db")
	originalFlagValue := dbFlag.Value.String()
	originalFlagChanged := dbFlag.Changed
	if err := dbFlag.Value.Set(""); err != nil {
		t.Fatalf("reset db flag: %v", err)
	}
	dbFlag.Changed = false
	t.Cleanup(func() {
		_ = dbFlag.Value.Set(originalFlagValue)
		dbFlag.Changed = originalFlagChanged
	})

	if got := resolveWhereBeadsDir(); !utils.PathsEqual(got, beadsDir) {
		t.Fatalf("resolveWhereBeadsDir() = %q, want %q", got, beadsDir)
	}
}

func TestResolveWhereBeadsDir_ReturnsEmptyWithoutWorkspace(t *testing.T) {
	saveAndRestoreGlobals(t)
	ensureCleanGlobalState(t)

	originalCmdCtx := cmdCtx
	defer func() {
		cmdCtx = originalCmdCtx
	}()

	resetCommandContext()

	workspace := t.TempDir()
	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatalf("chdir(%q): %v", workspace, err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(originalWD)
	})

	t.Setenv("BEADS_DIR", "")
	t.Setenv("BEADS_DB", "")
	t.Setenv("BD_DB", "")
	setDBPath("")

	dbFlag := rootCmd.PersistentFlags().Lookup("db")
	originalFlagValue := dbFlag.Value.String()
	originalFlagChanged := dbFlag.Changed
	if err := dbFlag.Value.Set(""); err != nil {
		t.Fatalf("reset db flag: %v", err)
	}
	dbFlag.Changed = false
	t.Cleanup(func() {
		_ = dbFlag.Value.Set(originalFlagValue)
		dbFlag.Changed = originalFlagChanged
	})

	if got := resolveWhereBeadsDir(); got != "" {
		t.Fatalf("resolveWhereBeadsDir() = %q, want empty", got)
	}
}

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

func TestShouldReadWherePrefixFromStore(t *testing.T) {
	t.Setenv("BEADS_DOLT_SERVER_MODE", "")
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "")

	t.Run("empty beads dir", func(t *testing.T) {
		if got := shouldReadWherePrefixFromStore(""); got {
			t.Fatal("shouldReadWherePrefixFromStore(\"\") = true, want false")
		}
	})

	t.Run("missing metadata", func(t *testing.T) {
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		if err := os.MkdirAll(beadsDir, 0o755); err != nil {
			t.Fatalf("mkdir beads dir: %v", err)
		}
		if got := shouldReadWherePrefixFromStore(beadsDir); !got {
			t.Fatal("shouldReadWherePrefixFromStore(missing metadata) = false, want true")
		}
	})

	t.Run("server mode metadata", func(t *testing.T) {
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		if err := os.MkdirAll(beadsDir, 0o755); err != nil {
			t.Fatalf("mkdir beads dir: %v", err)
		}
		cfg := &configfile.Config{
			Backend:  configfile.BackendDolt,
			DoltMode: configfile.DoltModeServer,
		}
		if err := cfg.Save(beadsDir); err != nil {
			t.Fatalf("save metadata: %v", err)
		}
		if got := shouldReadWherePrefixFromStore(beadsDir); got {
			t.Fatal("shouldReadWherePrefixFromStore(server mode) = true, want false")
		}
	})

	t.Run("embedded mode metadata", func(t *testing.T) {
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		if err := os.MkdirAll(beadsDir, 0o755); err != nil {
			t.Fatalf("mkdir beads dir: %v", err)
		}
		cfg := &configfile.Config{
			Backend:  configfile.BackendDolt,
			DoltMode: configfile.DoltModeEmbedded,
		}
		if err := cfg.Save(beadsDir); err != nil {
			t.Fatalf("save metadata: %v", err)
		}
		if got := shouldReadWherePrefixFromStore(beadsDir); !got {
			t.Fatal("shouldReadWherePrefixFromStore(embedded mode) = false, want true")
		}
	})
}

func TestWhereCommand_UsesConfigPrefixFromSelectedDB(t *testing.T) {
	saveAndRestoreGlobals(t)
	ensureCleanGlobalState(t)
	initConfigForTest(t)

	originalCmdCtx := cmdCtx
	originalJSONOutput := jsonOutput
	originalRootCtx := rootCtx
	defer func() {
		cmdCtx = originalCmdCtx
		jsonOutput = originalJSONOutput
		rootCtx = originalRootCtx
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
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue-prefix: yamlprefix\n"), 0o644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	t.Setenv("BEADS_DIR", "")
	t.Setenv("BEADS_DB", dbDir)
	t.Setenv("BD_DB", "")

	dbFlag := rootCmd.PersistentFlags().Lookup("db")
	originalFlagValue := dbFlag.Value.String()
	originalFlagChanged := dbFlag.Changed
	if err := dbFlag.Value.Set(""); err != nil {
		t.Fatalf("reset db flag: %v", err)
	}
	dbFlag.Changed = false
	t.Cleanup(func() {
		_ = dbFlag.Value.Set(originalFlagValue)
		dbFlag.Changed = originalFlagChanged
	})

	jsonOutput = true
	rootCtx = context.Background()

	output := captureStdout(t, func() error {
		whereCmd.Run(whereCmd, nil)
		return nil
	})

	var result WhereResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", output, err)
	}

	if !utils.PathsEqual(result.Path, beadsDir) {
		t.Fatalf("Path = %q, want %q", result.Path, beadsDir)
	}
	if !utils.PathsEqual(result.DatabasePath, dbDir) {
		t.Fatalf("DatabasePath = %q, want %q", result.DatabasePath, dbDir)
	}
	if result.Prefix != "yamlprefix" {
		t.Fatalf("Prefix = %q, want %q", result.Prefix, "yamlprefix")
	}
}
