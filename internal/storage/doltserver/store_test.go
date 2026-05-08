package doltserver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/steveyegge/beads/internal/storage/db/proxy"
	"github.com/steveyegge/beads/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shutdownOnInterrupt installs a SIGINT/SIGTERM handler that runs
// proxy.Shutdown(rootDir) and exits, so the proxy and child dolt sql-server
// don't survive an early test abort (e.g. Ctrl+C). The handler is removed via
// t.Cleanup on normal test completion.
func shutdownOnInterrupt(t *testing.T, rootDir string) {
	t.Helper()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
			_ = proxy.Shutdown(rootDir)
			os.Exit(1)
		case <-done:
		}
	}()
	t.Cleanup(func() {
		signal.Stop(ch)
		close(done)
	})
}

func TestNewDoltServerStore_ValidationErrors(t *testing.T) {
	cases := []struct {
		name     string
		database string
		rootUser string
		doltBin  string
		backend  proxy.Backend
		want     string
	}{
		{"empty database", "", "root", "/usr/bin/true", proxy.BackendLocalServer, "database name must not be empty"},
		{"invalid backend", "beads", "root", "/usr/bin/true", proxy.Backend("nope"), "unknown backend"},
		{"empty rootUser", "beads", "", "/usr/bin/true", proxy.BackendLocalServer, "rootUser must not be empty"},
		{"empty doltBin", "beads", "root", "", proxy.BackendLocalServer, "doltBinExec must not be empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := newDoltServerStore(
				context.Background(),
				t.TempDir(), t.TempDir(),
				tc.database, "Test", "test@example.com",
				"", "", tc.backend, false,
				tc.rootUser, "", tc.doltBin,
			)
			assert.Nil(t, s)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestNewDoltServerStore_HappyPath(t *testing.T) {
	testutil.RequireDoltBinary(t)
	bin, err := exec.LookPath("dolt")
	require.NoError(t, err)

	bdBin := buildBDBinary(t)
	prev := proxy.ResolveExecutable
	proxy.ResolveExecutable = func() (string, error) { return bdBin, nil }
	t.Cleanup(func() { proxy.ResolveExecutable = prev })

	t.Setenv("HOME", t.TempDir())

	port, err := proxy.PickFreePort()
	require.NoError(t, err)
	storeRootDir := t.TempDir()
	shutdownOnInterrupt(t, storeRootDir)
	t.Cleanup(func() {
		if err := proxy.Shutdown(storeRootDir); err != nil {
			t.Logf("proxy.Shutdown(%s): %v", storeRootDir, err)
		}
	})
	cfgPath := writeServerConfig(t, port)
	logPath := filepath.Join(t.TempDir(), "server.log")

	store, err := newDoltServerStore(
		context.Background(),
		storeRootDir,
		t.TempDir(),
		"beads",
		"test_user",
		"test@example.com",
		logPath,
		cfgPath,
		proxy.BackendLocalServer,
		false,
		"root",
		"",
		bin,
	)

	require.NoError(t, err)
	require.NotNil(t, store)
	t.Cleanup(func() { _ = store.db.Close() })
}

var (
	bdBinaryOnce sync.Once
	bdBinary     string
	bdBinaryErr  error
)

func buildBDBinary(t *testing.T) string {
	t.Helper()
	bdBinaryOnce.Do(func() {
		if prebuilt := os.Getenv("BEADS_TEST_BD_BINARY"); prebuilt != "" {
			if _, err := os.Stat(prebuilt); err != nil {
				bdBinaryErr = fmt.Errorf("BEADS_TEST_BD_BINARY=%q not found: %w", prebuilt, err)
				return
			}
			bdBinary = prebuilt
			return
		}
		tmpDir, err := os.MkdirTemp("", "bd-doltserver-test-*")
		if err != nil {
			bdBinaryErr = fmt.Errorf("temp dir: %w", err)
			return
		}
		name := "bd"
		if runtime.GOOS == "windows" {
			name = "bd.exe"
		}
		bdBinary = filepath.Join(tmpDir, name)
		cmd := exec.Command("go", "build", "-tags", "gms_pure_go", "-o", bdBinary, "github.com/steveyegge/beads/cmd/bd")
		if out, err := cmd.CombinedOutput(); err != nil {
			bdBinaryErr = fmt.Errorf("go build bd: %v\n%s", err, out)
		}
	})
	if bdBinaryErr != nil {
		t.Fatalf("build bd: %v", bdBinaryErr)
	}
	return bdBinary
}

func writeServerConfig(t *testing.T, port int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := fmt.Sprintf("log_level: debug\nlistener:\n  host: 127.0.0.1\n  port: %d\n", port)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}
