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
	if !contains(dsn, "readTimeout=10s") {
		t.Errorf("expected default readTimeout=10s in DSN, got: %s", dsn)
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
	if !contains(dsn, "readTimeout=10s") {
		t.Errorf("expected fallback readTimeout=10s when env invalid, got: %s", dsn)
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

// TestBuildServerDSN_WriteTimeoutUnaffected verifies WriteTimeout stays at 10s
// regardless of BEADS_DOLT_READ_TIMEOUT.
func TestBuildServerDSN_WriteTimeoutUnaffected(t *testing.T) {
	os.Setenv("BEADS_DOLT_READ_TIMEOUT", "2m")
	defer os.Unsetenv("BEADS_DOLT_READ_TIMEOUT")

	cfg := &Config{
		ServerHost: "127.0.0.1",
		ServerPort: 3306,
		ServerUser: "root",
	}
	dsn := buildServerDSN(cfg, "testdb")
	if !contains(dsn, "writeTimeout=10s") {
		t.Errorf("expected writeTimeout=10s unchanged, got: %s", dsn)
	}
	if !contains(dsn, "readTimeout=2m0s") {
		t.Errorf("expected readTimeout=2m0s, got: %s", dsn)
	}
}

// compile-time check that time.ParseDuration is used (avoid unused import).
var _ = time.ParseDuration
