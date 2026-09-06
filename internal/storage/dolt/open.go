package dolt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/doltserver"
)

// doltOpenRetryBudgetKey is the config.yaml key holding the open-retry
// budget: how long a failing pre-dial probe against a dolt sql-server bd does
// not manage may keep retrying before the open fails (GH#4379).
//
// Grammar (parseTimeout): a Go duration string ("30s", "2m", "1m30s") or a
// bare number read as seconds ("30"). Absent, empty, "0" or unparseable all
// mean OFF -- one dial attempt, today's fail-fast error.
const doltOpenRetryBudgetKey = "dolt.open-retry-budget"

// ServerMode is re-exported from doltserver for convenience.
type ServerMode = doltserver.ServerMode

// Re-export ServerMode constants for callers that import storage/dolt.
const (
	ServerModeOwned    = doltserver.ServerModeOwned
	ServerModeExternal = doltserver.ServerModeExternal
	ServerModeEmbedded = doltserver.ServerModeEmbedded
)

// ApplyCLIAutoStart sets the standalone auto-start policy used by the
// normal CLI path. Honors the actual server mode resolved from
// metadata.json + env: when External (e.g. metadata.json has explicit
// dolt_server_port), suppresses fallback auto-spawn — the user has
// configured an external server; if it's transiently unreachable, bd
// errors out rather than silently spawning a different server from
// .beads/dolt/ (the shadow database bug).
//
// Cold standalone setups remain unaffected: bd init writes the port and
// starts the server in one shot, so subsequent commands find a running
// server and don't need fallback auto-start. If the user explicitly
// stops the server, External mode's "you manage the lifecycle" semantics
// asks the user to run `bd dolt start`.
func ApplyCLIAutoStart(beadsDir string, cfg *Config) {
	if cfg.DisableAutoStart {
		cfg.AutoStart = false
		return
	}
	autoStartCfg := config.GetString("dolt.auto-start")
	if autoStartCfg == "" {
		autoStartCfg = config.GetStringFromDir(beadsDir, "dolt.auto-start")
	}
	mode := doltserver.ResolveServerMode(beadsDir)
	cfg.AutoStart = resolveAutoStart(true, autoStartCfg, mode)
}

// ApplyOpenRetryConfigDir records which project a HAND-BUILT Config is about,
// for the sake of dolt.open-retry-budget resolution alone.
//
// It is the third of the Apply*(beadsDir, cfg) helpers a caller that does not
// go through applyResolvedConfig needs, and it exists as its own helper rather
// than as "just set cfg.BeadsDir" because BeadsDir is read by several other
// things -- project-identity verification, local-data-dir resolution, the
// auto-started-server bookkeeping -- and a caller that deliberately leaves it
// empty is deliberately opting out of those. Setting only this field changes
// nothing except which directory the budget is read from.
//
// Without it, a Config whose data path is outside the project (a custom
// absolute dolt_data_dir, shared-server mode) has no directory to resolve
// from, and the budget falls back on the user-level default -- so a project's
// explicit "0" would not be honoured for that open.
func ApplyOpenRetryConfigDir(beadsDir string, cfg *Config) {
	if cfg == nil {
		return
	}
	cfg.OpenRetryConfigDir = beadsDir
}

// ApplyResolvedServerPort fills cfg's server port from doltserver's
// precedence chain (env > port file > dolt config.yaml > beads config.yaml >
// metadata.json), and — the part that is easy to drop — records WHICH of those
// produced it.
//
// The source is not decoration. applyConfigDefaults infers "the caller
// explicitly asserted this port" from "ServerPort nonzero, ServerPortSource
// unset" (be-wf9a.1). A site that copies doltserver.DefaultConfig(dir).Port on
// its own therefore launders bd's own bookkeeping — the gitignored port file,
// or the shared-server default 3308 — into a deliberate user assertion, and
// that mislabel has two visible consequences:
//
//   - the legacy BEADS_DOLT_PORT override becomes a silent no-op, because an
//     authoritative source outranks the env read (be-9tju); and
//   - auto-start's benign "configured port is stale, here is the new one"
//     retarget turns into a hard failure telling the user to fix a port they
//     never configured (GH#4052).
//
// So every hand-built dolt.Config that resolves its port this way goes through
// here rather than reaching for .Port. Callers that want to resolve only when
// unset keep their own `if cfg.ServerPort == 0` guard; this function is
// unconditional.
func ApplyResolvedServerPort(beadsDir string, cfg *Config) {
	resolved := doltserver.DefaultConfig(beadsDir)
	cfg.ServerPort = resolved.Port
	cfg.ServerPortSource = resolved.PortSource
	cfg.ServerPortSharedServer = resolved.PortSharedServer
}

// requireDoltBackend keeps metadata-driven callers from bypassing the storage
// factory and interpreting another backend's workspace as Dolt. Removed backend
// identifiers deliberately remain recognizable in metadata so this check can fail
// closed instead of opening a new, empty Dolt database.
func requireDoltBackend(fileCfg *configfile.Config) error {
	switch fileCfg.Backend {
	case configfile.BackendPostgres, configfile.BackendMySQL, configfile.BackendSQLite:
		return fmt.Errorf("configured storage backend %q is no longer supported and cannot be opened as Dolt: %s", fileCfg.Backend, configfile.RemovedBackendDetail(fileCfg.Backend))
	}
	if !configfile.IsSupportedBackend(fileCfg.Backend) {
		return fmt.Errorf("configured storage backend %q in metadata.json is not recognized and cannot be opened as Dolt; %s", fileCfg.Backend, configfile.BackendNotOpenedGuarantee)
	}
	backend := fileCfg.GetBackend()
	if backend != configfile.BackendDolt {
		return fmt.Errorf("configured storage backend %q cannot be opened as Dolt", backend)
	}
	return nil
}

// NewFromConfig creates a DoltStore based on the metadata.json configuration.
// beadsDir is the path to the .beads directory.
func NewFromConfig(ctx context.Context, beadsDir string) (*DoltStore, error) {
	return NewFromConfigWithOptions(ctx, beadsDir, nil)
}

// NewFromConfigWithCLIOptions creates a DoltStore using the standalone CLI
// auto-start policy from cmd/bd/main.go. This is for CLI helper paths like
// `bd doctor` that should behave the same way as normal top-level CLI commands
// while still honoring externally managed server mode.
func NewFromConfigWithCLIOptions(ctx context.Context, beadsDir string, cfg *Config) (*DoltStore, error) {
	fileCfg, err := configfile.Load(beadsDir)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	if fileCfg == nil {
		fileCfg = configfile.DefaultConfig()
	}
	if err := requireDoltBackend(fileCfg); err != nil {
		return nil, err
	}

	// Apply central server config as defaults for any server fields not
	// set in the per-project metadata.json. This eliminates the need to
	// duplicate host/port/user across 30+ project configs.
	applyCentralConfigDefaults(fileCfg)

	if cfg == nil {
		cfg = &Config{}
	}
	if err := applyResolvedConfig(ctx, beadsDir, fileCfg, cfg); err != nil {
		return nil, err
	}
	ApplyCLIAutoStart(beadsDir, cfg)

	return New(ctx, cfg)
}

// NewFromConfigWithOptions creates a DoltStore with options from metadata.json.
// Options in cfg override those from the config file. Pass nil for default options.
func NewFromConfigWithOptions(ctx context.Context, beadsDir string, cfg *Config) (*DoltStore, error) {
	fileCfg, err := configfile.Load(beadsDir)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	if fileCfg == nil {
		fileCfg = configfile.DefaultConfig()
	}
	if err := requireDoltBackend(fileCfg); err != nil {
		return nil, err
	}

	// Apply central server config as defaults for any server fields not
	// set in the per-project metadata.json.
	applyCentralConfigDefaults(fileCfg)

	// Build config from metadata.json, allowing overrides from caller
	if cfg == nil {
		cfg = &Config{}
	}
	if err := applyResolvedConfig(ctx, beadsDir, fileCfg, cfg); err != nil {
		return nil, err
	}

	// Enable auto-start for standalone users (similar to main.go's auto-start
	// handling), with additional support for BEADS_TEST_MODE and a config.yaml
	// fallback for library consumers that never call config.Initialize().
	// Disabled under orchestrator (which manages its own server), by explicit config,
	// or in test mode (tests manage their own server lifecycle via testdoltserver).
	// Note: cfg.ReadOnly refers to the store's read-only mode, not the server —
	// the server must be running regardless of whether the store is read-only.
	//
	// Prefer the global viper config (populated when config.Initialize() has been
	// called, i.e. all CLI paths). Fall back to a direct read of the project
	// config.yaml for library consumers that never call config.Initialize().
	autoStartCfg := config.GetString("dolt.auto-start")
	if autoStartCfg == "" {
		autoStartCfg = config.GetStringFromDir(beadsDir, "dolt.auto-start")
	}
	// When the server is externally managed (explicit port in metadata.json,
	// shared server mode, etc.), suppress auto-start. This prevents bd from
	// launching a different server when the user's configured server is
	// temporarily unreachable — the root cause of the shadow database bug.
	mode := doltserver.ResolveServerMode(beadsDir)
	if cfg.DisableAutoStart {
		cfg.AutoStart = false
	} else {
		cfg.AutoStart = resolveAutoStart(cfg.AutoStart, autoStartCfg, mode)
	}

	return New(ctx, cfg)
}

// resolveAutoStart computes the effective AutoStart value, respecting a
// caller-provided value (current) while applying system-level overrides.
//
// Priority (highest to lowest):
//  1. BEADS_TEST_MODE=1                    → always false (tests own the server lifecycle)
//  2. BEADS_DOLT_AUTO_START=0              → always false (explicit env opt-out)
//  3. mode == ServerModeExternal           → always false (server is externally managed;
//     auto-starting a different server would create shadow databases)
//  4. doltAutoStartCfg == "false"/"0"/"off" → false (config.yaml explicit opt-out;
//     user intent to disable auto-start must be respected even when callers
//     like ApplyCLIAutoStart or bootstrap pass current=true — GH#autostart-bug)
//  5. current == true                      → true  (caller option wins over default)
//  6. default                              → true  (standalone user; safe default)
//
// doltAutoStartCfg is the raw value of the "dolt.auto-start" key from config.yaml
// (pass config.GetString("dolt.auto-start") at the call site).
//
// Note: because AutoStart is a plain bool, a zero value (false) cannot be
// distinguished from an explicit "opt-out" by the caller.  Callers that need
// to suppress auto-start should use one of the environment-variable or
// config-file overrides above.
func resolveAutoStart(current bool, doltAutoStartCfg string, mode ServerMode) bool {
	if os.Getenv("BEADS_TEST_MODE") == "1" {
		return false
	}
	if os.Getenv("BEADS_DOLT_AUTO_START") == "0" {
		return false
	}
	// When the server is externally managed, never auto-start.
	// The user has configured a specific server — if it's down, error out
	// rather than silently starting a different server from .beads/dolt/.
	if mode == ServerModeExternal {
		return false
	}
	// Config.yaml explicit opt-out takes precedence over caller-provided
	// current=true. Without this, ApplyCLIAutoStart (which passes current=true)
	// and bootstrap paths (which hardcode AutoStart=true) would ignore the
	// user's dolt.auto-start: false setting, spawning rogue dolt servers that
	// overwrite port files and cause DB lock conflicts.
	if strings.EqualFold(doltAutoStartCfg, "false") || doltAutoStartCfg == "0" || strings.EqualFold(doltAutoStartCfg, "off") {
		return false
	}
	// Caller option wins over default.
	if current {
		return true
	}
	// Default: auto-start for standalone users.
	return true
}

// GetBackendFromConfig returns the backend type from metadata.json.
// Returns "dolt" if no config exists or backend is not specified.
func GetBackendFromConfig(beadsDir string) string {
	cfg, err := configfile.Load(beadsDir)
	if err != nil || cfg == nil {
		return configfile.BackendDolt
	}
	return cfg.GetBackend()
}

// applyResolvedConfig merges metadata.json-derived defaults into a store config.
// Server connection fields are always populated because the storage layer is
// server-backed even when older metadata.json files omit dolt_mode. It returns an
// error only when a configured server credential command fails (fail-closed).
func applyResolvedConfig(ctx context.Context, beadsDir string, fileCfg *configfile.Config, cfg *Config) error {
	cfg.Path = fileCfg.DatabasePath(beadsDir)
	// OpenRetryConfigDir tracks this open the way Path does -- rewritten
	// unconditionally, every time -- because it is the ONE authoritative
	// answer to "which .beads directory is this open about". BeadsDir is a
	// documented caller override filled in only when empty, so on a reused
	// Config it still holds the previous project; Path is not it either,
	// since DatabasePath returns a custom absolute dolt_data_dir and the
	// shared-server data directory, neither of which sits inside this
	// project's .beads. See openRetryBeadsDir.
	cfg.OpenRetryConfigDir = beadsDir
	if cfg.BeadsDir == "" {
		cfg.BeadsDir = beadsDir
	}

	// GH#2438: Warn if data-dir is set in server mode — it has no effect on
	// which database the server uses and can cause silent DB context switches.
	if fileCfg.DoltDataDir != "" && fileCfg.IsDoltServerMode() {
		fmt.Fprintf(os.Stderr, "Warning: dolt_data_dir is set (%s) but Dolt is in server mode.\n", fileCfg.DoltDataDir)
		fmt.Fprintf(os.Stderr, "In server mode, data-dir does not control which database is used.\n")
		fmt.Fprintf(os.Stderr, "This may cause commands to operate on the wrong database.\n")
		fmt.Fprintf(os.Stderr, "Fix: bd dolt set data-dir ''   (clear the data-dir setting)\n\n")
	}

	// Always apply database name from metadata.json (prefix-based naming, bd-u8rda).
	if cfg.Database == "" {
		cfg.Database = fileCfg.GetDoltDatabase()
	}

	if cfg.ServerHost == "" {
		cfg.ServerHost = fileCfg.GetDoltServerHost()
	}
	if cfg.ServerPort == 0 {
		// fileCfg.GetDoltServerPort() falls back to 3307, which is wrong for
		// standalone repos; ApplyResolvedServerPort walks doltserver's real
		// precedence chain and carries the source along with the port.
		ApplyResolvedServerPort(beadsDir, cfg)
	}
	// Resolve the server-mode credential (the connection username). In server mode a
	// configured credential command takes precedence over the static user; it fails
	// closed (see ApplyGatewayCredential). The command runs only in server mode — an
	// embedded store never presents a username, so a command exported in the environment
	// must not run (or fail) an embedded open. The server-mode test mirrors main.go's
	// (metadata dolt_mode=server, or shared-server via env/config.yaml) so both the CLI
	// and library/doctor open paths agree on whether to run the command.
	applied := false
	if fileCfg.IsDoltServerMode() || doltserver.IsSharedServerMode() {
		got, err := ApplyGatewayCredential(ctx, fileCfg, cfg)
		if err != nil {
			return fmt.Errorf("resolving dolt credential command: %w", err)
		}
		applied = got
	}
	if !applied && cfg.ServerUser == "" {
		cfg.ServerUser = fileCfg.GetDoltServerUser()
	}
	// Populate password and TLS the same way the CLI CRUD path does. Without
	// this, callers that rely on NewFromConfigWithOptions (e.g. doctor's
	// SharedStore) fail to reach externally-hosted Dolt servers that keep
	// credentials in ~/.config/beads/credentials, while bd create/list/close
	// succeed (bd-h5k7). GetDoltServerPasswordForPort checks BEADS_DOLT_PASSWORD
	// env first, then credentials file keyed by [host:resolved-port].
	if cfg.ServerPassword == "" {
		cfg.ServerPassword = fileCfg.GetDoltServerPasswordForPort(cfg.ServerPort)
	}
	if !cfg.ServerTLS {
		cfg.ServerTLS = fileCfg.GetDoltServerTLS()
	}

	// Pool size: env var > config.yaml > caller override > default (10).
	// Useful for shared-server setups with many worktrees (GH#3140).
	if cfg.MaxOpenConns == 0 {
		if v := os.Getenv("BEADS_DOLT_MAX_CONNS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.MaxOpenConns = n
			}
		}
	}
	if cfg.MaxOpenConns == 0 {
		if v := config.GetString("dolt.max-conns"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.MaxOpenConns = n
			}
		}
	}

	// Pool per-I/O deadlines: caller override > env var > config.yaml > default
	// (10s, see buildServerDSN). The default fast-fail is right for healthy
	// local servers; overloaded shared-server deployments raise it so ordinary
	// queries stop dying with "i/o timeout" under load (bd-vz0y9).
	if cfg.PoolReadTimeout == 0 {
		cfg.PoolReadTimeout = timeoutFromEnv("BEADS_DOLT_POOL_READ_TIMEOUT", 0)
	}
	if cfg.PoolReadTimeout == 0 {
		cfg.PoolReadTimeout = parseTimeout(config.GetString("dolt.pool-read-timeout"), 0)
	}
	if cfg.PoolWriteTimeout == 0 {
		cfg.PoolWriteTimeout = timeoutFromEnv("BEADS_DOLT_POOL_WRITE_TIMEOUT", 0)
	}
	if cfg.PoolWriteTimeout == 0 {
		cfg.PoolWriteTimeout = parseTimeout(config.GetString("dolt.pool-write-timeout"), 0)
	}

	return nil
}

// openRetryBeadsDir returns the .beads directory whose config.yaml governs
// THIS open, or "" when no directory can be established -- in which case
// resolveOpenRetryBudget consults the user-level default only, and never some
// other project's settings.
//
// Precedence, and why each rung is where it is:
//
//  1. cfg.OpenRetryConfigDir -- what applyResolvedConfig was called with, set
//     unconditionally on every open, and what a hand-built Config sets through
//     ApplyOpenRetryConfigDir. Authoritative when present.
//  2. cfg.BeadsDir -- the caller override, which the direct dolt.New callers
//     (the CLI root pre-run, bootstrap, the migration planning store) set to
//     the project directory while setting Path to doltserver.ResolveDoltDir,
//     a path that in shared-server mode is ~/.beads/shared-server/dolt.
//  3. cfg.Path, ONLY when it sits directly inside a .beads directory.
//     Callers that set neither directory field leave this as the only signal,
//     and it is a sound one whenever the data lives in the project.
//
// Deriving from Path unconditionally -- which this did -- is wrong for exactly
// the layouts rung 3 excludes: DatabasePath returns a custom absolute
// dolt_data_dir verbatim, and ResolveDoltDir returns the shared-server data
// directory. Their parents are not the project's .beads, so the project's own
// explicit "0" was skipped and a positive user-global budget enabled retries
// it had opted out of (and a project-only budget was ignored).
func openRetryBeadsDir(cfg *Config) string {
	if cfg == nil {
		return ""
	}
	if cfg.OpenRetryConfigDir != "" {
		return cfg.OpenRetryConfigDir
	}
	if cfg.BeadsDir != "" {
		return cfg.BeadsDir
	}
	if isProjectDoltDataPath(cfg.Path) {
		return filepath.Dir(cfg.Path)
	}
	return ""
}

// isProjectDoltDataPath reports whether path sits DIRECTLY inside a .beads
// directory, the condition under which the data path's parent IS the project
// config directory. It holds for every project-rooted layout -- .beads/dolt,
// .beads/embeddeddolt, a relative dolt_data_dir -- and fails for the two that
// leave the project: ~/.beads/shared-server/dolt, whose parent is
// shared-server, and a custom ABSOLUTE dolt_data_dir, whose parent is wherever
// the operator put it. Testing the parent rather than the leaf name is what
// makes that split the one that matters: the leaf says which store, the parent
// says whose.
func isProjectDoltDataPath(path string) bool {
	if path == "" {
		return false
	}
	return filepath.Base(filepath.Dir(path)) == ".beads"
}

// resolveOpenRetryBudget resolves dolt.open-retry-budget for the store rooted
// at beadsDir. Zero -- the default -- keeps the fail-fast pre-dial probe
// exactly as it is; a positive budget lets an open ride out a restart of a
// server bd does not manage instead of failing in tens of milliseconds
// (GH#4379). openRetryEnabled decides which modes honour it.
//
// Precedence, highest first:
//
//  1. an explicit cfg.OpenRetryBudget -- a caller (a test, an embedder) that
//     set the field means it;
//  2. <beadsDir>/config.local.yaml, then <beadsDir>/config.yaml: the project
//     actually being opened, local override first because that is the order
//     config.Initialize merges them in. What decides is the key's PRESENCE,
//     not its value: an explicit "0" (or any value that does not parse to a
//     positive duration) disables the budget even when a wider default sets
//     one, so a workspace can opt out. Both spellings YAML permits are
//     honoured -- the nested "dolt:\n  open-retry-budget: 0" and the flat
//     "dolt.open-retry-budget: 0" -- because config.yaml is hand-edited and an
//     operator who writes the flat form to turn the budget OFF must not
//     silently keep it on. A key present with no value at all reads as unset;
//  3. the USER-level config.yaml, a genuine machine-wide default that every
//     workspace which has not overridden it inherits.
//
// What is deliberately NOT consulted is config.GetString, the process-wide
// merged configuration. That view carries the PROJECT settings of whichever
// workspace initialized the singleton first, so a library or multi-workspace
// process that opened workspace A with a 30s budget would silently give
// workspace B -- which says nothing, and whose operator configured nothing --
// a retrying open. A per-directory setting has to fall back on a machine-wide
// default, never on another directory's.
func resolveOpenRetryBudget(cfg *Config, beadsDir string) time.Duration {
	if cfg == nil {
		return 0
	}
	if cfg.OpenRetryBudget != 0 {
		return cfg.OpenRetryBudget
	}
	// A workspace that DECLARES itself embedded gets no budget, whatever its
	// config.yaml says. openRetryEnabled excludes the modes that have another
	// remedy, but it reasons about the Config, and an embedded workspace can
	// still reach the server open through a caller that does not go through
	// the CLI's store factory -- the version-maintenance probes, which open
	// with NewFromConfig* on whatever workspace the command is in. Those
	// probes were already dialling and already failing; without this they
	// would now WAIT the operator's budget first, on a workspace that has no
	// server for the waiting to help.
	//
	// doltserver.ResolveServerMode is the lifecycle resolver auto-start
	// already uses, so this decision and that one cannot disagree; it reports
	// Embedded only for an explicit dolt_mode="embedded" in metadata.json,
	// never for an absent or host-inferred one.
	if beadsDir != "" && doltserver.ResolveServerMode(beadsDir) == doltserver.ServerModeEmbedded {
		return 0
	}
	// config.WorkspaceYamlValue, not config.GetStringFromDir: the latter only
	// descends nested mappings, so it reports a flat
	// "dolt.open-retry-budget: 0" as absent.
	for _, read := range []func(string, string) (string, bool){
		config.WorkspaceLocalYamlValue,
		config.WorkspaceYamlValue,
	} {
		if beadsDir == "" {
			break
		}
		if raw, present := read(beadsDir, doltOpenRetryBudgetKey); present {
			if trimmed := strings.TrimSpace(raw); trimmed != "" {
				return parseTimeout(trimmed, 0)
			}
		}
	}
	if raw, present := config.UserGlobalYamlValue(doltOpenRetryBudgetKey); present {
		return parseTimeout(strings.TrimSpace(raw), 0)
	}
	return 0
}

// applyCentralConfigDefaults loads the central server config from
// ~/.config/beads/server.json (or BEADS_CENTRAL_CONFIG env var) and
// applies its server fields as defaults to the per-project config.
// A missing central config file is silently ignored.
func applyCentralConfigDefaults(fileCfg *configfile.Config) {
	centralPath := os.Getenv("BEADS_CENTRAL_CONFIG")
	if centralPath == "" {
		centralPath = configfile.DefaultCentralConfigPath()
	}
	if centralPath == "" {
		return
	}

	centralCfg, err := configfile.LoadCentralConfig(centralPath)
	if err != nil {
		// Log but don't fail — a broken central config shouldn't block operations.
		fmt.Fprintf(os.Stderr, "Warning: failed to load central config %s: %v\n", centralPath, err)
		return
	}

	configfile.ApplyCentralDefaults(fileCfg, centralCfg)
}
