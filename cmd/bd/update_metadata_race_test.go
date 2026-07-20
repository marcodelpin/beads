package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// metadataRaceEnv returns a subprocess environment pinned to a throwaway HOME
// with daemon/auto-export/metrics machinery disabled, so each bd invocation is
// a plain short-lived process against the workspace's SQLite store — the exact
// deployment shape in which the lost-update defect was proven.
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
func TestUpdateSetMetadata_ConcurrentProcesses_NoLostKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns many bd subprocesses; skipped in -short")
	}
	// The SQLite backend was removed (#4881, 2026-07-18): "bd init --backend
	// sqlite" now fails loud with "storage backend \"sqlite\" is no longer
	// supported". This test's interleaving forcer also depends on SQLite
	// specifics that have no direct Dolt equivalent: it holds a raw
	// sql.Open("sqlite", ...) write lock on the local .beads/beads.db file
	// (MaxOpenConns(1) + BEGIN IMMEDIATE) to stall both writer subprocesses
	// until they release together. Dolt is a server-mode MVCC store
	// (no local SQLite file to lock), and this build has no live Dolt server
	// to design and verify a server-side interleaving forcer against
	// (internal/storage/dolt is cgo-only; see test_helpers_pure_test.go).
	// Skip until a real Dolt-based interleaving forcer is designed and
	// verified against a live Dolt test server (bda-0kl).
	t.Skip("SQLite backend removed (#4881); needs a redesigned Dolt-server interleaving forcer + a live server to verify it, see bda-0kl")
	bd := buildBDForTest(t)
	dir := t.TempDir()
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

	run("init", "--prefix", "mrace", "--backend", "sqlite", "--non-interactive")
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

	// The natural read→write window inside one bd process is ~1ms, so two
	// processes launched together rarely interleave on their own. To align
	// them, the test holds the SQLite write lock while both writers start:
	// both block in their busy-timeout loop, then race read-merge-write when
	// the lock releases — the audit's deterministic reproduction shape.
	dbPath := filepath.Join(dir, ".beads", "beads.db")
	lockDB, err := sql.Open("sqlite",
		"file:"+dbPath+"?_txlock=immediate&_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatalf("open lock handle: %v", err)
	}
	defer func() { _ = lockDB.Close() }()
	lockDB.SetMaxOpenConns(1)

	// Each round launches several bd processes under the held lock, writing
	// distinct keys of the same issue. All must exit 0 AND every key must
	// survive.
	writers := []string{"a", "b", "c", "d"}
	const rounds = 20
	for i := 0; i < rounds; i++ {
		lockTx, err := lockDB.Begin()
		if err != nil {
			t.Fatalf("round %d: begin lock tx: %v", i, err)
		}
		// A no-op write takes SQLite's RESERVED lock, stalling both writers.
		if _, err := lockTx.Exec("UPDATE issues SET title = title WHERE id = ''"); err != nil {
			t.Fatalf("round %d: acquire write lock: %v", i, err)
		}

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

		// Give both processes time to start and block on the held lock.
		time.Sleep(300 * time.Millisecond)
		if err := lockTx.Rollback(); err != nil {
			t.Fatalf("round %d: release write lock: %v", i, err)
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
