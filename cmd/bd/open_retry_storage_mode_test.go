package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
)

// The open-retry budget (dolt.open-retry-budget) exists for an open that talks
// to a server beads does not own, so it must not engage on a workspace this
// package's store factory opens with the EMBEDDED backend: there is no server
// there for waiting to help.
//
// Both sides now ask configfile.StorageMode -- the factory to route the open,
// internal/storage/dolt to decide whether the budget applies -- so the two
// cannot disagree about which store a directory opens with. This test pins
// that agreement on the fixture where the previous predicate got it wrong: a
// metadata.json with a backend and a database and nothing about a server,
// which the lifecycle resolver (doltserver.ResolveServerMode) reports as
// Owned rather than Embedded.
func TestOpenRetryStorageModeAgreesBetweenFactoryAndOpen(t *testing.T) {
	write := func(t *testing.T, meta *configfile.Config) string {
		t.Helper()
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		if err := os.MkdirAll(beadsDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := meta.Save(beadsDir); err != nil {
			t.Fatalf("save metadata.json: %v", err)
		}
		return beadsDir
	}

	cases := []struct {
		name string
		meta *configfile.Config
		want configfile.StorageMode
	}{
		{
			name: "no dolt_mode is an embedded workspace",
			meta: &configfile.Config{
				Backend:      configfile.BackendDolt,
				DoltDatabase: "openretry_probe",
			},
			want: configfile.StorageModeEmbedded,
		},
		{
			// Positive control. Without it a predicate that answered
			// "embedded" for everything would satisfy the row above, and the
			// agreement it reports would be an agreement about nothing.
			name: "dolt_mode server is a server workspace",
			meta: &configfile.Config{
				Backend:      configfile.BackendDolt,
				DoltMode:     configfile.DoltModeServer,
				DoltDatabase: "openretry_probe",
			},
			want: configfile.StorageModeServer,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BEADS_DOLT_SERVER_MODE", "")
			t.Setenv("BEADS_DOLT_SHARED_SERVER", "")
			t.Setenv("BEADS_DOLT_SERVER_HOST", "")
			beadsDir := write(t, tc.meta)

			// The factory's own branch input: Load and normalizeLoadedConfig
			// are the two steps newDoltStoreFromConfig and
			// newReadOnlyStoreFromConfig take before switching on the mode,
			// and they are this package's functions, not a restatement.
			loaded, err := configfile.Load(beadsDir)
			if err != nil {
				t.Fatalf("load metadata.json: %v", err)
			}
			factory := normalizeLoadedConfig(loaded).StorageMode()

			// What internal/storage/dolt asks about the same directory when
			// it decides whether the budget applies.
			opener, err := configfile.ResolveStorageMode(beadsDir)
			if err != nil {
				t.Fatalf("resolve storage mode: %v", err)
			}

			if factory != tc.want {
				t.Errorf("the store factory must route this workspace to %v, got %v", tc.want, factory)
			}
			if opener != factory {
				t.Errorf("the open path resolved %v where the store factory routes %v", opener, factory)
			}
		})
	}
}
