//go:build cgo

package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestEmbeddedGCPlanHonoursRetainedTypes pins that the consent plans of
// `bd gc` (--plan JSON and --plan-summary text) list exactly what the decay
// would delete: a closed bead of a type named in types.retained is held back
// by the delete path, so it must not be offered for consent either, while a
// closed bead of another type still is (bda-tfok.4.5, codex round 1 F6).
func TestEmbeddedGCPlanHonoursRetainedTypes(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()
	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "gp")

	run := func(args ...string) (stdout, stderr string, exitCode int) {
		t.Helper()
		cmd := exec.Command(bd, args...)
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		out, errOut, err := runCommandBuffers(t, cmd)
		var exit *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &exit):
			exitCode = exit.ExitCode()
		default:
			t.Fatalf("bd %s: %v", strings.Join(args, " "), err)
		}
		return out.String(), errOut.String(), exitCode
	}

	if out, stderr, code := run("config", "set", "types.retained", "chore"); code != 0 {
		t.Fatalf("bd config set types.retained exited %d:\n%s\n%s", code, out, stderr)
	}
	retained := bdCreate(t, bd, dir, "retained chore", "--id", "gp-keep", "--type", "chore").ID
	bdClose(t, bd, dir, retained)
	listed := bdCreate(t, bd, dir, "ordinary task", "--id", "gp-take", "--type", "task").ID
	bdClose(t, bd, dir, listed)
	// --older-than 0 selects closed_at before now cut to the second, and a
	// close stamp is stored rounded half-up to the second (see
	// TestEmbeddedRetainedTypes), so let both stamps fall behind the cutoff.
	time.Sleep(2 * time.Second)

	t.Run("plan json", func(t *testing.T) {
		out, stderr, code := run("gc", "--plan", "--older-than", "0", "--allow-recent")
		if code != 0 {
			t.Fatalf("bd gc --plan exited %d:\n%s\n%s", code, out, stderr)
		}
		var plan struct {
			IssueCount        int `json:"issue_count"`
			RetentionRequired int `json:"retention_required"`
			Issues            []struct {
				ID string `json:"id"`
			} `json:"issues"`
		}
		if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &plan); err != nil {
			t.Fatalf("bd gc --plan output is not JSON: %v\n%s", err, out)
		}
		ids := map[string]bool{}
		for _, issue := range plan.Issues {
			ids[issue.ID] = true
		}
		if ids[retained] {
			t.Errorf("bd gc --plan lists %s, a closed bead of a retained type the decay keeps:\n%s", retained, out)
		}
		if !ids[listed] {
			t.Errorf("bd gc --plan omits %s, a closed task the decay deletes:\n%s", listed, out)
		}
		if plan.IssueCount != len(plan.Issues) {
			t.Errorf("issue_count = %d, want len(issues) = %d", plan.IssueCount, len(plan.Issues))
		}
		if plan.RetentionRequired != 1 {
			t.Errorf("retention_required = %d, want 1 (the retained chore held back):\n%s", plan.RetentionRequired, out)
		}
	})

	t.Run("plan summary", func(t *testing.T) {
		out, stderr, code := run("gc", "--plan-summary", "--older-than", "0", "--allow-recent")
		if code != 0 {
			t.Fatalf("bd gc --plan-summary exited %d:\n%s\n%s", code, out, stderr)
		}
		if strings.Contains(out, retained) {
			t.Errorf("bd gc --plan-summary lists %s, a closed bead of a retained type the decay keeps:\n%s", retained, out)
		}
		if !strings.Contains(out, listed) {
			t.Errorf("bd gc --plan-summary omits %s, a closed task the decay deletes:\n%s", listed, out)
		}
		if !strings.Contains(out, "Kept 1 closed issue(s) of a retained type") {
			t.Errorf("bd gc --plan-summary does not say it held the retained bead back:\n%s", out)
		}
	})
}
