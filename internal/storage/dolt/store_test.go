package dolt

import (
	"os"
	"testing"
	"time"
)

func TestBuildServerDSN_ReadTimeoutDefault(t *testing.T) {
	os.Unsetenv("BEADS_DOLT_READ_TIMEOUT")
	cfg := &Config{
		ServerHost: "127.0.0.1",
		ServerPort: 3306,
		ServerUser: "root",
	}
	dsn := buildServerDSN(cfg, "testdb")
	// Default bumped 10s -> 120s by sys-r8253 (migration 37 takes ~21s).
	if !contains(dsn, "readTimeout=2m0s") {
		t.Errorf("expected default readTimeout=2m0s in DSN, got: %s", dsn)
	}
}

func TestBuildServerDSN_ReadTimeoutEnvOverride(t *testing.T) {
	os.Setenv("BEADS_DOLT_READ_TIMEOUT", "30s")
	defer os.Unsetenv("BEADS_DOLT_READ_TIMEOUT")

	cfg := &Config{
		ServerHost: "127.0.0.1",
		ServerPort: 3306,
		ServerUser: "root",
	}
	dsn := buildServerDSN(cfg, "testdb")
	if !contains(dsn, "readTimeout=30s") {
		t.Errorf("expected readTimeout=30s in DSN, got: %s", dsn)
	}
}

func TestBuildServerDSN_ReadTimeoutEnvInvalid(t *testing.T) {
	os.Setenv("BEADS_DOLT_READ_TIMEOUT", "not-a-duration")
	defer os.Unsetenv("BEADS_DOLT_READ_TIMEOUT")

	cfg := &Config{
		ServerHost: "127.0.0.1",
		ServerPort: 3306,
		ServerUser: "root",
	}
	dsn := buildServerDSN(cfg, "testdb")
	// Invalid env falls back to the 120s default (sys-r8253).
	if !contains(dsn, "readTimeout=2m0s") {
		t.Errorf("expected fallback readTimeout=2m0s when env invalid, got: %s", dsn)
	}
}

func TestBuildServerDSN_ReadTimeoutEnvMinutes(t *testing.T) {
	os.Setenv("BEADS_DOLT_READ_TIMEOUT", "5m")
	defer os.Unsetenv("BEADS_DOLT_READ_TIMEOUT")

	cfg := &Config{
		ServerHost: "127.0.0.1",
		ServerPort: 3306,
		ServerUser: "root",
	}
	dsn := buildServerDSN(cfg, "testdb")
	if !contains(dsn, "readTimeout=5m0s") {
		t.Errorf("expected readTimeout=5m0s in DSN, got: %s", dsn)
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

// TestBuildServerDSN_WriteTimeoutUnaffected verifies WriteTimeout stays at the
// 120s default regardless of BEADS_DOLT_READ_TIMEOUT (which only governs the
// read side). The env read value is deliberately distinct from the write
// default so the assertions can tell the two timeouts apart.
func TestBuildServerDSN_WriteTimeoutUnaffected(t *testing.T) {
	os.Setenv("BEADS_DOLT_READ_TIMEOUT", "45s")
	defer os.Unsetenv("BEADS_DOLT_READ_TIMEOUT")

	cfg := &Config{
		ServerHost: "127.0.0.1",
		ServerPort: 3306,
		ServerUser: "root",
	}
	dsn := buildServerDSN(cfg, "testdb")
	if !contains(dsn, "writeTimeout=2m0s") {
		t.Errorf("expected writeTimeout=2m0s unchanged by read env, got: %s", dsn)
	}
	if !contains(dsn, "readTimeout=45s") {
		t.Errorf("expected readTimeout=45s from env, got: %s", dsn)
	}
}

// compile-time check that time.ParseDuration is used (avoid unused import).
var _ = time.ParseDuration
