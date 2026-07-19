// Tests for multi-ID `bd update` exit-code semantics (GH audit: multi-ID
// update exits 0 after a mid-batch failure, so callers cannot detect that
// some IDs were not updated).
//
// Contract under test:
//   - `bd update` applies updates per issue ID, not atomically across IDs:
//     successful IDs stay applied even when other IDs fail.
//   - Any per-ID failure makes the command exit nonzero with a per-ID error
//     report on stderr.
//   - In --json mode stdout keeps the existing success shape (array of
//     updated issues) and the failure report is emitted as a single JSON
//     line on stderr listing which IDs failed.
//   - The all-good path is unchanged: exit 0, clean stderr.
//
// This file MUST NOT carry a cgo build tag: it exercises the default sqlite
// backend via a bd binary built with the gms_pure_go tag.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// multiIDUpdateEnv returns a hermetic environment for bd subprocess runs:
// no inherited BEADS_* variables, HOME pinned to the test dir, metrics and
// daemons disabled.
func multiIDUpdateEnv(dir string) []string {
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "BEADS_") {
			continue
		}
		env = append(env, e)
	}
	return append(env,
		"HOME="+dir,
		"BD_NON_INTERACTIVE=1",
		"BD_DISABLE_METRICS=1",
		"BD_DISABLE_EVENT_FLUSH=1",
		"BEADS_NO_DAEMON=1",
		"BEADS_DOLT_AUTO_START=0",
	)
}

// runBDMultiID runs the bd binary and returns stdout, stderr, and the exit
// code. Only a failure to launch the process fails the test; nonzero exits
// are returned to the caller for assertion.
func runBDMultiID(t *testing.T, bd, dir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(bd, args...)
	cmd.Dir = dir
	cmd.Env = multiIDUpdateEnv(dir)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("bd %v did not run: %v", args, err)
		}
		return outBuf.String(), errBuf.String(), ee.ExitCode()
	}
	return outBuf.String(), errBuf.String(), 0
}

// setupMultiIDUpdateDB builds bd and initializes a fresh sqlite-backed
// database in a temp dir.
func setupMultiIDUpdateDB(t *testing.T) (bd, dir string) {
	t.Helper()
	bd = buildBDForInitTests(t)
	dir = t.TempDir()
	stdout, stderr, code := runBDMultiID(t, bd, dir,
		"init", "--prefix", "test", "--quiet", "--non-interactive", "--skip-hooks", "--skip-agents")
	if code != 0 {
		t.Fatalf("bd init failed (exit %d):\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	return bd, dir
}

// createMultiIDUpdateIssue creates an issue and returns its ID.
func createMultiIDUpdateIssue(t *testing.T, bd, dir, title string) string {
	t.Helper()
	stdout, stderr, code := runBDMultiID(t, bd, dir, "create", title, "-p", "2", "--json")
	if code != 0 {
		t.Fatalf("bd create failed (exit %d):\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	var issue struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(stdout), &issue); err != nil {
		t.Fatalf("parsing create --json output: %v\n%s", err, stdout)
	}
	if issue.ID == "" {
		t.Fatalf("bd create --json returned no id:\n%s", stdout)
	}
	return issue.ID
}

// showMultiIDUpdatePriority fetches an issue's priority via bd show --json.
func showMultiIDUpdatePriority(t *testing.T, bd, dir, id string) int {
	t.Helper()
	stdout, stderr, code := runBDMultiID(t, bd, dir, "show", id, "--json")
	if code != 0 {
		t.Fatalf("bd show %s failed (exit %d):\nstdout:\n%s\nstderr:\n%s", id, code, stdout, stderr)
	}
	var details []struct {
		ID       string `json:"id"`
		Priority int    `json:"priority"`
	}
	if err := json.Unmarshal([]byte(stdout), &details); err != nil {
		t.Fatalf("parsing show --json output for %s: %v\n%s", id, err, stdout)
	}
	if len(details) != 1 || details[0].ID != id {
		t.Fatalf("show --json for %s returned unexpected issues:\n%s", id, stdout)
	}
	return details[0].Priority
}

// bogusMultiIDUpdateID is an ID that cannot collide with generated issue IDs
// (bd resolves fuzzy/substring matches, so it must not be a substring of a
// real ID either).
const bogusMultiIDUpdateID = "test-zzzzzzzzzz"

func TestMultiIDUpdatePartialFailureExitsNonzero(t *testing.T) {
	bd, dir := setupMultiIDUpdateDB(t)
	id1 := createMultiIDUpdateIssue(t, bd, dir, "first issue")
	id2 := createMultiIDUpdateIssue(t, bd, dir, "second issue")

	stdout, stderr, code := runBDMultiID(t, bd, dir, "update", id1, bogusMultiIDUpdateID, id2, "--priority", "0")
	if code == 0 {
		t.Errorf("bd update with a bogus ID mid-batch exited 0, want nonzero\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, bogusMultiIDUpdateID) {
		t.Errorf("stderr does not mention the failed ID %s:\n%s", bogusMultiIDUpdateID, stderr)
	}

	// The command is per-ID, not atomic: both good IDs must still be applied.
	for _, id := range []string{id1, id2} {
		if got := showMultiIDUpdatePriority(t, bd, dir, id); got != 0 {
			t.Errorf("issue %s priority = %d, want 0 (successful IDs must stay applied)", id, got)
		}
	}
}

func TestMultiIDUpdatePartialFailureJSONReportsFailedIDs(t *testing.T) {
	bd, dir := setupMultiIDUpdateDB(t)
	id1 := createMultiIDUpdateIssue(t, bd, dir, "first issue")
	id2 := createMultiIDUpdateIssue(t, bd, dir, "second issue")

	stdout, stderr, code := runBDMultiID(t, bd, dir, "update", id1, bogusMultiIDUpdateID, id2, "--priority", "0", "--json")
	if code == 0 {
		t.Errorf("bd update --json with a bogus ID mid-batch exited 0, want nonzero\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	// stdout keeps the existing success shape: an array of the successfully
	// updated issues.
	var updated []struct {
		ID       string `json:"id"`
		Priority int    `json:"priority"`
	}
	if err := json.Unmarshal([]byte(stdout), &updated); err != nil {
		t.Fatalf("stdout is not the array-of-issues success shape: %v\n%s", err, stdout)
	}
	gotIDs := map[string]bool{}
	for _, u := range updated {
		gotIDs[u.ID] = true
		if u.Priority != 0 {
			t.Errorf("updated issue %s priority = %d, want 0", u.ID, u.Priority)
		}
	}
	if !gotIDs[id1] || !gotIDs[id2] || len(updated) != 2 {
		t.Errorf("stdout array = %v, want exactly [%s %s]", updated, id1, id2)
	}

	// The last stderr line is a machine-parseable failure report naming the
	// failed ID.
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	last := lines[len(lines)-1]
	var report struct {
		Error  string `json:"error"`
		Failed []struct {
			ID    string `json:"id"`
			Error string `json:"error"`
		} `json:"failed"`
	}
	if err := json.Unmarshal([]byte(last), &report); err != nil {
		t.Fatalf("last stderr line is not a JSON failure report: %v\nstderr:\n%s", err, stderr)
	}
	if report.Error == "" {
		t.Errorf("JSON failure report has empty error message: %s", last)
	}
	if len(report.Failed) != 1 || report.Failed[0].ID != bogusMultiIDUpdateID {
		t.Errorf("JSON failure report failed list = %+v, want exactly one entry for %s", report.Failed, bogusMultiIDUpdateID)
	}
	if len(report.Failed) == 1 && report.Failed[0].Error == "" {
		t.Errorf("JSON failure report entry has empty per-ID error: %s", last)
	}
}

func TestMultiIDUpdateAllFailStillExitsNonzero(t *testing.T) {
	bd, dir := setupMultiIDUpdateDB(t)

	stdout, stderr, code := runBDMultiID(t, bd, dir, "update", bogusMultiIDUpdateID, "--priority", "0")
	if code == 0 {
		t.Errorf("bd update with only a bogus ID exited 0, want nonzero\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, bogusMultiIDUpdateID) {
		t.Errorf("stderr does not mention the failed ID %s:\n%s", bogusMultiIDUpdateID, stderr)
	}
}

func TestMultiIDUpdateAllGoodPathUnchanged(t *testing.T) {
	bd, dir := setupMultiIDUpdateDB(t)
	id1 := createMultiIDUpdateIssue(t, bd, dir, "first issue")
	id2 := createMultiIDUpdateIssue(t, bd, dir, "second issue")

	stdout, stderr, code := runBDMultiID(t, bd, dir, "update", id1, id2, "--priority", "1", "--json")
	if code != 0 {
		t.Fatalf("all-good bd update --json exited %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if stderr != "" {
		t.Errorf("all-good bd update --json wrote to stderr (success path must stay byte-identical):\n%s", stderr)
	}
	var updated []struct {
		ID       string `json:"id"`
		Priority int    `json:"priority"`
	}
	if err := json.Unmarshal([]byte(stdout), &updated); err != nil {
		t.Fatalf("stdout is not the array-of-issues success shape: %v\n%s", err, stdout)
	}
	if len(updated) != 2 {
		t.Fatalf("updated %d issues, want 2:\n%s", len(updated), stdout)
	}
	for _, u := range updated {
		if u.Priority != 1 {
			t.Errorf("updated issue %s priority = %d, want 1", u.ID, u.Priority)
		}
	}
}

// seedNonObjectMetadata sets an issue's metadata to a JSON array so a later
// --metadata merge or --set-metadata edit for that ID fails ("existing metadata
// is not a JSON object"). --metadata only requires valid JSON, not an object,
// so the non-object value persists and reproduces the mid-batch metadata
// failure the per-ID contract must survive.
func seedNonObjectMetadata(t *testing.T, bd, dir, id string) {
	t.Helper()
	stdout, stderr, code := runBDMultiID(t, bd, dir, "update", id, "--metadata", "[]")
	if code != 0 {
		t.Fatalf("seeding non-object metadata on %s failed (exit %d):\nstdout:\n%s\nstderr:\n%s", id, code, stdout, stderr)
	}
}

// showMultiIDUpdateMetadataValue returns a string-valued metadata key for an
// issue via bd show --json, failing the test if the key is missing or not a
// JSON string. Used to confirm that IDs surrounding a failed one still received
// their metadata update.
func showMultiIDUpdateMetadataValue(t *testing.T, bd, dir, id, key string) string {
	t.Helper()
	stdout, stderr, code := runBDMultiID(t, bd, dir, "show", id, "--json")
	if code != 0 {
		t.Fatalf("bd show %s failed (exit %d):\nstdout:\n%s\nstderr:\n%s", id, code, stdout, stderr)
	}
	var details []struct {
		ID       string                     `json:"id"`
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(stdout), &details); err != nil {
		t.Fatalf("parsing show --json output for %s: %v\n%s", id, err, stdout)
	}
	if len(details) != 1 || details[0].ID != id {
		t.Fatalf("show --json for %s returned unexpected issues:\n%s", id, stdout)
	}
	raw, ok := details[0].Metadata[key]
	if !ok {
		t.Fatalf("issue %s metadata has no key %q:\n%s", id, key, stdout)
	}
	var val string
	if err := json.Unmarshal(raw, &val); err != nil {
		t.Fatalf("issue %s metadata[%q] = %s, not a JSON string: %v", id, key, raw, err)
	}
	return val
}

// TestMultiIDUpdatePartialMetadataMergeFailure covers a --metadata merge that
// fails for one middle ID (its stored metadata is not a JSON object). The merge
// failure must be a per-ID failure — reported on stderr with a nonzero exit —
// not a batch abort that skips the trailing IDs.
func TestMultiIDUpdatePartialMetadataMergeFailure(t *testing.T) {
	bd, dir := setupMultiIDUpdateDB(t)
	id1 := createMultiIDUpdateIssue(t, bd, dir, "first issue")
	id2 := createMultiIDUpdateIssue(t, bd, dir, "second issue")
	id3 := createMultiIDUpdateIssue(t, bd, dir, "third issue")
	seedNonObjectMetadata(t, bd, dir, id2)

	stdout, stderr, code := runBDMultiID(t, bd, dir, "update", id1, id2, id3, "--metadata", `{"team":"platform"}`)
	if code == 0 {
		t.Errorf("bd update with a mid-batch metadata-merge failure exited 0, want nonzero\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, id2) {
		t.Errorf("stderr does not mention the failed ID %s:\n%s", id2, stderr)
	}
	// Per-ID, not atomic: the IDs on either side of the failure must still carry
	// the merged metadata.
	for _, id := range []string{id1, id3} {
		if got := showMultiIDUpdateMetadataValue(t, bd, dir, id, "team"); got != "platform" {
			t.Errorf("issue %s metadata team = %q, want \"platform\" (successful IDs must stay applied)", id, got)
		}
	}
}

// TestMultiIDUpdatePartialMetadataEditFailureJSON covers a --set-metadata edit
// that fails for one middle ID under --json. stdout must keep the success array
// for the applied IDs while the failed ID is reported only in the JSON failure
// line on stderr, with a nonzero exit.
func TestMultiIDUpdatePartialMetadataEditFailureJSON(t *testing.T) {
	bd, dir := setupMultiIDUpdateDB(t)
	id1 := createMultiIDUpdateIssue(t, bd, dir, "first issue")
	id2 := createMultiIDUpdateIssue(t, bd, dir, "second issue")
	id3 := createMultiIDUpdateIssue(t, bd, dir, "third issue")
	seedNonObjectMetadata(t, bd, dir, id2)

	stdout, stderr, code := runBDMultiID(t, bd, dir, "update", id1, id2, id3, "--set-metadata", "team=platform", "--json")
	if code == 0 {
		t.Errorf("bd update --json with a mid-batch metadata-edit failure exited 0, want nonzero\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	// stdout keeps the array-of-updated-issues success shape for the IDs that
	// applied; the failed ID must not appear there.
	var updated []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(stdout), &updated); err != nil {
		t.Fatalf("stdout is not the array-of-issues success shape: %v\n%s", err, stdout)
	}
	gotIDs := map[string]bool{}
	for _, u := range updated {
		gotIDs[u.ID] = true
	}
	if !gotIDs[id1] || !gotIDs[id3] || len(updated) != 2 {
		t.Errorf("stdout array = %v, want exactly [%s %s]", updated, id1, id3)
	}
	if gotIDs[id2] {
		t.Errorf("failed ID %s must not appear in the stdout success array:\n%s", id2, stdout)
	}

	// The last stderr line is a machine-parseable failure report naming the
	// failed ID.
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	last := lines[len(lines)-1]
	var report struct {
		Error  string `json:"error"`
		Failed []struct {
			ID    string `json:"id"`
			Error string `json:"error"`
		} `json:"failed"`
	}
	if err := json.Unmarshal([]byte(last), &report); err != nil {
		t.Fatalf("last stderr line is not a JSON failure report: %v\nstderr:\n%s", err, stderr)
	}
	if len(report.Failed) != 1 || report.Failed[0].ID != id2 {
		t.Errorf("JSON failure report failed list = %+v, want exactly one entry for %s", report.Failed, id2)
	}
	if len(report.Failed) == 1 && report.Failed[0].Error == "" {
		t.Errorf("JSON failure report entry has empty per-ID error: %s", last)
	}

	// The IDs surrounding the failure still carry the edited metadata.
	for _, id := range []string{id1, id3} {
		if got := showMultiIDUpdateMetadataValue(t, bd, dir, id, "team"); got != "platform" {
			t.Errorf("issue %s metadata team = %q, want \"platform\"", id, got)
		}
	}
}
