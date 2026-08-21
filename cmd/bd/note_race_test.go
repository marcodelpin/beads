//go:build cgo

package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
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
//
// Backend note (bda-8gx): this test used to run on SQLite and forced a
// deterministic interleave by holding the local .beads/beads.db write lock
// while every writer started. The SQLite backend was removed (#4881) and Dolt
// offers no equivalent primitive -- it is a server-mode MVCC store, and
// nothing reachable over its wire protocol stalls writers the way SQLite's
// file lock did (measured, bda-8gx). The port therefore drops the forcer and
// asserts the invariant under RAW PARALLELISM: each round starts its writers
// back-to-back and lets them overlap on their own. That trades determinism for
// volume, so the counts below are not arbitrary -- they were tuned against a
// deliberately re-introduced defect until this test went RED, and the fixed
// tree was then re-run to satisfy the determinism gate. Re-tune them the same
// way if they ever stop reproducing; a green run of an untuned raw-parallelism
// test proves nothing.
func TestNote_ConcurrentProcesses_NoLostNotes(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns many bd subprocesses; skipped in -short")
	}
	if testDoltServerPort == 0 {
		t.Skip("Dolt test server not available, skipping")
	}
	bd := buildBDForTest(t)
	dir := raceWorkspaceOnDoltServer(t, "nrace")
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

	// Ten `bd note` writers plus one `bd update --append-notes` writer per
	// round: the note command and the update flag share the same column and
	// must not erase each other. The high per-round concurrency is what makes
	// the pre-fix loss reproducible without a forcer -- each writer used to
	// read the notes in one transaction and write the pre-merged column in a
	// later one, so any read-read-write-write interleave among the pack
	// silently dropped a line.
	notePrefixes := []string{"a", "b", "c", "d", "e", "f", "g", "h", "j", "k"}
	const rounds = 15
	for i := 0; i < rounds; i++ {
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
