package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
)

func TestRunServerHealthChecksSQLiteIsNotAMigrationWarning(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := (&configfile.Config{Backend: configfile.BackendSQLite, SQLitePath: "beads.db"}).Save(beadsDir); err != nil {
		t.Fatalf("save SQLite config: %v", err)
	}

	result := RunServerHealthChecks(tmpDir)
	if !result.OverallOK || len(result.Checks) != 1 {
		t.Fatalf("SQLite server-health result = %#v, want one benign N/A check", result)
	}
	check := result.Checks[0]
	if check.Status != StatusOK || !strings.Contains(check.Message, "SQLite") || check.Fix != "" {
		t.Fatalf("SQLite server-health check = %#v, want supported-backend N/A without metadata edit", check)
	}
}
