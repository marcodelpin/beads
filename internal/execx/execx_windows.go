//go:build windows

package execx

import (
	"os/exec"
	"syscall"
)

// createNoWindow is the CREATE_NO_WINDOW process creation flag: the child gets
// no console at all, so no conhost window is ever allocated.
const createNoWindow = 0x08000000

// hideConsole marks the command so its console window never appears.
// HideWindow (STARTF_USESHOWWINDOW + SW_HIDE) and CREATE_NO_WINDOW are both
// set, belt and suspenders: CREATE_NO_WINDOW prevents conhost allocation for
// console children; HideWindow covers children that re-show themselves.
// Existing SysProcAttr values set by the caller are preserved.
func hideConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
