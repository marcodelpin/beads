//go:build cgo

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestIfRevisionRecloseReportsAlreadyClosed pins the fork's GH#4816 contract
// (bda-4myc) on the --if-revision close route that #7203 added: re-closing an
// already-closed issue at its CURRENT revision is the same idempotent no-op as
// a plain re-close, so it must not print a "Closed" success line, it must say
// "already closed" on stderr in both output modes, and the first close_reason
// must survive.
func TestIfRevisionRecloseReportsAlreadyClosed(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "rc")

	for _, mode := range []string{"human", "json"} {
		t.Run(mode, func(t *testing.T) {
			issue := bdCreate(t, bd, dir, "Guarded reclose "+mode, "--type", "task")
			bdClose(t, bd, dir, issue.ID, "--reason", "FIRST")
			rev := bdShowRevision(t, bd, dir, issue.ID)

			args := []string{"close", issue.ID, "--if-revision", revStr(rev), "--reason", "SECOND"}
			if mode == "json" {
				args = append(args, "--json")
			}
			cmd := exec.Command(bd, args...)
			cmd.Dir = dir
			cmd.Env = bdEnv(dir)
			stdout, stderr, err := runCommandBuffers(t, cmd)
			if err != nil {
				t.Fatalf("guarded re-close at the current revision must exit 0: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			if mode == "human" && strings.Contains(stdout.String(), "Closed") {
				t.Errorf("guarded re-close printed a success line for a no-op:\n%s", stdout.String())
			}
			if mode == "json" && !strings.Contains(stdout.String(), issue.ID) {
				t.Errorf("--json output should still carry the already-closed issue, got:\n%s", stdout.String())
			}
			if !strings.Contains(stderr.String(), "already closed") {
				t.Errorf("expected stderr to report the issue was already closed, got:\n%s", stderr.String())
			}
			if got := bdShow(t, bd, dir, issue.ID); got.CloseReason != "FIRST" {
				t.Errorf("close_reason = %q after guarded re-close, want %q (first close wins)", got.CloseReason, "FIRST")
			}
		})
	}
}
