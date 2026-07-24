package main

import (
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/config"
)

type backupEnabledEffectiveValueCase struct {
	name         string
	envVal       string // "\x00" = unset
	hasRemote    bool
	sharedServer bool
	wantPrefix   string // leading "true "/"false "
	wantContains string // source annotation substring
}

// TestConfigGetBackupEnabled_EffectiveValue pins wy-zrmqr fix #3: `bd config
// get backup.enabled` must report the EFFECTIVE value (what isBackupAutoEnabled
// actually returns) plus its source, not the raw stored value. Before the fix
// it printed "false"/"not set" even while auto-backup was running via
// primeHasGitRemote — the mismatch that hid the storm from operators.
func TestConfigGetBackupEnabled_EffectiveValue(t *testing.T) {
	runConfigGetBackupEnabledEffectiveValueCases(t, []backupEnabledEffectiveValueCase{
		{
			name:         "unset + remote + sql-server → off (server mode)",
			envVal:       "\x00",
			hasRemote:    true,
			sharedServer: true,
			wantPrefix:   "false ",
			wantContains: "sql-server mode",
		},
		{
			name:         "explicit true + sql-server → on (env var)",
			envVal:       "true",
			hasRemote:    false,
			sharedServer: true,
			wantPrefix:   "true ",
			wantContains: "env var",
		},
		{
			name:         "explicit false → off (env var)",
			envVal:       "false",
			hasRemote:    true,
			wantPrefix:   "false ",
			wantContains: "env var",
		},
	})
}

func runConfigGetBackupEnabledEffectiveValueCases(t *testing.T, tests []backupEnabledEffectiveValueCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := primeHasGitRemote
			primeHasGitRemote = func() bool { return tt.hasRemote }
			t.Cleanup(func() { primeHasGitRemote = orig })

			if tt.sharedServer {
				t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")
			} else {
				os.Unsetenv("BEADS_DOLT_SHARED_SERVER")
				t.Cleanup(func() { os.Unsetenv("BEADS_DOLT_SHARED_SERVER") })
			}

			if tt.envVal == "\x00" {
				os.Unsetenv("BD_BACKUP_ENABLED")
				t.Cleanup(func() { os.Unsetenv("BD_BACKUP_ENABLED") })
			} else {
				t.Setenv("BD_BACKUP_ENABLED", tt.envVal)
			}

			// The two "unset + no sql-server" cases assert the EMBEDDED-mode
			// defaults, which a !cgo build can never reach: store_factory_nocgo.go
			// defines usesSQLServer() as an unconditional true (embedded Dolt needs
			// CGO), so runConfigGetBackupEnabled always reports "sql-server mode"
			// there. That is correct production behaviour, not a bug -- so skip
			// those cases instead of asserting an unreachable branch (bda-qoh).
			//
			// The guard is deliberately narrow: cases that set BEADS_DOLT_SHARED_SERVER
			// or an explicit BD_BACKUP_ENABLED still run in BOTH builds, so this file
			// keeps real coverage under CGO_ENABLED=0.
			if !tt.sharedServer && tt.envVal == "\x00" && usesSQLServer() {
				t.Skip("needs an embedded-capable (CGO) build: usesSQLServer() is unconditionally true without cgo")
			}

			config.ResetForTesting()
			t.Cleanup(config.ResetForTesting)
			if err := config.Initialize(); err != nil {
				t.Fatalf("config.Initialize: %v", err)
			}

			oldJSON := jsonOutput
			jsonOutput = false
			t.Cleanup(func() { jsonOutput = oldJSON })

			out := captureStdout(t, runConfigGetBackupEnabled)

			if !strings.HasPrefix(out, tt.wantPrefix) {
				t.Errorf("output %q: want prefix %q", out, tt.wantPrefix)
			}
			if !strings.Contains(out, tt.wantContains) {
				t.Errorf("output %q: want to contain %q", out, tt.wantContains)
			}
		})
	}
}
