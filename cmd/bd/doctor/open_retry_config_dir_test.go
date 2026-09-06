package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
)

// The diagnostic opens built here must resolve dolt.open-retry-budget from the
// project they are about, not from wherever its Dolt data happens to sit.
//
// doltServerConfig deliberately leaves cfg.BeadsDir empty -- setting it would
// also switch on project-identity verification and local-data-dir resolution
// for every doctor check, which is a different change -- so the only thing
// carrying the project directory into the budget resolution is
// OpenRetryConfigDir. Without it, a custom absolute dolt_data_dir leaves the
// open with no directory at all, and the project's explicit "0" loses to a
// user-global default.
func TestDoltServerConfigCarriesTheProjectDirForOpenRetry(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}
	if err := (&configfile.Config{Backend: configfile.BackendDolt}).Save(beadsDir); err != nil {
		t.Fatalf("save metadata.json: %v", err)
	}

	t.Run("default layout", func(t *testing.T) {
		doltPath := getDatabasePath(beadsDir)
		// Control: in this layout the data path's parent IS the project, so
		// the field is not the only thing that could answer. The arm below is
		// the one that discriminates.
		if filepath.Dir(doltPath) != beadsDir {
			t.Fatalf("control: the default layout must keep the data path inside the project, got %q", doltPath)
		}
		if got := doltServerConfig(beadsDir, doltPath).OpenRetryConfigDir; got != beadsDir {
			t.Errorf("OpenRetryConfigDir = %q, want %q", got, beadsDir)
		}
	})

	t.Run("custom absolute data dir", func(t *testing.T) {
		custom := filepath.Join(t.TempDir(), "fastfs", "beads-dolt-data")
		if err := os.MkdirAll(custom, 0o755); err != nil {
			t.Fatalf("mkdir custom data dir: %v", err)
		}
		t.Setenv("BEADS_DOLT_DATA_DIR", custom)

		doltPath := getDatabasePath(beadsDir)
		// Precondition: the data really did leave the project, so nothing
		// derivable from the path can name it.
		if doltPath != custom {
			t.Fatalf("precondition: expected the custom data dir %q, got %q", custom, doltPath)
		}
		if filepath.Dir(doltPath) == beadsDir {
			t.Fatalf("precondition: the custom data path must not sit inside the project, got %q", doltPath)
		}
		if got := doltServerConfig(beadsDir, doltPath).OpenRetryConfigDir; got != beadsDir {
			t.Errorf("OpenRetryConfigDir = %q, want %q -- the project directory is the only place this open can learn its budget from", got, beadsDir)
		}
	})
}
