//go:build cgo

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/doltserver"
	"github.com/steveyegge/beads/internal/storage/dolt"
)

// autoMigrateSQLiteToDolt finds the .beads directory and delegates to
// doAutoMigrateSQLiteToDolt for the actual migration logic.
func autoMigrateSQLiteToDolt() {
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return
	}
	doAutoMigrateSQLiteToDolt(beadsDir)
}

// doAutoMigrateSQLiteToDolt detects a legacy SQLite beads.db in the given
// .beads directory and automatically migrates it to Dolt. This runs once,
// transparently, on the first bd command after upgrading to a Dolt-only CLI.
//
// The migration is best-effort: failures produce warnings, not fatal errors.
// After a successful migration, beads.db is renamed to beads.db.migrated.
//
// Edge cases handled:
//   - beads.db is 0 bytes → not a real database, remove it
//   - metadata.json backend already "dolt" → stale leftover, rename to .migrated
//   - beads.db.migrated already exists → migration already completed, skip
//   - beads.db + dolt/ both exist → leftover SQLite, rename it
//   - Dolt directory already exists → no migration needed
//   - Corrupted SQLite → warn and skip
//   - Dolt server not running → warn and skip (retry on next command)
func doAutoMigrateSQLiteToDolt(beadsDir string) {
	// Check for SQLite database
	sqlitePath := findSQLiteDB(beadsDir)
	if sqlitePath == "" {
		return // No SQLite database, nothing to migrate
	}

	// Skip backup/migrated files
	base := filepath.Base(sqlitePath)
	if strings.Contains(base, ".backup") || strings.Contains(base, ".migrated") {
		return
	}

	// Guard: if the file is empty (0 bytes), it's not a real SQLite database.
	// This happens when a process creates beads.db but crashes before writing.
	// Remove the empty file to prevent repeated failed migration attempts.
	if info, err := os.Stat(sqlitePath); err == nil && info.Size() == 0 {
		debug.Logf("auto-migrate-sqlite: removing empty %s (not a valid database)", base)
		_ = os.Remove(sqlitePath)
		return
	}

	// Guard: if metadata.json explicitly indicates SQLite backend, the user has
	// opted to keep SQLite. Do NOT auto-migrate. Fixes GH#2016.
	if cfg, err := configfile.Load(beadsDir); err == nil && cfg != nil {
		// Use GetBackend() for SQLite check (normalizes case, checks env var)
		if cfg.GetBackend() == configfile.BackendSQLite {
			debug.Logf("auto-migrate-sqlite: skipping — backend explicitly set to sqlite")
			return
		}
		// Use raw field for Dolt check: only match when metadata.json was
		// explicitly written with "dolt" (by a prior migration). Empty/missing
		// Backend field means legacy pre-Dolt config that needs migration.
		if strings.EqualFold(cfg.Backend, configfile.BackendDolt) {
			migratedPath := sqlitePath + ".migrated"
			if _, err := os.Stat(migratedPath); err != nil {
				if err := os.Rename(sqlitePath, migratedPath); err == nil {
					debug.Logf("auto-migrate-sqlite: renamed stale %s (backend already dolt)", base)
				}
			}
			return
		}
	}

	// Check if Dolt already exists — if so, SQLite is leftover from a prior migration
	doltPath := filepath.Join(beadsDir, "dolt")
	if _, err := os.Stat(doltPath); err == nil {
		// Dolt exists alongside SQLite. Rename the leftover SQLite file.
		migratedPath := sqlitePath + ".migrated"
		if _, err := os.Stat(migratedPath); err != nil {
			// No .migrated file yet — rename now
			if err := os.Rename(sqlitePath, migratedPath); err == nil {
				debug.Logf("auto-migrate-sqlite: renamed leftover %s to %s", filepath.Base(sqlitePath), filepath.Base(migratedPath))
			}
		}
		return
	}

	ctx := context.Background()

	// Phase 1: Backup — ALWAYS backup before touching anything
	fmt.Fprintf(os.Stderr, "Backing up SQLite database...\n")
	backupPath, backupErr := backupSQLite(sqlitePath)
	if backupErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: SQLite auto-migration aborted (backup failed): %v\n", backupErr)
		fmt.Fprintf(os.Stderr, "Hint: check disk space and permissions, then retry\n")
		return
	}
	fmt.Fprintf(os.Stderr, "  Backup saved to %s\n", filepath.Base(backupPath))

	// Extract data from SQLite (read-only)
	fmt.Fprintf(os.Stderr, "Extracting data from SQLite...\n")
	data, err := extractFromSQLite(ctx, sqlitePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: SQLite auto-migration failed (extract): %v\n", err)
		fmt.Fprintf(os.Stderr, "Hint: run 'bd migrate --to-dolt' manually, or remove %s to skip\n", base)
		fmt.Fprintf(os.Stderr, "  Your backup is at: %s\n", backupPath)
		return
	}

	if data.issueCount == 0 {
		debug.Logf("auto-migrate-sqlite: SQLite database is empty, migrating empty database")
	}
	fmt.Fprintf(os.Stderr, "  Found %d issues\n", data.issueCount)

	// Determine database name from prefix
	dbName := "beads"
	if data.prefix != "" {
		dbName = data.prefix
	}

	// Resolve server connection settings once — used for both verification and import
	resolvedPort := doltserver.DefaultConfig(beadsDir).Port
	resolvedHost := "127.0.0.1"
	resolvedUser := "root"
	resolvedPassword := ""
	resolvedTLS := false
	if cfg, err := configfile.Load(beadsDir); err == nil && cfg != nil {
		resolvedHost = cfg.GetDoltServerHost()
		if cfg.DoltServerPort > 0 {
			resolvedPort = cfg.DoltServerPort
		}
		resolvedUser = cfg.GetDoltServerUser()
		resolvedPassword = cfg.GetDoltServerPassword()
		resolvedTLS = cfg.GetDoltServerTLS()
	}

	// Verify server target — don't write to the wrong Dolt server
	if err := verifyServerTarget(dbName, resolvedPort); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: SQLite auto-migration aborted (server check): %v\n", err)
		fmt.Fprintf(os.Stderr, "\nTo fix:\n")
		fmt.Fprintf(os.Stderr, "  1. Stop the other project's Dolt server\n")
		fmt.Fprintf(os.Stderr, "  2. Or set BEADS_DOLT_SERVER_PORT to a unique port for this project\n")
		fmt.Fprintf(os.Stderr, "  Your SQLite database is intact. Backup at: %s\n", backupPath)
		return
	}

	// Save original config for rollback
	originalCfg, _ := configfile.Load(beadsDir)

	// Phase 2: Create Dolt store and import — use same resolved settings as verification
	doltPath = filepath.Join(beadsDir, "dolt")
	doltCfg := &dolt.Config{
		Path:           doltPath,
		Database:       dbName,
		ServerHost:     resolvedHost,
		ServerPort:     resolvedPort,
		ServerUser:     resolvedUser,
		ServerPassword: resolvedPassword,
		ServerTLS:      resolvedTLS,
	}

	fmt.Fprintf(os.Stderr, "Creating Dolt database...\n")
	doltStore, err := dolt.New(ctx, doltCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: SQLite auto-migration failed (dolt init): %v\n", err)
		fmt.Fprintf(os.Stderr, "Hint: ensure the Dolt server is running, then retry any bd command\n")
		fmt.Fprintf(os.Stderr, "  Your SQLite database is intact. Backup at: %s\n", backupPath)
		return
	}

	fmt.Fprintf(os.Stderr, "Importing data...\n")
	imported, skipped, importErr := importToDolt(ctx, doltStore, data)
	if importErr != nil {
		_ = doltStore.Close()
		_ = os.RemoveAll(doltPath) // Safe: doltPath was just created by us (guarded by Stat check above)
		fmt.Fprintf(os.Stderr, "Warning: SQLite auto-migration failed (import): %v\n", importErr)
		fmt.Fprintf(os.Stderr, "  Your SQLite database is intact. Backup at: %s\n", backupPath)
		return
	}

	// Set sync mode in Dolt config table
	if err := doltStore.SetConfig(ctx, "sync.mode", "dolt-native"); err != nil {
		debug.Logf("auto-migrate-sqlite: failed to set sync.mode: %v", err)
	}

	// Commit the migration
	commitMsg := fmt.Sprintf("Auto-migrate from SQLite: %d issues imported", imported)
	if err := doltStore.Commit(ctx, commitMsg); err != nil {
		debug.Logf("auto-migrate-sqlite: failed to create Dolt commit: %v", err)
	}

	_ = doltStore.Close()

	// Verify migration counts before finalizing
	// Note: importToDolt returns issue counts only, not dep counts.
	// Pass 0 for deps to skip that check (deps are imported with issues).
	if err := verifyMigrationCounts(data.issueCount, 0, imported+skipped, 0); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: migration verification failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "  Your SQLite database is intact. Backup at: %s\n", backupPath)
		// Rollback: remove dolt dir, restore metadata
		_ = os.RemoveAll(doltPath)
		if originalCfg != nil {
			if rbErr := rollbackMetadata(beadsDir, originalCfg); rbErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: metadata rollback also failed: %v\n", rbErr)
			}
		}
		return
	}

	// Finalize — update metadata and rename SQLite (atomic cutover)
	if err := finalizeMigration(beadsDir, sqlitePath, dbName); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: migration completed but finalization failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "  Data is in Dolt but metadata may be inconsistent.\n")
		fmt.Fprintf(os.Stderr, "  Run 'bd doctor --fix' to repair.\n")
		return
	}

	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "Migrated %d issues from SQLite to Dolt (%d skipped)\n", imported, skipped)
	} else {
		fmt.Fprintf(os.Stderr, "Migrated %d issues from SQLite to Dolt ✓\n", imported)
	}
}
