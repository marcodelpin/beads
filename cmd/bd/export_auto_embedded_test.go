//go:build cgo

// These three tests drive a child "bd init"/"bd" binary that opens an EMBEDDED
// Dolt store, so they need a CGO build. They used to live in
// export_auto_test.go, which is NOT cgo-gated because 19 of its 22 tests are
// pure Go -- under the CGO_ENABLED=0 pure-Go suite these three failed with
// "embedded Dolt requires a CGO build", an environment error rather than a
// product defect (bda-a2l).
//
// A file-level tag on the original would have excluded those 19 passing
// pure-Go tests as well, so the cgo-requiring ones move here. Matches the ~229
// other cgo-gated test files in this package.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/testutil"
)

func TestAutoExportGitAddFailureExitsNonZero(t *testing.T) {
	testutil.SkipIfShort(t, "builds+spawns the bd binary and real git subprocesses; skipped in -short (bda-9l1)")
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	bd := buildBDForInitTests(t)
	dir := t.TempDir()
	env := append(autoExportDataLossTestEnv(dir), "BD_NON_INTERACTIVE=1")

	runGit := func(args ...string) {
		t.Helper()
		c := gitCommand(args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q")

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

	run("init", "--prefix", "agf", "--quiet", "--non-interactive", "--skip-hooks", "--skip-agents")
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".beads/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("config", "set", "export.interval", "1ms")
	run("config", "set", "export.auto", "true")
	run("config", "set", "export.git-add", "true")
	if err := os.Remove(filepath.Join(dir, ".beads", exportAutoStateFile)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)

	cmd := exec.Command(bd, "create", "caller visible git add failure", "-p", "2")
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("bd create succeeded despite auto-export git add failure:\n%s", out)
	}
	output := string(out)
	if !strings.Contains(output, "Error: auto-export: git add failed") {
		t.Fatalf("expected caller-visible auto-export git add error, got:\n%s", output)
	}
	if !strings.Contains(strings.ToLower(output), "ignored") {
		t.Fatalf("expected git add stderr to explain ignored path, got:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(dir, ".beads", exportAutoStateFile)); !os.IsNotExist(err) {
		t.Fatalf("git-add failure should not save export state, stat err=%v", err)
	}
}

// TestGitAddFile_RedirectCase_DoesNotStageInMainRepo regresses the
// silent-stage-in-main follow-up from the GH#3311 review: when a worktree
// has .beads/redirect -> main/.beads, the worktree's pre-commit hook must
// NOT stage the redirected path into main's index. That would silently
// pollute a repo the user did not tell us to touch. Expected behavior is
// to skip staging entirely (the file content on disk is still correct).

func TestAutoExportSkipsEmptyExportOverPopulatedJSONL(t *testing.T) {
	testutil.SkipIfShort(t, "builds+spawns the bd binary repeatedly; skipped in -short (bda-9l1)")
	bd := buildBDForInitTests(t)
	dir := t.TempDir()
	env := autoExportDataLossTestEnv(dir)

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

	run("init", "--prefix", "dl", "--non-interactive")
	run("config", "set", "export.path", "custom.jsonl")

	jsonlPath := filepath.Join(dir, ".beads", "custom.jsonl")
	original := []byte(`{"_type":"issue","id":"dl-1","title":"Recovered issue","priority":1,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}` + "\n")
	if err := os.WriteFile(jsonlPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	run("config", "set", "export.auto", "true")
	out := run("remember", "private context that should not be auto-exported")
	if !strings.Contains(out, "refusing to overwrite") {
		t.Fatalf("expected auto-export refusal warning, got:\n%s", out)
	}

	got, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatalf("expected populated JSONL to remain: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("populated JSONL was modified:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, ".beads", exportAutoStateFile)); !os.IsNotExist(err) {
		t.Fatalf("empty skipped auto-export should not save export state, stat err=%v", err)
	}
}

func TestAutoExportSkipsWhenExistingJSONLHasIDsMissingFromStore(t *testing.T) {
	testutil.SkipIfShort(t, "builds+spawns the bd binary repeatedly; skipped in -short (bda-9l1)")
	bd := buildBDForInitTests(t)
	dir := t.TempDir()
	env := autoExportDataLossTestEnv(dir)

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

	run("init", "--prefix", "dl", "--non-interactive")
	run("config", "set", "export.path", "custom.jsonl")
	run("create", "local issue", "-p", "2")

	jsonlPath := filepath.Join(dir, ".beads", "custom.jsonl")
	original := []byte(strings.Join([]string{
		`{"_type":"issue","id":"dl-1","title":"Local issue","priority":2,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`,
		`{"_type":"issue","id":"dl-jsonl-only","title":"Only in JSONL","priority":1,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`,
		``,
	}, "\n"))
	if err := os.WriteFile(jsonlPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	run("config", "set", "export.interval", "1ms")
	run("config", "set", "export.auto", "true")
	out := run("create", "another local issue", "-p", "2")
	if !strings.Contains(out, "JSONL-only issue record") || !strings.Contains(out, "dl-jsonl-only") {
		t.Fatalf("expected JSONL-only refusal warning, got:\n%s", out)
	}

	got, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatalf("expected JSONL to remain: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("JSONL-only records were overwritten:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, ".beads", exportAutoStateFile)); !os.IsNotExist(err) {
		t.Fatalf("skipped auto-export should not save export state, stat err=%v", err)
	}
}

func autoExportDataLossTestEnv(home string) []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "BEADS_") {
			continue
		}
		env = append(env, e)
	}
	return append(env, "HOME="+home, "BEADS_DOLT_AUTO_START=0", "BEADS_NO_DAEMON=1", "BD_DISABLE_METRICS=1", "BD_DISABLE_EVENT_FLUSH=1")
}

// jsonlHasIssueID reports whether the JSONL export at path lists id, read
// through the same parser the auto-export guards use.
func jsonlHasIssueID(t *testing.T, path, id string) bool {
	t.Helper()
	records, err := issueRecordsInJSONL(path)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, r := range records {
		if r.ID == id {
			return true
		}
	}
	return false
}

// TestEmbeddedAutoExportFirstCreateAfterOptIn walks the opt-in recipe the
// configuration docs give (`export.auto` then `export.git-add`) on a fresh
// embedded workspace, then creates one issue inside the default 60s
// export.interval and reads issues.jsonl WITHOUT any manual export. Before
// GH#6955's fix the opt-in's empty auto-export started the throttle window,
// so the first create was skipped and the file did not exist.
func TestEmbeddedAutoExportFirstCreateAfterOptIn(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt auto-export tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "optin")
	bdCommand(t, bd, dir, "config", "set", "export.auto", "true")
	bdCommand(t, bd, dir, "config", "set", "export.git-add", "true")

	id := strings.TrimSpace(bdCreateSilent(t, bd, dir, "first issue after opt-in"))
	if id == "" {
		t.Fatal("bd create --silent printed no id")
	}

	exportPath := filepath.Join(beadsDir, "issues.jsonl")
	if _, err := os.Stat(exportPath); err != nil {
		t.Fatalf("issues.jsonl after the first create: %v (the opt-in's empty auto-export throttled the first real export)", err)
	}
	if !jsonlHasIssueID(t, exportPath, id) {
		data, _ := os.ReadFile(exportPath)
		t.Fatalf("issues.jsonl does not list %s after the first create:\n%s", id, data)
	}
}

// TestEmbeddedAutoExportUncommittedWriteReachesJSONL pins GH#6955's second
// skip: under batch or off auto-commit an embedded write stays in the working
// set and HEAD does not move. With HEAD as the change token, auto-export saw
// "no change" and never wrote the new issue, however long the throttle had
// been open. The saved timestamp is aged past export.interval first, so
// change detection is the only thing that can skip the export.
func TestEmbeddedAutoExportUncommittedWriteReachesJSONL(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt auto-export tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	for _, tc := range []struct{ mode, prefix string }{
		{"batch", "aeb"},
		{"off", "aeo"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			dir, beadsDir, _ := bdInit(t, bd, "--prefix", tc.prefix)
			bdCommand(t, bd, dir, "config", "set", "export.auto", "true")
			exportPath := filepath.Join(beadsDir, "issues.jsonl")

			// Nonempty baseline: a committed create that auto-export wrote.
			baseline := strings.TrimSpace(bdCreateSilent(t, bd, dir, "baseline issue"))
			if !jsonlHasIssueID(t, exportPath, baseline) {
				t.Fatalf("baseline %s is not in %s; the fixture needs an exported baseline", baseline, exportPath)
			}

			// Open the throttle: age the saved timestamp past the default
			// 60s export.interval.
			state := loadExportAutoState(beadsDir)
			state.Timestamp = time.Now().Add(-2 * time.Hour)
			saveExportAutoState(beadsDir, state)
			if !shouldExport(loadExportAutoState(beadsDir), 60*time.Second) {
				t.Fatal("aged export state still throttles; the fixture must leave only change detection in the way")
			}

			head := embeddedCurrentCommit(t, beadsDir, tc.prefix)
			id := strings.TrimSpace(bdCreateSilent(t, bd, dir, "uncommitted issue", "--dolt-auto-commit", tc.mode))
			assertEmbeddedHeadUnchanged(t, beadsDir, tc.prefix, head, "create --dolt-auto-commit "+tc.mode)

			if !jsonlHasIssueID(t, exportPath, id) {
				data, _ := os.ReadFile(exportPath)
				t.Fatalf("issues.jsonl does not list %s, created under --dolt-auto-commit %s with HEAD unchanged; auto-export took the write for no change:\n%s", id, tc.mode, data)
			}
			if !jsonlHasIssueID(t, exportPath, baseline) {
				t.Fatalf("issues.jsonl lost the baseline %s", baseline)
			}
		})
	}
}
