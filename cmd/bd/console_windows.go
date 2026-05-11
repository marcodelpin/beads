//go:build windows

package main

import "syscall"

// chg-8hnz / CLA-rjj sibling (marcodelpin/beads fork — do NOT push to gastownhall):
// AttachConsole(ATTACH_PARENT_PROCESS) bridge so the bd binary can be built with
// -H=windowsgui (no conhost flash on every spawn from hooks, statusline, /bdloop,
// /go scans) yet still emit stdout/stderr to an interactive pwsh terminal when
// the user types `bd ready` or `bd show <id>` directly.
//
// AttachConsole semantics:
//   - parent has a console attached (pwsh user typed bd …) → attach to it,
//     Go runtime's os.Stdout/os.Stderr now flow back through the parent's
//     conhost. Output appears in the terminal exactly as for the legacy
//     console-subsystem build.
//   - parent has NO console (hook spawned us via CreateProcess+pipes, e.g.
//     claude-hooks invoking `bd json` for a scan) → AttachConsole fails
//     silently with ERROR_INVALID_HANDLE. The Go runtime's stdio handles
//     (inherited from the parent's pipes) keep working unchanged — caller
//     still captures our stdout via the pipe.
//
// Net effect across both modes: zero conhost flash, no regression to
// interactive use.
//
// kernel32!AttachConsole signature: BOOL AttachConsole(DWORD dwProcessId);
// ATTACH_PARENT_PROCESS = (DWORD)-1 = 0xFFFFFFFF.
var (
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	procAttachConsole = kernel32.NewProc("AttachConsole")
	attachParentPID   = uintptr(^uintptr(0)) // (DWORD)-1
)

func attachParentConsole() {
	// Fire-and-forget: return value intentionally ignored. Failure is the
	// expected/correct outcome when there is no parent console (hook spawn).
	_, _, _ = procAttachConsole.Call(attachParentPID)
}
