//go:build cgo

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/dolt"
)

func metadataRaceEnv(home string) []string {
	env := make([]string, 0, len(os.Environ())+5)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "BEADS_") {
			continue
		}
		env = append(env, e)
	}
	return append(env,
		"HOME="+home,
		"BEADS_DOLT_AUTO_START=0",
		"BEADS_NO_DAEMON=1",
		"BD_DISABLE_METRICS=1",
		"BD_DISABLE_EVENT_FLUSH=1",
	)
}

// raceWorkspaceOnDoltServer provisions a workspace directory whose
// .beads/metadata.json points at this package's Dolt test server, creates the
// backing database and seeds issue_prefix, then returns the directory. bd
// subprocesses started with cmd.Dir set to it resolve that workspace by
// walking up to .beads, exactly as a real invocation would.
//
// This replaces the `bd init --backend sqlite --non-interactive` the race
// tests used before #4881 removed the SQLite backend. The provisioning shape
// mirrors the existing cgo integration tests (see
// TestListExplicitDBPathRebindsTargetContext) rather than inventing a second
// convention.
func raceWorkspaceOnDoltServer(t *testing.T, issuePrefix string) string {
	t.Helper()
	if testDoltServerPort == 0 {
		t.Fatalf("raceWorkspaceOnDoltServer called without a Dolt test server")
	}
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("create .beads: %v", err)
	}

	database := uniqueTestDBName(t)
	if err := (&configfile.Config{
		Database:       "dolt",
		Backend:        configfile.BackendDolt,
		DoltMode:       configfile.DoltModeServer,
		DoltServerHost: "127.0.0.1",
		DoltServerPort: testDoltServerPort,
		DoltDatabase:   database,
	}).Save(beadsDir); err != nil {
		t.Fatalf("save workspace metadata: %v", err)
	}

	ctx := context.Background()
	store, err := dolt.New(ctx, &dolt.Config{
		Path:            filepath.Join(beadsDir, "dolt"),
		BeadsDir:        beadsDir,
		ServerHost:      "127.0.0.1",
		ServerPort:      testDoltServerPort,
		Database:        database,
		CreateIfMissing: true,
	})
	if err != nil {
		t.Fatalf("create race test database: %v", err)
	}
	if err := store.SetConfig(ctx, "issue_prefix", issuePrefix); err != nil {
		_ = store.Close()
		dropTestDatabase(database, testDoltServerPort)
		t.Fatalf("set issue_prefix: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close provisioning store: %v", err)
	}
	t.Cleanup(func() { dropTestDatabase(database, testDoltServerPort) })
	return dir
}

// TestUpdateSetMetadata_ConcurrentProcesses_NoLostKeys is the end-to-end
// regression test for the concurrent-metadata lost-update defect: real bd
// processes running `bd update --set-metadata` concurrently with DISTINCT keys
// on the SAME issue, all exiting 0, must all have their keys in the final
// metadata.
//
// Before the fix, cmd/bd/update.go merged --set-metadata into a snapshot of
// the issue read in an earlier transaction and wrote the whole metadata column
// back, so whichever process committed second silently erased the other's key
// (7 of 200 exit-0 writes lost in the audit hammer). The fix passes the edit
// operations through to the storage layer, which re-reads and merges inside
// the single mutation transaction.
//
// Backend note (bda-8gx): see the companion comment on
// TestNote_ConcurrentProcesses_NoLostNotes. The SQLite write-lock forcer is
// gone with the SQLite backend (#4881) and has no Dolt equivalent, so this
// asserts the invariant under raw parallelism and leans on volume instead of
// determinism. The counts below were tuned against a re-introduced defect
// until the test went RED; do not lower them without redoing that.
func TestUpdateSetMetadata_ConcurrentProcesses_NoLostKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns many bd subprocesses; skipped in -short")
	}
	if testDoltServerPort == 0 {
		t.Skip("Dolt test server not available, skipping")
	}
	bd := buildBDForTest(t)
	dir := raceWorkspaceOnDoltServer(t, "mrace")
	env := metadataRaceEnv(dir)

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bd, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bd %v failed: %v\n%s", args, err, out)
		}
		return string(out)
	}

	createOut := run("create", "metadata race target", "-p", "2", "--json")
	jsonStart := strings.Index(createOut, "{")
	if jsonStart < 0 {
		t.Fatalf("no JSON in create output: %s", createOut)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(createOut[jsonStart:]), &created); err != nil {
		t.Fatalf("parse create output: %v\n%s", err, createOut)
	}
	if created.ID == "" {
		t.Fatalf("create output has no id: %s", createOut)
	}

	// Each round launches several bd processes back-to-back, writing distinct
	// keys of the same issue. All must exit 0 AND every key must survive.
	writers := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	const rounds = 20
	for i := 0; i < rounds; i++ {
		var cmds []*exec.Cmd
		var bufs []*strings.Builder
		for _, prefix := range writers {
			cmd := exec.Command(bd, "update", created.ID,
				"--set-metadata", fmt.Sprintf("%s%d=1", prefix, i))
			cmd.Dir = dir
			cmd.Env = env
			buf := &strings.Builder{}
			cmd.Stdout = buf
			cmd.Stderr = buf
			cmds = append(cmds, cmd)
			bufs = append(bufs, buf)
		}
		for j, cmd := range cmds {
			if err := cmd.Start(); err != nil {
				t.Fatalf("round %d: start writer %d: %v", i, j, err)
			}
		}

		for j, cmd := range cmds {
			if err := cmd.Wait(); err != nil {
				t.Fatalf("round %d: writer %d exited nonzero: %v\n%s", i, j, err, bufs[j].String())
			}
		}
	}

	showOut := run("show", created.ID, "--json")
	arrStart := strings.Index(showOut, "[")
	if arrStart < 0 {
		t.Fatalf("no JSON array in show output: %s", showOut)
	}
	var details []struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(showOut[arrStart:]), &details); err != nil {
		t.Fatalf("parse show output: %v\n%s", err, showOut)
	}
	if len(details) == 0 {
		t.Fatalf("show returned no issues: %s", showOut)
	}
	got := map[string]any{}
	if len(details[0].Metadata) > 0 {
		if err := json.Unmarshal(details[0].Metadata, &got); err != nil {
			t.Fatalf("parse metadata %q: %v", details[0].Metadata, err)
		}
	}

	var missing []string
	for i := 0; i < rounds; i++ {
		for _, prefix := range writers {
			key := fmt.Sprintf("%s%d", prefix, i)
			if _, ok := got[key]; !ok {
				missing = append(missing, key)
			}
		}
	}
	if len(missing) > 0 {
		t.Fatalf("silent lost update: %d of %d exit-0 --set-metadata writes missing from final metadata: %v",
			len(missing), rounds*len(writers), missing)
	}
}
