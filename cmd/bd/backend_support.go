package main

import (
	"fmt"

	"github.com/steveyegge/beads/internal/configfile"
)

func removedBackendError(backend string) error {
	return fmt.Errorf("storage backend %q is no longer supported: direct support for general-purpose server databases was rolled back to keep Beads simple and resource-light; the configured %s database was not opened or modified; export it with a bd version that supports %s, then follow bd help init-safety to reinitialize with Dolt or SQLite and import the exported data", backend, backend, backend)
}

func unknownBackendError(backend string) error {
	return fmt.Errorf("storage backend %q in metadata.json is not recognized or supported; no storage database was opened or modified; supported backends are \"dolt\" and \"sqlite\"; fix or restore metadata.json and retry", backend)
}

func validateConfiguredBackend(cfg *configfile.Config) error {
	if cfg == nil {
		return nil
	}
	switch cfg.Backend {
	case configfile.BackendPostgres, configfile.BackendMySQL:
		return removedBackendError(cfg.Backend)
	case "", configfile.BackendDolt, configfile.BackendSQLite:
		return nil
	default:
		return unknownBackendError(cfg.Backend)
	}
}

func requireDoltBackend(cfg *configfile.Config) error {
	if err := validateConfiguredBackend(cfg); err != nil {
		return err
	}
	if cfg != nil && cfg.GetBackend() != configfile.BackendDolt {
		return fmt.Errorf("not using Dolt backend (configured backend %q)", cfg.GetBackend())
	}
	return nil
}

func loadDoltBackendConfig(beadsDir string) (*configfile.Config, error) {
	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	if cfg == nil {
		cfg = configfile.DefaultConfig()
	}
	if err := requireDoltBackend(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
