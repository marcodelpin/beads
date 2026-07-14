//go:build !windows

package main

import (
	"os"
	"syscall"
)

// processAlive reports whether the process is still running.
//
// On Unix os.FindProcess always succeeds (it does not touch the OS), so the only
// way to tell a live PID from a dead one is to deliver the null signal: signal 0
// performs the permission and existence checks without sending anything.
func processAlive(proc *os.Process) bool {
	return proc.Signal(syscall.Signal(0)) == nil
}
