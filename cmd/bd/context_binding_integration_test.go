//go:build cgo

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/types"
)

var (
	fullCGOBDOnce sync.Once
	fullCGOBDPath string
	fullCGOBDErr  error
)

// buildFullCGOBDBinary returns the path to a bd binary built WITHOUT the
// gms_pure_go tag (the default/full cgo build). This is a genuinely
// different build config from buildBDForInitTests (which always passes
// -tags gms_pure_go), so per bda-9l1 fix-direction (b) ("do NOT merge
// helpers whose configs genuinely differ") it gets its own cached builder
// instead of being folded into the shared one. It intentionally does NOT
// honor BEADS_TEST_BD_BINARY either: a prebuilt binary supplied via that
// env var is built with gms_pure_go and would silently substitute the
// wrong build config for this test.
func buildFullCGOBDBinary(t *testing.T) string {
	t.Helper()
	fullCGOBDOnce.Do(func() {
		tmpDir, err := testTempDir("bd-full-cgo-test-*")
		if err != nil {
			fullCGOBDErr = fmt.Errorf("failed to create temp dir: %w", err)
			return
		}
		binPath := filepath.Join(tmpDir, "bd-under-test")
		packageDir, err := os.Getwd()
		if err != nil {
			fullCGOBDErr = fmt.Errorf("getwd: %w", err)
			return
		}
		buildCmd := exec.Command("go", "build", "-buildvcs=false", "-o", binPath, ".")
		buildCmd.Dir = packageDir
		if out, err := buildCmd.CombinedOutput(); err != nil {
			fullCGOBDErr = fmt.Errorf("go build failed: %v\n%s", err, out)
			return
		}
		fullCGOBDPath = binPath
	})
	if fullCGOBDErr != nil {
		t.Fatalf("build full-cgo bd binary: %v", fullCGOBDErr)
	}
	return fullCGOBDPath
}

func filteredEnvForContextBinding(keys ...string) []string {
	strip := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		strip[key+"="] = struct{}{}
	}

	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, "BEADS_") || strings.HasPrefix(entry, "BD_") {
			continue
		}
		trim := false
		for prefix := range strip {
			if strings.HasPrefix(entry, prefix) {
				trim = true
				break
			}
		}
		if !trim {
			filtered = append(filtered, entry)
		}
	}
	return append(filtered, "BD_DISABLE_METRICS=1", "BD_DISABLE_EVENT_FLUSH=1")
}

func TestListExplicitDBPathRebindsTargetContext(t *testing.T) {
	if testDoltServerPort == 0 {
		t.Skip("Dolt test server not available, skipping")
	}

	tmpDir := t.TempDir()
	callerRepo := filepath.Join(tmpDir, "caller")
	callerBeadsDir := filepath.Join(callerRepo, ".beads")
	writeTestConfigYAML(t, callerBeadsDir, "dolt.auto-commit: invalid\nactor: caller-actor\n")
	if err := os.WriteFile(filepath.Join(callerBeadsDir, ".env"), []byte("BEADS_DOLT_SERVER_PORT=1\n"), 0o600); err != nil {
		t.Fatalf("write caller .env: %v", err)
	}

	targetRepo := filepath.Join(tmpDir, "target")
	targetBeadsDir := filepath.Join(targetRepo, ".beads")
	writeTestConfigYAML(t, targetBeadsDir, "dolt.auto-commit: off\nactor: target-actor\n")
	database := uniqueTestDBName(t)
	if err := (&configfile.Config{
		Backend:        configfile.BackendDolt,
		DoltMode:       configfile.DoltModeServer,
		DoltServerHost: "127.0.0.1",
		DoltServerPort: testDoltServerPort,
		DoltDatabase:   database,
	}).Save(targetBeadsDir); err != nil {
		t.Fatalf("save target metadata: %v", err)
	}

	ctx := context.Background()
	testStore, err := dolt.New(ctx, &dolt.Config{
		Path:            filepath.Join(targetBeadsDir, "dolt"),
		BeadsDir:        targetBeadsDir,
		ServerHost:      "127.0.0.1",
		ServerPort:      testDoltServerPort,
		Database:        database,
		CreateIfMissing: true,
	})
	if err != nil {
		t.Fatalf("create test store: %v", err)
	}
	defer func() {
		_ = testStore.Close()
		dropTestDatabase(database, testDoltServerPort)
	}()
	if err := testStore.SetConfig(ctx, "issue_prefix", "ctx"); err != nil {
		t.Fatalf("set issue_prefix: %v", err)
	}
	now := time.Now()
	nowIssue := &types.Issue{
		ID:          "ctx-1",
		Title:       "Context binding proof",
		Description: "Proves explicit --db commands use the target workspace config",
		Status:      types.StatusOpen,
		Priority:    1,
		IssueType:   types.TypeTask,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := testStore.CreateIssue(ctx, nowIssue, "test-user"); err != nil {
		t.Fatalf("create issue: %v", err)
	}

	binPath := buildFullCGOBDBinary(t)

	listCmd := exec.Command(binPath, "list", "--db", filepath.Join(targetBeadsDir, "dolt"), "--json")
	listCmd.Dir = callerRepo
	listCmd.Env = append(filteredEnvForContextBinding("BEADS_DIR", "BEADS_DB", "BD_DB", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_SERVER_DATABASE"),
		"HOME="+t.TempDir(),
		"XDG_CONFIG_HOME="+t.TempDir(),
		"BEADS_TEST_MODE=1",
		"BEADS_DIR="+callerBeadsDir,
		"BEADS_DB=",
	)
	output, err := listCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bd list failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "Context binding proof") {
		t.Fatalf("expected list output to include target issue\n%s", output)
	}

	if _, err := os.Stat(filepath.Join(callerBeadsDir, localVersionFile)); err == nil {
		t.Fatalf("caller workspace unexpectedly created %s", filepath.Join(callerBeadsDir, localVersionFile))
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat caller %s: %v", localVersionFile, err)
	}
	if _, err := os.Stat(filepath.Join(targetBeadsDir, localVersionFile)); err != nil {
		t.Fatalf("target workspace should create %s: %v", localVersionFile, err)
	}
}
