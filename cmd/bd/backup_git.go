package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/steveyegge/beads/internal/debug"
)

// gitBackup adds, commits, and pushes the backup directory to git.
// Failures are logged as warnings, never fatal — git push is best-effort.
func gitBackup(ctx context.Context) error {
	dir, err := backupDir()
	if err != nil {
		return err
	}

	// git add -f .beads/backup/ (force-add past .gitignore)
	if err := gitExec(ctx, "add", "-f", dir); err != nil {
		return fmt.Errorf("git add: %w", err)
	}

	// Check if there's anything to commit
	out, err := exec.CommandContext(ctx, "git", "diff", "--cached", "--quiet", "--", dir).CombinedOutput()
	if err == nil {
		debug.Logf("backup: no git changes to commit\n")
		return nil // nothing staged
	}
	// exit code 1 = there are differences (good, we want to commit)
	_ = out

	// git commit
	msg := fmt.Sprintf("bd: backup %s", time.Now().UTC().Format("2006-01-02 15:04"))
	if err := gitExec(ctx, "commit", "-m", msg, "--", dir); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}

	// git push with timeout (failure = warning only)
	pushCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := gitExec(pushCtx, "push"); err != nil {
		debug.Logf("backup: git push failed (non-fatal): %v\n", err)
		fmt.Fprintf(os.Stderr, "Warning: backup git push failed: %v\n", err)
		return nil // non-fatal
	}

	return nil
}

// gitExec runs a git command and returns any error.
func gitExec(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, out)
	}
	return nil
}
