package testutil

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const testPidDir = "/tmp"
const testPidPrefix = "beads-test-dolt-"

// TestDoltServer represents a running test dolt server instance.
type TestDoltServer struct {
	Port    int
	cmd     *exec.Cmd
	tmpDir  string
	pidFile string
}

// StartTestDoltServer starts a dedicated Dolt SQL server in a temp directory
// on a dynamic port. Cleans up stale test servers first. Installs a signal
// handler so cleanup runs even when tests are interrupted with Ctrl+C.
//
// tmpDirPrefix is the os.MkdirTemp prefix (e.g. "beads-test-dolt-*").
// Returns the server (nil if dolt not installed) and a cleanup function.
func StartTestDoltServer(tmpDirPrefix string) (*TestDoltServer, func()) {
	CleanStaleTestServers()

	if _, err := exec.LookPath("dolt"); err != nil {
		return nil, func() {}
	}

	tmpDir, err := os.MkdirTemp("", tmpDirPrefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN: failed to create test dolt dir: %v\n", err)
		return nil, func() {}
	}

	dbDir := filepath.Join(tmpDir, "data")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: failed to create test dolt data dir: %v\n", err)
		_ = os.RemoveAll(tmpDir)
		return nil, func() {}
	}

	// Configure dolt user identity (required by dolt init).
	doltEnv := append(os.Environ(), "DOLT_ROOT_PATH="+tmpDir)
	for _, args := range [][]string{
		{"dolt", "config", "--global", "--add", "user.name", "beads-test"},
		{"dolt", "config", "--global", "--add", "user.email", "test@beads.local"},
	} {
		cfgCmd := exec.Command(args[0], args[1:]...)
		cfgCmd.Env = doltEnv
		if out, err := cfgCmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: %s failed: %v\n%s\n", args[1], err, out)
			_ = os.RemoveAll(tmpDir)
			return nil, func() {}
		}
	}

	initCmd := exec.Command("dolt", "init")
	initCmd.Dir = dbDir
	initCmd.Env = doltEnv
	if out, err := initCmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: dolt init failed for test server: %v\n%s\n", err, out)
		_ = os.RemoveAll(tmpDir)
		return nil, func() {}
	}

	port, err := FindFreePort()
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN: failed to find free port: %v\n", err)
		_ = os.RemoveAll(tmpDir)
		return nil, func() {}
	}

	serverCmd := exec.Command("dolt", "sql-server",
		"-H", "127.0.0.1",
		"-P", fmt.Sprintf("%d", port),
		"--no-auto-commit",
	)
	serverCmd.Dir = dbDir
	serverCmd.Env = doltEnv
	if os.Getenv("BEADS_TEST_DOLT_VERBOSE") != "1" {
		serverCmd.Stderr = nil
		serverCmd.Stdout = nil
	}
	if err := serverCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: failed to start test dolt server: %v\n", err)
		_ = os.RemoveAll(tmpDir)
		return nil, func() {}
	}

	// Write PID file so stale cleanup can find orphans from interrupted runs
	pidFile := filepath.Join(testPidDir, fmt.Sprintf("%s%d.pid", testPidPrefix, port))
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(serverCmd.Process.Pid)), 0600)

	if !WaitForServer(port, 10*time.Second) {
		fmt.Fprintf(os.Stderr, "WARN: test dolt server did not become ready on port %d\n", port)
		_ = serverCmd.Process.Kill()
		_ = serverCmd.Wait()
		_ = os.RemoveAll(tmpDir)
		_ = os.Remove(pidFile)
		return nil, func() {}
	}

	srv := &TestDoltServer{
		Port:    port,
		cmd:     serverCmd,
		tmpDir:  tmpDir,
		pidFile: pidFile,
	}

	// Install signal handler so cleanup runs even when defer doesn't
	// (e.g. Ctrl+C during test run)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		srv.cleanup()
		os.Exit(1)
	}()

	cleanup := func() {
		signal.Stop(sigCh)
		srv.cleanup()
	}

	return srv, cleanup
}

// cleanup stops the server, removes temp dir and PID file.
func (s *TestDoltServer) cleanup() {
	if s == nil {
		return
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
	if s.tmpDir != "" {
		_ = os.RemoveAll(s.tmpDir)
	}
	if s.pidFile != "" {
		_ = os.Remove(s.pidFile)
	}
}

// CleanStaleTestServers kills orphaned test dolt servers from previous
// interrupted test runs by scanning PID files in /tmp.
func CleanStaleTestServers() {
	pattern := filepath.Join(testPidDir, testPidPrefix+"*.pid")
	entries, err := filepath.Glob(pattern)
	if err != nil {
		return
	}
	for _, pidFile := range entries {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			_ = os.Remove(pidFile)
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			_ = os.Remove(pidFile)
			continue
		}
		process, err := os.FindProcess(pid)
		if err != nil {
			_ = os.Remove(pidFile)
			continue
		}
		if err := process.Signal(syscall.Signal(0)); err != nil {
			// Process is dead — clean up stale PID file
			_ = os.Remove(pidFile)
			continue
		}
		// Process is alive — verify it's a dolt server before killing
		if isDoltTestProcess(pid) {
			_ = process.Signal(syscall.SIGKILL)
			time.Sleep(100 * time.Millisecond)
		}
		_ = os.Remove(pidFile)
	}
}

// FindFreePort finds an available TCP port by binding to :0.
func FindFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

// WaitForServer polls until the server accepts TCP connections on the given port.
func WaitForServer(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// isDoltTestProcess verifies that a PID belongs to a dolt sql-server process.
func isDoltTestProcess(pid int) bool {
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	cmdline := strings.TrimSpace(string(output))
	return strings.Contains(cmdline, "dolt") && strings.Contains(cmdline, "sql-server")
}
