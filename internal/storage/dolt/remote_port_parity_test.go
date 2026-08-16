package dolt

import (
	"testing"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/doltserver"
)

// TestRemotePortParity_StatusAndConnectionAgree pins the invariant kb-fwrz
// violated: the port a diagnostic REPORTS and the port the connection USES
// come from the same resolution.
//
// `bd dolt status` and `bd dolt show` resolve through
// doltserver.DefaultConfig; the connection path resolves through
// applyConfigDefaults. For a repo whose metadata.json carries dolt_mode:
// server plus a remote dolt_server_host and no port anywhere, the two
// disagreed — DefaultConfig returned 0 (its remote default was gated on
// ServerModeExternal, which a persisted dolt_mode suppresses) while
// applyConfigDefaults filled in the shared-server port. `bd dolt push`
// therefore worked while `bd dolt status` called the same healthy remote
// unreachable, which is exactly backwards for the command an operator runs to
// verify a push landed.
func TestRemotePortParity_StatusAndConnectionAgree(t *testing.T) {
	// The whole package runs with BEADS_TEST_MODE=1 (see TestMain), which
	// suppresses the remote default on both sides — clear it so the two
	// paths are compared where the default actually applies. No connection
	// is attempted here: both functions under test are pure config
	// resolution against a temp dir and a host that does not resolve.
	t.Setenv("BEADS_TEST_MODE", "")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "")
	t.Setenv("BEADS_DOLT_PORT", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "")
	t.Setenv("BEADS_DOLT_SERVER_MODE", "")
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "")
	t.Setenv("BEADS_DOLT_SERVER_SOCKET", "")
	config.ResetForTesting()

	const remoteHost = "dolt.example.test"
	beadsDir := t.TempDir()
	metaCfg := &configfile.Config{
		DoltMode:       configfile.DoltModeServer,
		DoltServerHost: remoteHost,
		DoltDatabase:   "parity_test",
	}
	if err := metaCfg.Save(beadsDir); err != nil {
		t.Fatal(err)
	}

	statusPort := doltserver.DefaultConfig(beadsDir).Port

	connCfg := &Config{BeadsDir: beadsDir, ServerHost: remoteHost}
	applyConfigDefaults(connCfg)

	if statusPort != connCfg.ServerPort {
		t.Errorf("port resolution disagrees: DefaultConfig (what status reports) = %d, applyConfigDefaults (what the connection uses) = %d",
			statusPort, connCfg.ServerPort)
	}
	if statusPort != doltserver.DefaultSharedServerPort {
		t.Errorf("resolved port = %d, want %d (the shared-server default for a remote host with no port configured)",
			statusPort, doltserver.DefaultSharedServerPort)
	}
}

// TestRemotePortParity_TestModeSuppressesBothSides guards the exemption that
// keeps the default from pointing a test run at a production shared server:
// under BEADS_TEST_MODE neither side may reach for it. The connection path
// then rewrites the unresolved port to the fail-fast sentinel 1.
func TestRemotePortParity_TestModeSuppressesBothSides(t *testing.T) {
	t.Setenv("BEADS_TEST_MODE", "1")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "")
	t.Setenv("BEADS_DOLT_PORT", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "")
	t.Setenv("BEADS_DOLT_SERVER_MODE", "")
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "")
	t.Setenv("BEADS_DOLT_SERVER_SOCKET", "")
	config.ResetForTesting()

	const remoteHost = "dolt.example.test"
	beadsDir := t.TempDir()
	metaCfg := &configfile.Config{
		DoltMode:       configfile.DoltModeServer,
		DoltServerHost: remoteHost,
		DoltDatabase:   "parity_test",
	}
	if err := metaCfg.Save(beadsDir); err != nil {
		t.Fatal(err)
	}

	if port := doltserver.DefaultConfig(beadsDir).Port; port != 0 {
		t.Errorf("DefaultConfig.Port = %d in test mode, want 0: a test must never silently reach the production shared server", port)
	}

	connCfg := &Config{BeadsDir: beadsDir, ServerHost: remoteHost}
	applyConfigDefaults(connCfg)
	if connCfg.ServerPort != 1 {
		t.Errorf("applyConfigDefaults ServerPort = %d in test mode, want the sentinel 1", connCfg.ServerPort)
	}
}
