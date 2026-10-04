package server

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/servercfg"
	"github.com/stretchr/testify/require"
)

// FakeMySQLGreeting is a minimal stand-in for a MySQL handshake packet; only
// its non-emptiness matters to doltserver.DrainAndCloseProbe. It is exported
// so the fake dolt in testmain_test.go (package server_test) greets with the
// same bytes.
var FakeMySQLGreeting = []byte("\x0a5.7.9-fake\x00")

// newConfigForListener builds a servercfg.ServerConfig whose Host()/Port()
// point at ln and whose LogLevel() is logLevel, which decides whether
// waitReady waits for dolt's ready line (info) or lets the dial decide
// (warning). YAMLConfig is the simplest settable ServerConfig
// implementation in the servercfg package: DefaultServerConfig() returns one
// (unexported behind the interface), and its exported fields are plain
// pointers, so a struct literal is enough here without needing a config file
// on disk.
func newConfigForListener(t *testing.T, ln net.Listener, logLevel string) servercfg.ServerConfig {
	t.Helper()
	addr := ln.Addr().(*net.TCPAddr)
	host := addr.IP.String()
	port := addr.Port
	return servercfg.YAMLConfig{
		LogLevelStr: &logLevel,
		ListenerConfig: servercfg.ListenerYAMLConfig{
			HostStr:    &host,
			PortNumber: &port,
		},
	}
}

// readyWatch returns a startupWatch for port that has already seen dolt's
// ready line.
func readyWatch(t *testing.T, port int) *startupWatch {
	t.Helper()
	w := newStartupWatch(nil, port)
	_, err := w.Write([]byte(doltReadyLine + "\n"))
	require.NoError(t, err)
	require.True(t, w.isReady(), "the watch must be ready before waitReady runs")
	return w
}

// serveMute accepts connections on ln and holds them open without writing
// anything until the test ends: a listener that answers the dial but is not
// a MySQL server.
func serveMute(t *testing.T, ln net.Listener) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go func(c net.Conn) {
				<-stop
				_ = c.Close()
			}(conn)
		}
	}()
}

// shortSocketPath returns a Unix socket path named name that stays under 104
// bytes (macOS's sun_path limit; Linux allows 108 including the NUL). TMPDIR is
// pinned under a suite-owned root here (testmain_test.go), whose random
// suffixes can push a socket path past the limit, so fall back to /tmp when it
// would rather than fail at bind() or skip.
func shortSocketPath(t *testing.T, name string) string {
	t.Helper()
	for _, base := range []string{"", "/tmp"} {
		dir, err := os.MkdirTemp(base, "sock")
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		if p := filepath.Join(dir, name); len(p) < 104 {
			return p
		}
	}
	t.Fatal("no Unix socket path under 104 bytes, even under /tmp")
	return ""
}

// requireNotReady runs waitReady under a 600ms context and asserts that it
// refuses readiness and gives up shortly after that deadline.
func requireNotReady(t *testing.T, s *DoltServer, watch *startupWatch, why string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s.waitReady(ctx, watch)
	elapsed := time.Since(start)
	require.Error(t, err, why)
	require.Lessf(t, elapsed, 2*time.Second, "waitReady took %s; expected it to give up shortly after the 600ms context deadline, not run the full 30s internal deadline", elapsed)
}

// TestWaitReadyGreetedListener verifies waitReady declares the server ready
// once a probe dial both succeeds and observes a MySQL greeting, under a log
// level with no ready line, where the dial alone decides.
func TestWaitReadyGreetedListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go func(c net.Conn) {
				_, _ = c.Write(FakeMySQLGreeting)
				time.Sleep(20 * time.Millisecond)
				_ = c.Close()
			}(conn)
		}
	}()

	s := &DoltServer{
		config:          newConfigForListener(t, ln, "warning"),
		egCtx:           context.Background(),
		keepAlivePeriod: time.Second,
	}

	watch := newStartupWatch(nil, ln.Addr().(*net.TCPAddr).Port)
	done := make(chan error, 1)
	go func() {
		done <- s.waitReady(context.Background(), watch)
	}()

	select {
	case err := <-done:
		require.NoError(t, err, "waitReady should succeed against a listener that sends a greeting")
	case <-time.After(5 * time.Second):
		t.Fatal("waitReady did not return within 5s against a greeted listener")
	}
}

// TestWaitReadyMuteListenerNotReady verifies waitReady keeps polling (and
// does not declare readiness) when the listener accepts connections but
// never writes a MySQL greeting, under a log level with no ready line.
func TestWaitReadyMuteListenerNotReady(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	serveMute(t, ln)

	s := &DoltServer{
		config:          newConfigForListener(t, ln, "warning"),
		egCtx:           context.Background(),
		keepAlivePeriod: time.Second,
	}
	requireNotReady(t, s, newStartupWatch(nil, ln.Addr().(*net.TCPAddr).Port),
		"waitReady must not declare readiness against a mute listener")
}

// TestWaitReadyReadyLineStillNeedsGreeting verifies that dolt's ready line
// does not stand in for the greeting: a mute listener on the configured port
// is still not ready after the line has been seen.
func TestWaitReadyReadyLineStillNeedsGreeting(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	serveMute(t, ln)

	s := &DoltServer{
		config:          newConfigForListener(t, ln, "info"),
		egCtx:           context.Background(),
		keepAlivePeriod: time.Second,
	}
	requireNotReady(t, s, readyWatch(t, ln.Addr().(*net.TCPAddr).Port),
		"the ready line must not make a mute listener ready")
}

// TestWaitReadyReadyLineMuteSocketNotReady pins the case the ready line cannot
// cover: a foreign, mute listener holding the configured Unix socket. Dolt
// treats a taken socket as nonfatal and still logs its ready line, while Dial
// prefers the socket, so only the missing greeting tells the two apart.
func TestWaitReadyReadyLineMuteSocketNotReady(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix domain sockets not supported on windows")
	}
	sock := shortSocketPath(t, "m.sock")
	t.Logf("socket path: %s (%d bytes)", sock, len(sock))
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	defer ln.Close()
	serveMute(t, ln)

	host, port, level := "127.0.0.1", freeTestPort(t), "info"
	s := &DoltServer{
		config: servercfg.YAMLConfig{
			LogLevelStr: &level,
			ListenerConfig: servercfg.ListenerYAMLConfig{
				HostStr:    &host,
				PortNumber: &port,
				Socket:     &sock,
			},
		},
		egCtx:           context.Background(),
		keepAlivePeriod: time.Second,
	}
	requireNotReady(t, s, readyWatch(t, port),
		"a mute listener on the configured socket must not count as ready")
}
