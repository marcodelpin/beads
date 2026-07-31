package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage"
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

// The gateway refusal must be decided from the CONFIGURATION, never from the
// result of running the credential command. Reaching it any other way means
// every refused `bd serve` has already spawned the operator's command and
// minted a short-lived identity token it is about to throw away.
//
// The command here fails loudly if it ever runs: without the fix the test sees
// that failure instead of the refusal.
func TestResolveServerModeUOWTopology_RefusesGatewayWithoutRunningTheCommand(t *testing.T) {
	beadsDir := serverModeBeadsDir(t, &configfile.Config{
		DoltServerHost: "127.0.0.1",
		DoltServerPort: 3521,
		DoltDatabase:   "beads_serve",
	})
	t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "exit 17")

	_, err := resolveServerModeUOWTopology(context.Background(), beadsDir)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "BEADS_DOLT_CREDENTIAL_COMMAND",
		"the refusal has to name what to change")
	assert.Contains(t, err.Error(), "short-lived")
	assert.NotContains(t, err.Error(), "resolving dolt credential command",
		"that wrapper means the command was executed before the refusal was decided")
	assert.NotContains(t, err.Error(), "17",
		"the command's own exit status can only appear if the command ran")
}

// The refusal names the knob to change and what serve cannot do, and stays a
// plain error: storage.ErrUnsupported carries a BACKEND, and typing this one
// would tell a caller that bd serve does not support dolt.
func TestErrServeGatewayCredential_SaysWhatItCannotDo(t *testing.T) {
	err := errServeGatewayCredential()
	require.Error(t, err)

	var unsupported *storage.ErrUnsupported
	assert.False(t, errors.As(err, &unsupported),
		"this is not a backend limitation: bd serve supports dolt, it cannot hold a refreshing identity")
	assert.Contains(t, err.Error(), "BEADS_DOLT_CREDENTIAL_COMMAND")
	assert.Contains(t, err.Error(), "cannot refresh it")
}
