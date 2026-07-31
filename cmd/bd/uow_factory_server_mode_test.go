package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/dbproxy/proxy"
)

// serverModeBeadsDir writes a server-mode metadata.json into a fresh beads dir
// and returns it. Nothing here starts a server: every assertion below is about
// the topology resolution, which happens before anything is dialed.
func serverModeBeadsDir(t *testing.T, cfg *configfile.Config) string {
	t.Helper()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	require.NoError(t, os.MkdirAll(beadsDir, config.BeadsDirPerm))
	cfg.Backend = configfile.BackendDolt
	cfg.DoltMode = configfile.DoltModeServer
	require.NoError(t, cfg.Save(beadsDir))
	// doltserver.DefaultConfig consults these before metadata.json; an
	// inherited value from the developer's shell would decide the assertions.
	t.Setenv("BEADS_DOLT_SERVER_PORT", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "")
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "")
	t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "")
	return beadsDir
}

// The proxy serve builds must never reap itself. serve is its only client, and
// serve's pool drops its last connection after 5 minutes of no requests — at
// which point a finite-idle proxy exits and takes the OS-assigned port the
// provider's DSN already pinned, permanently, with serve still answering
// /healthz.
//
// Without the fix this resolves to a zero idle timeout, which
// NewExternalDoltServerUOWProvider turns into the 30s default.
func TestResolveServerModeUOWTopology_ProxyNeverIdlesOut(t *testing.T) {
	beadsDir := serverModeBeadsDir(t, &configfile.Config{
		DoltServerHost: "127.0.0.1",
		DoltServerPort: 3521,
		DoltDatabase:   "beads_serve",
	})

	topology, err := resolveServerModeUOWTopology(context.Background(), beadsDir)
	require.NoError(t, err)

	assert.Equal(t, proxy.IdleTimeoutNever, topology.proxyIdle,
		"a finite idle timeout lets the proxy exit during a quiet period and strands serve on a dead port")
	// A zero timeout is the specific value that reads as "use the default"
	// downstream, so pin that it is not what we send.
	assert.NotZero(t, topology.proxyIdle,
		"zero is the sentinel NewExternalDoltServerUOWProvider replaces with the 30s default")
}
