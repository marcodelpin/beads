package doltserver

import (
	"os"
	"testing"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/configfile"
)

// clearPortEnv pins every environment input to port resolution so a test's
// expectation does not depend on the shell (or an integration TestMain) that
// launched the run.
func clearPortEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BEADS_DOLT_SERVER_PORT", "")
	t.Setenv("BEADS_DOLT_PORT", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "")
	t.Setenv("BEADS_DOLT_SERVER_MODE", "")
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "")
	t.Setenv("BEADS_DOLT_SERVER_SOCKET", "")
	t.Setenv("BEADS_TEST_MODE", "")
	config.ResetForTesting()
}

// TestDefaultConfig_PersistedServerModeRemoteHost is the kb-fwrz regression:
// metadata.json carrying BOTH dolt_mode: server and a remote dolt_server_host,
// with no port configured anywhere. The persisted mode suppresses host
// inference (HostImpliesServerMode returns false as soon as dolt_mode is set),
// so ResolveServerMode reports Owned even though the server is on another
// machine. Gating the remote-host port default on ServerModeExternal therefore
// left this shape at port 0: `bd dolt status` dialed host:0, got "connection
// refused", and reported a healthy remote as unreachable — while `bd dolt
// push` reached it, because the storage layer applies its own remote default.
func TestDefaultConfig_PersistedServerModeRemoteHost(t *testing.T) {
	clearPortEnv(t)

	dir := t.TempDir()
	metaCfg := &configfile.Config{
		DoltMode:       configfile.DoltModeServer,
		DoltServerHost: "dolt.example.test",
	}
	if err := metaCfg.Save(dir); err != nil {
		t.Fatal(err)
	}

	// Precondition: this is the mode-gate hole, not an external-mode repo.
	if mode := ResolveServerMode(dir); mode != ServerModeOwned {
		t.Fatalf("precondition: persisted dolt_mode should resolve Owned, got %v", mode)
	}

	cfg := DefaultConfig(dir)
	if cfg.Port != DefaultSharedServerPort {
		t.Errorf("DefaultConfig.Port = %d, want %d for a remote host with no port configured", cfg.Port, DefaultSharedServerPort)
	}
	if cfg.PortSource != PortSourceExternalHostDefault {
		t.Errorf("DefaultConfig.PortSource = %q, want %q", cfg.PortSource, PortSourceExternalHostDefault)
	}
}

// TestDefaultConfig_PersistedServerModeRemoteHost_StalePortFile checks that
// the port file — bd's bookkeeping for a bd-owned LOCAL server — is not
// paired with a remote host in this mode either. Dialing the remote machine
// on a local ephemeral port fails just as reliably as dialing :0.
func TestDefaultConfig_PersistedServerModeRemoteHost_StalePortFile(t *testing.T) {
	clearPortEnv(t)

	dir := t.TempDir()
	metaCfg := &configfile.Config{
		DoltMode:       configfile.DoltModeServer,
		DoltServerHost: "dolt.example.test",
	}
	if err := metaCfg.Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(portPath(dir), []byte("45123"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig(dir)
	if cfg.Port != DefaultSharedServerPort {
		t.Errorf("DefaultConfig.Port = %d, want %d: a stale local port file must not be paired with a remote host", cfg.Port, DefaultSharedServerPort)
	}
}

// TestDefaultConfig_PersistedServerModeRemoteHost_LegacyEnvWins pins the other
// half of the parity: the storage layer honors the legacy BEADS_DOLT_PORT
// spelling ahead of persisted sources, so resolution here must too — otherwise
// a diagnostic reports one port while the connection uses another.
func TestDefaultConfig_PersistedServerModeRemoteHost_LegacyEnvWins(t *testing.T) {
	clearPortEnv(t)
	t.Setenv("BEADS_DOLT_PORT", "34567")

	dir := t.TempDir()
	metaCfg := &configfile.Config{
		DoltMode:       configfile.DoltModeServer,
		DoltServerHost: "dolt.example.test",
	}
	if err := metaCfg.Save(dir); err != nil {
		t.Fatal(err)
	}

	if cfg := DefaultConfig(dir); cfg.Port != 34567 {
		t.Errorf("DefaultConfig.Port = %d, want 34567 from legacy BEADS_DOLT_PORT", cfg.Port)
	}
}

// TestDefaultConfig_LocalHostUnaffected guards the blast radius: a local host
// must still resolve to 0 so Start() allocates an ephemeral port. Defaulting
// it to a fixed port is the cross-project data leakage of GH#2098 / GH#2372.
func TestDefaultConfig_LocalHostUnaffected(t *testing.T) {
	clearPortEnv(t)

	dir := t.TempDir()
	metaCfg := &configfile.Config{
		DoltMode:       configfile.DoltModeServer,
		DoltServerHost: "127.0.0.1",
	}
	if err := metaCfg.Save(dir); err != nil {
		t.Fatal(err)
	}

	if cfg := DefaultConfig(dir); cfg.Port != 0 {
		t.Errorf("DefaultConfig.Port = %d, want 0 for a local host with no port configured", cfg.Port)
	}
}

// TestDefaultConfig_ProxiedServerRemoteHostExempt keeps the proxied-server
// exemption that HostImpliesServerMode applies: those workspaces talk to a
// team server, never to the Dolt port, so filling one in invents provenance.
func TestDefaultConfig_ProxiedServerRemoteHostExempt(t *testing.T) {
	clearPortEnv(t)

	dir := t.TempDir()
	metaCfg := &configfile.Config{
		DoltMode:       configfile.DoltModeProxiedServer,
		DoltServerHost: "dolt.example.test",
	}
	if err := metaCfg.Save(dir); err != nil {
		t.Fatal(err)
	}

	if cfg := DefaultConfig(dir); cfg.Port != 0 {
		t.Errorf("DefaultConfig.Port = %d, want 0 for a proxied-server workspace", cfg.Port)
	}
}

func TestRemoteFallbackPort(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		env      map[string]string
		wantPort int
		wantOK   bool
	}{
		{name: "remote host", host: "dolt.example.test", wantPort: DefaultSharedServerPort, wantOK: true},
		{name: "localhost", host: "localhost"},
		{name: "loopback ip", host: "127.0.0.1"},
		{name: "ipv6 loopback", host: "::1"},
		{name: "empty host", host: ""},
		{
			// A configured unix socket makes the TCP port unused; the
			// storage layer skips the default there, so this must too.
			name: "socket configured",
			host: "dolt.example.test",
			env:  map[string]string{"BEADS_DOLT_SERVER_SOCKET": "/tmp/dolt.sock"},
		},
		{
			// Tests must never silently reach a production shared server.
			name: "test mode",
			host: "dolt.example.test",
			env:  map[string]string{"BEADS_TEST_MODE": "1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BEADS_DOLT_SERVER_SOCKET", "")
			t.Setenv("BEADS_TEST_MODE", "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			port, ok := RemoteFallbackPort(tt.host)
			if ok != tt.wantOK || port != tt.wantPort {
				t.Errorf("RemoteFallbackPort(%q) = (%d, %t), want (%d, %t)", tt.host, port, ok, tt.wantPort, tt.wantOK)
			}
		})
	}
}
