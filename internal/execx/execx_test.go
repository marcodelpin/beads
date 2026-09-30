package execx

import (
	"context"
	"testing"
)

// TestGitCommandArgs verifies GitCommand builds a git invocation with the
// caller's args appended after the binary name (drop-in for
// exec.Command("git", ...)).
func TestGitCommandArgs(t *testing.T) {
	cmd := GitCommand("rev-parse", "--git-dir")
	if len(cmd.Args) != 3 || cmd.Args[1] != "rev-parse" || cmd.Args[2] != "--git-dir" {
		t.Errorf("unexpected args: %v", cmd.Args)
	}
}

// TestGitCommandContextArgs verifies the context variant builds the same argv.
func TestGitCommandContextArgs(t *testing.T) {
	cmd := GitCommandContext(context.Background(), "status")
	if len(cmd.Args) != 2 || cmd.Args[1] != "status" {
		t.Errorf("unexpected args: %v", cmd.Args)
	}
}

func TestCommandContextKeepsPinnedExecutable(t *testing.T) {
	cmd := CommandContext(context.Background(), "/opt/git/bin/git", "-c", "core.hooksPath=", "status")
	if cmd.Path != "/opt/git/bin/git" {
		t.Errorf("Path = %q, want the pinned executable", cmd.Path)
	}
	if len(cmd.Args) != 4 || cmd.Args[0] != "/opt/git/bin/git" || cmd.Args[3] != "status" {
		t.Errorf("unexpected args: %v", cmd.Args)
	}
}
