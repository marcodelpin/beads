package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/doltserver"
	"github.com/steveyegge/beads/internal/storage/dbproxy/proxy"
	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/storage/uow"
)

// sqlServerUOWTopology is everything the two unit-of-work providers need that
// differs between workspaces: which database, whose schema, where the proxy
// listens, and — when the Dolt SQL server is not one Beads starts itself —
// how to reach it.
//
// It exists so the two ways a workspace can describe its SQL server (the
// proxied-server sidecar, and server mode's Dolt connection settings) meet in
// one shape before anything is constructed. The construction below is then
// identical for both, which is the point: a proxied workspace and a server-mode
// workspace differ in where the answer is written down, not in what is built.
type sqlServerUOWTopology struct {
	database     string
	teamServer   bool
	proxyPort    int
	proxyIdle    time.Duration
	external     *configfile.ExternalDoltConfig
	rootPassword string
}

func newProxiedServerUOWProvider(ctx context.Context, beadsDir, databaseOverride string) (uow.UnitOfWorkProvider, error) {
	if beadsDir == "" {
		return nil, fmt.Errorf("newProxiedServerUOWProvider: beadsDir must be set")
	}
	return newSQLServerUOWProvider(ctx, beadsDir, resolveProxiedServerUOWTopology(beadsDir, databaseOverride))
}

// newServerModeUOWProvider builds the provider for a workspace whose Dolt SQL
// server Beads does not front with a proxy of its own: `bd init --server`
// (whether the server is one Beads auto-starts or one the operator runs),
// and shared-server mode.
//
// Such a workspace has no proxied-server sidecar to read the topology from, so
// it comes from the same Dolt connection settings the CLI's store open uses.
// The server is ALWAYS described as external, including when Beads started it:
// the local-server provider would spawn a second `dolt sql-server` on the data
// directory the first one already holds an exclusive write lock on, which Dolt
// refuses outright ("database is locked by another dolt process"). One server,
// fronted — never a second one.
func newServerModeUOWProvider(ctx context.Context, beadsDir string) (uow.UnitOfWorkProvider, error) {
	if beadsDir == "" {
		return nil, fmt.Errorf("newServerModeUOWProvider: beadsDir must be set")
	}
	topology, err := resolveServerModeUOWTopology(ctx, beadsDir)
	if err != nil {
		return nil, err
	}
	return newSQLServerUOWProvider(ctx, beadsDir, topology)
}

func newSQLServerUOWProvider(ctx context.Context, beadsDir string, topology sqlServerUOWTopology) (uow.UnitOfWorkProvider, error) {
	if topology.external != nil {
		return newExternalProxiedServerUOWProvider(ctx, beadsDir, topology)
	}
	return newManagedProxiedServerUOWProvider(ctx, beadsDir, topology)
}

// resolveProxiedServerUOWTopology reads a proxied-server workspace's topology
// out of metadata.json and the proxied-server sidecar. Neither read is fatal:
// an absent or unreadable one leaves the defaults, which is the behavior every
// proxied command has had, and the provider construction below is where a
// workspace that cannot be reached actually fails.
func resolveProxiedServerUOWTopology(beadsDir, databaseOverride string) sqlServerUOWTopology {
	persisted, _ := configfile.Load(beadsDir)
	topology := sqlServerUOWTopology{database: configfile.DefaultDoltDatabase}
	if persisted != nil {
		topology.database = persisted.GetDoltDatabase()
		topology.teamServer = persisted.IsTeamServerManaged()
	}
	if databaseOverride != "" {
		topology.database = databaseOverride
	}

	info, _ := configfile.LoadProxiedServerClientInfo(beadsDir)
	if info != nil {
		topology.proxyPort = info.Port
		topology.proxyIdle = info.IdleTimeout
		if info.External != nil {
			topology.external = info.External
			topology.rootPassword = os.Getenv(configfile.ExternalDoltPasswordEnvVar)
		}
	}
	return topology
}

// resolveServerModeUOWTopology reads a server-mode workspace's Dolt SQL server
// out of its configuration, in the shape the external provider takes.
//
// The connection itself is resolved by resolveDoltServerConnection, the same
// function the CLI's store open goes through, so the HTTP server and a CLI
// command in this workspace reach the same server as the same identity.
func resolveServerModeUOWTopology(ctx context.Context, beadsDir string) (sqlServerUOWTopology, error) {
	fileCfg, err := configfile.Load(beadsDir)
	if err != nil {
		return sqlServerUOWTopology{}, fmt.Errorf("load %s: %w", configfile.ConfigPath(beadsDir), err)
	}
	if fileCfg == nil {
		// No metadata.json at all. The store open in this same process takes the
		// defaults and carries on (shared-server mode reaches exactly that), so
		// refusing here would refuse a workspace the CLI just opened.
		fileCfg = configfile.DefaultConfig()
	}

	conn := &dolt.Config{BeadsDir: beadsDir, ServerMode: true}
	if err := resolveDoltServerConnection(ctx, beadsDir, fileCfg, conn); err != nil {
		return sqlServerUOWTopology{}, err
	}
	if conn.Gateway {
		// The credential command mints a SHORT-LIVED token that is presented as
		// the connection username. A command resolves it once and exits; a
		// server would pin that token for its whole lifetime and start failing
		// authentication at an hour nobody is watching. Refuse while it says so.
		return sqlServerUOWTopology{}, fmt.Errorf(
			"this workspace authenticates to Dolt with a credential command (BEADS_DOLT_CREDENTIAL_COMMAND), " +
				"which mints a short-lived token; bd serve holds one connection identity for the life of the " +
				"process and cannot refresh it")
	}

	external := &configfile.ExternalDoltConfig{User: conn.ServerUser}
	switch {
	case conn.ServerSocket != "":
		external.Socket = conn.ServerSocket
	case conn.ServerPort > 0:
		external.Host = conn.ServerHost
		external.Port = conn.ServerPort
	default:
		// Port 0 means no source resolved one — for a Beads-managed server that
		// is the case where nothing has started it yet, and the port file it
		// would write does not exist.
		return sqlServerUOWTopology{}, fmt.Errorf(
			"cannot determine the Dolt server port for %s; start the server with `bd dolt start`, "+
				"or set the port explicitly (BEADS_DOLT_SERVER_PORT or dolt.port in config.yaml)", beadsDir)
	}
	external.TLSRequired = conn.ServerTLS

	database := fileCfg.GetDoltDatabase()
	// --global selects the shared-server global database, exactly as it does for
	// the store the CLI opens (main.go rejects the flag outside shared-server
	// mode); without this the HTTP surface would answer from the project
	// database while the CLI in the same process answered from the global one.
	if globalFlag && doltserver.IsSharedServerMode() {
		database = doltserver.GlobalDatabaseName
	}

	return sqlServerUOWTopology{
		database:     database,
		external:     external,
		rootPassword: conn.ServerPassword,
	}, nil
}

// newExternalProxiedServerUOWProvider fronts a Dolt SQL server this process
// neither started nor owns. "Proxied" in the name is the PROXY it builds, not
// the workspace mode: since bd-emv a server-mode workspace lands here too, and
// the paths it resolves (root, log) are the same ones proxied mode uses because
// both modes root their server at the same directory.
func newExternalProxiedServerUOWProvider(ctx context.Context, beadsDir string, topology sqlServerUOWTopology) (uow.UnitOfWorkProvider, error) {
	rootPath, err := resolveProxiedServerRootPath(beadsDir)
	if err != nil {
		return nil, fmt.Errorf("newExternalProxiedServerUOWProvider: resolve root path: %w", err)
	}
	if err := validateProxiedServerRootPath(rootPath); err != nil {
		return nil, fmt.Errorf("newExternalProxiedServerUOWProvider: proxied server root (from env or %s): %w", configfile.ProxiedServerClientInfoFileName, err)
	}

	logPath, isCustomLog, err := resolveProxiedServerLogPath(beadsDir)
	if err != nil {
		return nil, fmt.Errorf("newExternalProxiedServerUOWProvider: resolve log path: %w", err)
	}
	if isCustomLog {
		if err := validateProxiedServerLogPath(logPath); err != nil {
			return nil, fmt.Errorf("newExternalProxiedServerUOWProvider: proxied server log (from env or %s): %w", configfile.ProxiedServerClientInfoFileName, err)
		}
	}

	if err := os.MkdirAll(rootPath, config.BeadsDirPerm); err != nil {
		return nil, fmt.Errorf("newExternalProxiedServerUOWProvider: mkdir %s: %w", rootPath, err)
	}

	return uow.NewExternalDoltServerUOWProvider(
		ctx,
		rootPath,
		topology.database,
		logPath,
		*topology.external,
		topology.external.ResolvedUser(),
		topology.rootPassword,
		topology.proxyPort,
		topology.proxyIdle,
		topology.teamServer,
	)
}

func newManagedProxiedServerUOWProvider(ctx context.Context, beadsDir string, topology sqlServerUOWTopology) (uow.UnitOfWorkProvider, error) {
	doltBin, err := exec.LookPath("dolt")
	if err != nil {
		return nil, fmt.Errorf("newProxiedServerUOWProvider: dolt is not installed (not found in PATH); install from https://docs.dolthub.com/introduction/installation: %w", err)
	}

	rootPath, err := resolveProxiedServerRootPath(beadsDir)
	if err != nil {
		return nil, fmt.Errorf("newProxiedServerUOWProvider: resolve root path: %w", err)
	}
	if err := validateProxiedServerRootPath(rootPath); err != nil {
		return nil, fmt.Errorf("newProxiedServerUOWProvider: proxied server root (from env or %s): %w", configfile.ProxiedServerClientInfoFileName, err)
	}

	// Gate auto_gc_behavior.archive_level: 0 on the resolved external dolt's
	// version — Dolt's YAML config loader uses yaml.UnmarshalStrict, so an
	// older dolt whose own YAMLConfig struct lacks this field would refuse
	// to start rather than ignore the unknown key (gastownhall/beads#4986).
	archiveLevelSupported := doltserver.SupportsArchiveLevelConfig(doltBin)

	configPath, err := ensureProxiedServerConfig(beadsDir, archiveLevelSupported)
	if err != nil {
		return nil, err
	}

	logPath, isCustomLog, err := resolveProxiedServerLogPath(beadsDir)
	if err != nil {
		return nil, fmt.Errorf("newProxiedServerUOWProvider: resolve log path: %w", err)
	}
	if isCustomLog {
		if err := validateProxiedServerLogPath(logPath); err != nil {
			return nil, fmt.Errorf("newProxiedServerUOWProvider: proxied server log (from env or %s): %w", configfile.ProxiedServerClientInfoFileName, err)
		}
	}

	return uow.NewDoltServerUOWProvider(
		ctx,
		rootPath,
		topology.database,
		logPath,
		configPath,
		proxy.BackendLocalServer,
		"root",
		"", // proxy is loopback-only, no auth
		doltBin,
		topology.proxyPort,
		topology.proxyIdle,
		topology.teamServer,
	)
}
