//go:build windows

package main

import "os"

// processAlive reports whether the process is still running.
//
// Windows has no signals: os.Process.Signal rejects anything but Kill with
// "not supported by windows", so the Unix null-signal probe (signal 0) always
// returned an error here and made every live PID look dead.
//
// The existence check is instead already performed by os.FindProcess, which on
// Windows issues a real OpenProcess and fails when the PID does not exist. So by
// the time we hold a *os.Process the process was found, which is exactly the
// heuristic this caller wants (see isServerProbablyRunning: "probably").
func processAlive(_ *os.Process) bool {
	return true
}
