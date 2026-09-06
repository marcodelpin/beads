package configfile

import "fmt"

// StorageMode is which dolt store an open of a workspace goes to. It is the
// STORAGE question -- "where do this workspace's issues live" -- and it is
// answered here, once, so that every caller that has to route an open reaches
// the same conclusion.
//
// It is deliberately NOT the same question as doltserver.ResolveServerMode,
// which answers the LIFECYCLE one: who is responsible for starting the server
// (beads itself, something external, or nobody because the store is embedded).
// The two overlap but cannot be substituted for one another. A workspace whose
// metadata.json carries no dolt_mode, no configured host and no explicit port
// is StorageModeEmbedded here -- the store factory opens it with the embedded
// backend -- while ResolveServerMode reports ServerModeOwned, meaning "if there
// were a server, beads would own it". Reading a lifecycle answer as a storage
// answer is how an embedded workspace ends up on a server code path.
type StorageMode int

const (
	// StorageModeEmbedded is the in-process store under <beadsDir>/embeddeddolt.
	// It is the default: a workspace has to say something to get anything else.
	StorageModeEmbedded StorageMode = iota
	// StorageModeServer is a dolt sql-server reached over the MySQL protocol.
	StorageModeServer
	// StorageModeProxiedServer is a server reached through the beads proxy,
	// which owns its own connection details and lifecycle.
	StorageModeProxiedServer
)

func (m StorageMode) String() string {
	switch m {
	case StorageModeServer:
		return "server"
	case StorageModeProxiedServer:
		return "proxied-server"
	case StorageModeEmbedded:
		return "embedded"
	}
	return fmt.Sprintf("StorageMode(%d)", int(m))
}

// StorageMode reports which store this configuration selects.
//
// The order matters and matches the store factory's: proxied-server is checked
// first because IsDoltServerMode and IsDoltProxiedServerMode are mutually
// exclusive by construction and the proxied arm has its own opener.
//
// A nil receiver is the "no metadata.json" case, which resolves exactly as an
// empty configuration does -- the env vars, the persisted host and config.yaml
// still get their say through IsDoltServerMode, and only then does it fall
// through to embedded.
func (c *Config) StorageMode() StorageMode {
	if c == nil {
		c = DefaultConfig()
	}
	if c.IsDoltProxiedServerMode() {
		return StorageModeProxiedServer
	}
	if c.IsDoltServerMode() {
		return StorageModeServer
	}
	return StorageModeEmbedded
}

// ResolveStorageMode reports the storage mode of the workspace rooted at
// beadsDir, for callers that hold a directory rather than a loaded
// configuration.
//
// An unreadable metadata.json is returned as an error rather than as a mode:
// the caller that is about to OPEN it will refuse the open anyway (a
// present-but-unloadable metadata.json is a hard error, never a silent
// embedded fallback), and a caller merely asking about the workspace must be
// able to tell "embedded" from "I could not tell".
func ResolveStorageMode(beadsDir string) (StorageMode, error) {
	cfg, err := Load(beadsDir)
	if err != nil {
		return StorageModeEmbedded, fmt.Errorf("load %s: %w", ConfigPath(beadsDir), err)
	}
	return cfg.StorageMode(), nil
}
