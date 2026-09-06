//go:build cgo

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage/dolt"
)

// The open-retry budget (dolt.open-retry-budget, GH#4379) lives inside
// dolt.New's server-mode open. The maintainer's mode-exclusion ask includes
// embedded mode, and internal/storage/dolt cannot answer for it: the exclusion
// there is a ROUTING fact owned by this package's store factory, which sends a
// non-server-mode open to internal/storage/embeddeddolt and never calls
// dolt.New at all.
//
// This test asserts that routing directly, with the budget configured, and
// pairs it with the server-mode arm as a positive control -- otherwise the
// embedded arm's "never reached the dial" would be indistinguishable from a
// factory that fails early for some unrelated reason.
func newOpenRetryBeadsDir(t *testing.T, budget string) string {
	t.Helper()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "dolt:\n  auto-start: false\n  open-retry-budget: " + budget + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	return beadsDir
}

func TestNewDoltStore_EmbeddedRoutingNeverReachesTheOpenRetryPath(t *testing.T) {
	// A port that is neither a production port nor the auto-start default.
	const externalTestPort = 43211
	t.Setenv("BEADS_TEST_MODE", "1")

	t.Run("embedded routing never reaches dolt.New", func(t *testing.T) {
		beadsDir := newOpenRetryBeadsDir(t, "30s")
		cfg := &dolt.Config{
			// ServerMode false is the whole point: newDoltStore routes this
			// to embeddeddolt, so the server open -- and with it the retry
			// budget -- is unreachable by construction.
			ServerMode: false,
			BeadsDir:   beadsDir,
			Path:       filepath.Join(beadsDir, "dolt"),
			// An empty database name makes embeddeddolt.Open refuse before it
			// opens anything, which keeps this a routing assertion rather than
			// a database fixture.
			Database:   "",
			ServerHost: "dolt.example.invalid",
			ServerPort: externalTestPort,
		}

		_, err := newDoltStore(t.Context(), cfg)
		if err == nil {
			t.Fatal("expected embeddeddolt.Open to refuse an empty database name")
		}
		if !strings.Contains(err.Error(), "database name must not be empty") {
			t.Fatalf("expected the embedded refusal, got %v", err)
		}
		// The server open's own vocabulary must be absent: reaching it would
		// have produced "Dolt server unreachable", with or without a budget.
		for _, forbidden := range []string{"Dolt server unreachable", "open-retry-budget"} {
			if strings.Contains(err.Error(), forbidden) {
				t.Errorf("an embedded open must not reach the server path (%q): %v", forbidden, err)
			}
		}
	})

	// Positive control: the SAME beadsDir and the SAME budget, with the one
	// field that decides routing flipped, does reach the server open. Without
	// this arm the assertion above could hold for a factory that never routes
	// anywhere.
	t.Run("server mode does reach dolt.New", func(t *testing.T) {
		beadsDir := newOpenRetryBeadsDir(t, "200ms")
		cfg := &dolt.Config{
			ServerMode: true,
			BeadsDir:   beadsDir,
			Path:       filepath.Join(beadsDir, "dolt"),
			Database:   "openretry_probe",
			// A closed local port refuses the connection, which IS retryable,
			// so a resolved budget shows up in the error text. That makes this
			// arm prove two things from outside the package: the open reached
			// dolt.New, and the SAME config.yaml the embedded arm used did
			// configure a budget there.
			ServerHost: "127.0.0.1",
			ServerPort: externalTestPort,
		}

		_, err := newDoltStore(t.Context(), cfg)
		if err == nil {
			t.Fatal("expected the server open to fail against a closed port")
		}
		for _, want := range []string{"Dolt server unreachable", "dolt.open-retry-budget=200ms"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("expected the server open's error to contain %q, got %v", want, err)
			}
		}
	})
}
