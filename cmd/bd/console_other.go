//go:build !windows

package main

// chg-8hnz: no-op on non-Windows. The console-flash problem is Windows-specific
// (conhost.exe allocation on console-subsystem binary launch). On Linux/macOS
// stdio works through inherited file descriptors regardless of "subsystem".
func attachParentConsole() {}
