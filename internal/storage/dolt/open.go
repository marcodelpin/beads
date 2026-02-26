package dolt

import (
	"context"
	"fmt"
	"os"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/doltserver"
)

// NewFromConfig creates a DoltStore based on the metadata.json configuration.
// beadsDir is the path to the .beads directory.
func NewFromConfig(ctx context.Context, beadsDir string) (*DoltStore, error) {
	return NewFromConfigWithOptions(ctx, beadsDir, nil)
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

	// Build config from metadata.json, allowing overrides from caller
	if cfg == nil {
		cfg = &Config{}
	}
	cfg.Path = fileCfg.DatabasePath(beadsDir)

	// Always apply database name from metadata.json (prefix-based naming, bd-u8rda).
	if cfg.Database == "" {
		cfg.Database = fileCfg.GetDoltDatabase()
	}

	// Merge server connection config (config provides defaults, caller can override)
	if fileCfg.IsDoltServerMode() {
		if cfg.ServerHost == "" {
			cfg.ServerHost = fileCfg.GetDoltServerHost()
		}
		if cfg.ServerPort == 0 {
			cfg.ServerPort = fileCfg.GetDoltServerPort()
		}
		if cfg.ServerUser == "" {
			cfg.ServerUser = fileCfg.GetDoltServerUser()
		}
	}

	// Enable auto-start for standalone users (same logic as main.go).
	// Disabled under Gas Town (which manages its own server), by explicit config,
	// or in test mode (tests manage their own server lifecycle via testdoltserver).
	// Note: cfg.ReadOnly refers to the store's read-only mode, not the server —
	// the server must be running regardless of whether the store is read-only.
	if os.Getenv("BEADS_TEST_MODE") != "1" {
		cfg.AutoStart = true
		if doltserver.IsDaemonManaged() {
			cfg.AutoStart = false
		}
		if os.Getenv("BEADS_DOLT_AUTO_START") == "0" {
			cfg.AutoStart = false
		}
	}

	return New(ctx, cfg)
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
