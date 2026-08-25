package dolt

import (
	"os"
	"testing"
	"time"
)

// bda-dtgj stage 3: the fork's 120s fallback block (and its BEADS_DOLT_READ_TIMEOUT
// env alias, sys-9np6d) was deleted from buildServerDSN. Pool deadlines now follow
// upstream's contract: cfg.PoolReadTimeout/PoolWriteTimeout (fed by
// BEADS_DOLT_POOL_READ_TIMEOUT / dolt.pool-read-timeout in open.go) win, and the
// default is upstream's 10s. Shared-server deployments carry the 120s value in
// .beads/config.yaml (fleet rollout 2026-08-25); these tests pin the NEW contract.

func TestBuildServerDSN_ReadTimeoutDefaultIsUpstream(t *testing.T) {
	cfg := &Config{
		ServerHost: "127.0.0.1",
		ServerPort: 3306,
		ServerUser: "root",
	}
	dsn := buildServerDSN(cfg, "testdb")
	if !contains(dsn, "readTimeout=10s") {
		t.Errorf("expected upstream default readTimeout=10s in DSN, got: %s", dsn)
	}
	if !contains(dsn, "writeTimeout=10s") {
		t.Errorf("expected upstream default writeTimeout=10s in DSN, got: %s", dsn)
	}
}

func TestBuildServerDSN_PoolConfigWins(t *testing.T) {
	cfg := &Config{
		ServerHost:       "127.0.0.1",
		ServerPort:       3306,
		ServerUser:       "root",
		PoolReadTimeout:  120 * time.Second,
		PoolWriteTimeout: 90 * time.Second,
	}
	dsn := buildServerDSN(cfg, "testdb")
	if !contains(dsn, "readTimeout=2m0s") {
		t.Errorf("expected cfg readTimeout=2m0s in DSN, got: %s", dsn)
	}
	if !contains(dsn, "writeTimeout=1m30s") {
		t.Errorf("expected cfg writeTimeout=1m30s in DSN, got: %s", dsn)
	}
}

// TestBuildServerDSN_RetiredForkEnvHasNoEffect proves the retirement: the old
// fork alias must NOT influence the DSN anymore. A regression reintroducing the
// alias turns this red.
func TestBuildServerDSN_RetiredForkEnvHasNoEffect(t *testing.T) {
	os.Setenv("BEADS_DOLT_READ_TIMEOUT", "45s")
	defer os.Unsetenv("BEADS_DOLT_READ_TIMEOUT")

	cfg := &Config{
		ServerHost: "127.0.0.1",
		ServerPort: 3306,
		ServerUser: "root",
	}
	dsn := buildServerDSN(cfg, "testdb")
	if contains(dsn, "readTimeout=45s") {
		t.Errorf("retired BEADS_DOLT_READ_TIMEOUT still affects DSN: %s", dsn)
	}
	if !contains(dsn, "readTimeout=10s") {
		t.Errorf("expected upstream default readTimeout=10s with retired env set, got: %s", dsn)
	}
}

// contains is a helper to check substring presence.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
