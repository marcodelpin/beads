//go:build !windows

package main

import (
	"os"
	"syscall"
)

// processAlive reports whether a process with the given PID is still running.
//
// On Unix os.FindProcess always succeeds (it does not touch the OS), so the only
// way to tell a live PID from a dead one is to deliver the null signal: signal 0
// performs the permission and existence checks without sending anything.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
