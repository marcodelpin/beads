// Package execx provides exec.Command constructors for child processes that
// must never flash a console window on Windows.
//
// bd is built as a GUI-subsystem binary on Windows (no console of its own).
// Every console-subsystem child (git.exe) spawned without window-suppression
// attributes allocates a fresh conhost, which flashes a window for a few
// milliseconds on each spawn. GitCommand/GitCommandContext are drop-in
// replacements for exec.Command("git", ...)/exec.CommandContext(ctx, "git", ...)
// that set HideWindow + CREATE_NO_WINDOW on Windows and are no-ops elsewhere
// (bda-3co).
//
// Detached long-lived children (dolt sql-server) intentionally do NOT use this
// package: they need procAttrDetached semantics (survive the parent), which is
// a different contract.
package execx

import (
	"context"
	"os/exec"
)

// GitCommand returns exec.Command("git", arg...) with platform attributes that
// suppress the console window of the child on Windows.
func GitCommand(arg ...string) *exec.Cmd {
	cmd := exec.Command("git", arg...) // #nosec G204 -- fixed binary name, args from callers
	hideConsole(cmd)
	return cmd
}

// GitCommandContext returns exec.CommandContext(ctx, "git", arg...) with
// platform attributes that suppress the console window of the child on Windows.
func GitCommandContext(ctx context.Context, arg ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", arg...) // #nosec G204 -- fixed binary name, args from callers
	hideConsole(cmd)
	return cmd
}
