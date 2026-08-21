//go:build !windows

package execx

import "os/exec"

// hideConsole is a no-op on non-Windows platforms: console windows are a
// Windows-only concept.
func hideConsole(_ *exec.Cmd) {}
