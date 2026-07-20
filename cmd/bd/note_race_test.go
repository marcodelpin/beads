package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestNote_ConcurrentProcesses_NoLostNotes is the end-to-end regression test
// for the notes arm of the concurrent lost-update defect: real bd processes
// running `bd note` (and `bd update --append-notes`) concurrently on the SAME
// issue, all exiting 0, must all have their lines in the final notes.
//
// Before the fix, cmd/bd/note.go concatenated the new line onto a snapshot of
// the issue read in an earlier transaction and wrote the whole notes column
// back, so whichever process committed second silently erased the other's
// note. The fix passes the append operation (issueops.OpAppendNotes) through
// to the storage layer, which re-reads and appends inside the single mutation
// transaction.
func TestNote_ConcurrentProcesses_NoLostNotes(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns many bd subprocesses; skipped in -short")
	}
	// The SQLite backend was removed (#4881, 2026-07-18): "bd init --backend
	// sqlite" now fails loud with "storage backend \"sqlite\" is no longer
	// supported". This test's interleaving forcer also depends on SQLite
	// specifics that have no direct Dolt equivalent: it holds a raw
	// sql.Open("sqlite", ...) write lock on the local .beads/beads.db file
	// (MaxOpenConns(1) + BEGIN IMMEDIATE) to stall every writer subprocess
	// until they all release together. Dolt is a server-mode MVCC store
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

	run("init", "--prefix", "nrace", "--backend", "sqlite", "--non-interactive")
	createOut := run("create", "note race target", "-p", "2", "--json")
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

	// Same interleaving forcer as the metadata race test: hold the SQLite
	// write lock while all writers start, so they race read-merge-write the
	// moment it releases.
	dbPath := filepath.Join(dir, ".beads", "beads.db")
	lockDB, err := sql.Open("sqlite",
		"file:"+dbPath+"?_txlock=immediate&_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatalf("open lock handle: %v", err)
	}
	defer func() { _ = lockDB.Close() }()
	lockDB.SetMaxOpenConns(1)

	// Ten `bd note` writers plus one `bd update --append-notes` writer per
	// round: the note command and the update flag share the same column and
	// must not erase each other. The high per-round concurrency is what makes
	// the pre-fix loss reliable — each writer used to read the notes in one
	// transaction and write the pre-merged column in a later one, so any
	// read-read-write-write interleave among the pack silently dropped a line.
	notePrefixes := []string{"a", "b", "c", "d", "e", "f", "g", "h", "j", "k"}
	const rounds = 15
	for i := 0; i < rounds; i++ {
		lockTx, err := lockDB.Begin()
		if err != nil {
			t.Fatalf("round %d: begin lock tx: %v", i, err)
		}
		if _, err := lockTx.Exec("UPDATE issues SET title = title WHERE id = ''"); err != nil {
			t.Fatalf("round %d: acquire write lock: %v", i, err)
		}

		argSets := [][]string{
			{"update", created.ID, "--append-notes", fmt.Sprintf("append-x%d", i)},
		}
		for _, p := range notePrefixes {
			argSets = append(argSets, []string{"note", created.ID, fmt.Sprintf("note-%s%d", p, i)})
		}
		var cmds []*exec.Cmd
		var bufs []*strings.Builder
		for _, args := range argSets {
			cmd := exec.Command(bd, args...)
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

		time.Sleep(250 * time.Millisecond)
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
		Notes string `json:"notes"`
	}
	if err := json.Unmarshal([]byte(showOut[arrStart:]), &details); err != nil {
		t.Fatalf("parse show output: %v\n%s", err, showOut)
	}
	if len(details) == 0 {
		t.Fatalf("show returned no issues: %s", showOut)
	}
	gotLines := strings.Split(details[0].Notes, "\n")

	var missing []string
	for i := 0; i < rounds; i++ {
		want := []string{fmt.Sprintf("append-x%d", i)}
		for _, p := range notePrefixes {
			want = append(want, fmt.Sprintf("note-%s%d", p, i))
		}
		for _, line := range want {
			if !slices.Contains(gotLines, line) {
				missing = append(missing, line)
			}
		}
	}
	if len(missing) > 0 {
		t.Fatalf("silent lost update: %d of %d exit-0 note writes missing from final notes: %v\nnotes:\n%s",
			len(missing), rounds*(len(notePrefixes)+1), missing, details[0].Notes)
	}
}
