//go:build cgo

package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/steveyegge/beads/cmd/bd/doctor"
	"github.com/steveyegge/beads/cmd/bd/doctor/fix"
	"github.com/steveyegge/beads/internal/configfile"
)

// Integration tests for the SQLite→Dolt migration safety features.
// These test the full lifecycle using the test Dolt server infrastructure.
// Fixes GH#2016, GH#2086.

// TestMigrationSafety_FullLifecycle tests the complete migration flow:
// SQLite → backup → extract → import → verify counts → finalize → bd works.
func TestMigrationSafety_FullLifecycle(t *testing.T) {
	if testDoltServerPort == 0 {
		t.Skip("Dolt test server not available, skipping")
	}

	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Setup: legacy metadata (no backend field) + SQLite with data
	cfg := &configfile.Config{
		Database:       "beads.db",
		Backend:        "", // legacy — triggers auto-migration
		DoltMode:       configfile.DoltModeServer,
		DoltServerHost: "127.0.0.1",
		DoltServerPort: testDoltServerPort,
	}
	if err := cfg.Save(beadsDir); err != nil {
		t.Fatal(err)
	}

	sqlitePath := filepath.Join(beadsDir, "beads.db")
	createTestSQLiteDB(t, sqlitePath, "lifecycle", 5)

	// Act: run migration
	doAutoMigrateSQLiteToDolt(beadsDir)

	// Verify 1: backup was created
	backupFound := false
	entries, _ := os.ReadDir(beadsDir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "backup-pre-dolt-") {
			backupFound = true
			// Verify backup has content (not empty)
			info, _ := e.Info()
			if info.Size() == 0 {
				t.Error("backup file is empty")
			}
			break
		}
	}
	if !backupFound {
		t.Error("no backup file created during migration")
	}

	// Verify 2: beads.db renamed to .migrated
	if _, err := os.Stat(sqlitePath); !os.IsNotExist(err) {
		t.Error("beads.db should have been renamed to .migrated")
	}
	migratedPath := sqlitePath + ".migrated"
	if _, err := os.Stat(migratedPath); err != nil {
		t.Errorf("beads.db.migrated should exist: %v", err)
	}

	// Verify 3: metadata.json updated correctly
	updatedCfg, err := configfile.Load(beadsDir)
	if err != nil {
		t.Fatalf("failed to load updated config: %v", err)
	}
	if updatedCfg.Backend != configfile.BackendDolt {
		t.Errorf("backend should be 'dolt', got %q", updatedCfg.Backend)
	}
	if updatedCfg.Database != "dolt" {
		t.Errorf("database should be 'dolt', got %q", updatedCfg.Database)
	}
	if updatedCfg.DoltDatabase != "lifecycle" {
		t.Errorf("dolt_database should be 'lifecycle', got %q", updatedCfg.DoltDatabase)
	}

	// Verify 4: server config preserved through migration
	if updatedCfg.DoltServerHost != "127.0.0.1" {
		t.Errorf("DoltServerHost should be preserved, got %q", updatedCfg.DoltServerHost)
	}
	if updatedCfg.DoltServerPort != testDoltServerPort {
		t.Errorf("DoltServerPort should be preserved (%d), got %d", testDoltServerPort, updatedCfg.DoltServerPort)
	}

	// Clean up test database
	dropTestDatabase("lifecycle", testDoltServerPort)
}

// TestMigrationSafety_FailedMigrationRecovery tests that when migration fails
// (corrupted SQLite), the database is left in a recoverable state.
func TestMigrationSafety_FailedMigrationRecovery(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Setup: legacy metadata + corrupted SQLite file
	cfg := &configfile.Config{
		Database: "beads.db",
		Backend:  "", // legacy
	}
	if err := cfg.Save(beadsDir); err != nil {
		t.Fatal(err)
	}

	sqlitePath := filepath.Join(beadsDir, "beads.db")
	if err := os.WriteFile(sqlitePath, []byte("this is not a sqlite database but has enough bytes"), 0600); err != nil {
		t.Fatal(err)
	}

	// Act: attempt migration (should fail during extract)
	doAutoMigrateSQLiteToDolt(beadsDir)

	// Verify 1: backup was created BEFORE the failure
	backupFound := false
	entries, _ := os.ReadDir(beadsDir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "backup-pre-dolt-") {
			backupFound = true
			break
		}
	}
	if !backupFound {
		t.Error("backup should be created even when migration fails")
	}

	// Verify 2: SQLite file is intact (NOT renamed)
	if _, err := os.Stat(sqlitePath); err != nil {
		t.Error("beads.db should still exist after failed migration")
	}

	// Verify 3: metadata.json is unchanged (backend NOT set to dolt)
	loadedCfg, err := configfile.Load(beadsDir)
	if err != nil {
		t.Fatal(err)
	}
	if loadedCfg.Backend != "" {
		t.Errorf("backend should be unchanged (''), got %q after failed migration", loadedCfg.Backend)
	}

	// Verify 4: no dolt/ directory created
	if _, err := os.Stat(filepath.Join(beadsDir, "dolt")); !os.IsNotExist(err) {
		t.Error("dolt/ should not exist after failed migration")
	}
}

// TestMigrationSafety_DoctorDetectsAndFixesBrokenState tests the full doctor
// cycle: detect broken migration → fix → metadata restored.
func TestMigrationSafety_DoctorDetectsAndFixesBrokenState(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Setup: simulate a broken migration state
	// metadata says dolt but no dolt/ dir and SQLite still exists
	cfg := &configfile.Config{
		Database:     "dolt",
		Backend:      configfile.BackendDolt,
		DoltDatabase: "broken-project",
	}
	if err := cfg.Save(beadsDir); err != nil {
		t.Fatal(err)
	}

	// Create a SQLite DB with real data (the data that would be lost without recovery)
	sqlitePath := filepath.Join(beadsDir, "beads.db")
	createTestSQLiteDB(t, sqlitePath, "broken", 3)

	// Act 1: Doctor should detect broken state
	check := doctor.CheckBrokenMigrationState(tmpDir)
	if check.Status != doctor.StatusError {
		t.Errorf("expected error status, got %q: %s", check.Status, check.Message)
	}
	if check.Fix == "" {
		t.Error("expected fix suggestion for broken migration state")
	}

	// Act 2: Apply fix directly via the fix package
	if err := fix.BrokenMigrationState(tmpDir); err != nil {
		t.Fatalf("BrokenMigrationState fix failed: %v", err)
	}

	// Verify: metadata restored to sqlite
	fixedCfg, err := configfile.Load(beadsDir)
	if err != nil {
		t.Fatal(err)
	}
	if fixedCfg.GetBackend() != configfile.BackendSQLite {
		t.Errorf("backend should be restored to 'sqlite', got %q", fixedCfg.GetBackend())
	}
	if fixedCfg.Database != "beads.db" {
		t.Errorf("database should be restored to 'beads.db', got %q", fixedCfg.Database)
	}

	// Verify: SQLite data still intact
	count, err := countSQLiteIssues(sqlitePath)
	if err != nil {
		t.Fatalf("failed to count SQLite issues after fix: %v", err)
	}
	if count != 3 {
		t.Errorf("SQLite should still have 3 issues after fix, got %d", count)
	}
}

// TestMigrationSafety_DoctorFixesBeadsDBMigrated tests that doctor --fix can
// recover from a broken state where beads.db was renamed to .migrated
// but the Dolt migration didn't actually complete.
func TestMigrationSafety_DoctorFixesBeadsDBMigrated(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Setup: metadata says dolt, beads.db was renamed to .migrated, no dolt/ dir
	cfg := &configfile.Config{
		Database:     "dolt",
		Backend:      configfile.BackendDolt,
		DoltDatabase: "stale-project",
	}
	if err := cfg.Save(beadsDir); err != nil {
		t.Fatal(err)
	}

	// beads.db.migrated exists (renamed during failed migration), but NO beads.db
	migratedPath := filepath.Join(beadsDir, "beads.db.migrated")
	createTestSQLiteDB(t, migratedPath, "stale", 7)

	// Act: Apply fix directly
	if err := fix.BrokenMigrationState(tmpDir); err != nil {
		t.Fatalf("BrokenMigrationState fix failed: %v", err)
	}

	// Verify: beads.db.migrated was renamed back to beads.db
	sqlitePath := filepath.Join(beadsDir, "beads.db")
	if _, err := os.Stat(sqlitePath); err != nil {
		t.Errorf("beads.db should exist after fix (renamed from .migrated): %v", err)
	}

	// Verify: metadata restored
	fixedCfg, err := configfile.Load(beadsDir)
	if err != nil {
		t.Fatal(err)
	}
	if fixedCfg.GetBackend() != configfile.BackendSQLite {
		t.Errorf("backend should be 'sqlite', got %q", fixedCfg.GetBackend())
	}

	// Verify: data intact (7 issues)
	count, err := countSQLiteIssues(sqlitePath)
	if err != nil {
		t.Fatalf("failed to count issues: %v", err)
	}
	if count != 7 {
		t.Errorf("expected 7 issues after recovery, got %d", count)
	}
}

// TestMigrationSafety_IdempotentRetry tests that running migration twice
// doesn't duplicate data or lose the backup.
func TestMigrationSafety_IdempotentRetry(t *testing.T) {
	if testDoltServerPort == 0 {
		t.Skip("Dolt test server not available, skipping")
	}

	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Setup: legacy metadata + SQLite with data
	cfg := &configfile.Config{
		Database:       "beads.db",
		Backend:        "",
		DoltMode:       configfile.DoltModeServer,
		DoltServerHost: "127.0.0.1",
		DoltServerPort: testDoltServerPort,
	}
	if err := cfg.Save(beadsDir); err != nil {
		t.Fatal(err)
	}

	sqlitePath := filepath.Join(beadsDir, "beads.db")
	createTestSQLiteDB(t, sqlitePath, "idempotent", 4)

	// Act 1: first migration
	doAutoMigrateSQLiteToDolt(beadsDir)

	// Verify first migration succeeded
	if _, err := os.Stat(sqlitePath); !os.IsNotExist(err) {
		t.Fatal("beads.db should be renamed after first migration")
	}
	cfg1, _ := configfile.Load(beadsDir)
	if cfg1.Backend != configfile.BackendDolt {
		t.Fatal("backend should be dolt after first migration")
	}

	// Act 2: run again — should be a no-op (backend already dolt)
	doAutoMigrateSQLiteToDolt(beadsDir)

	// Verify: metadata unchanged
	cfg2, _ := configfile.Load(beadsDir)
	if cfg2.Backend != configfile.BackendDolt {
		t.Error("backend should still be dolt after second run")
	}
	if cfg2.DoltDatabase != "idempotent" {
		t.Errorf("dolt_database should still be 'idempotent', got %q", cfg2.DoltDatabase)
	}

	// Verify: backup still exists
	backupCount := 0
	entries, _ := os.ReadDir(beadsDir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "backup-pre-dolt-") {
			backupCount++
		}
	}
	if backupCount != 1 {
		t.Errorf("expected exactly 1 backup file, got %d", backupCount)
	}

	// Clean up
	dropTestDatabase("idempotent", testDoltServerPort)
}

// TestMigrationSafety_SQLiteGuardPreventsAutoMigration tests that setting
// backend=sqlite in metadata.json prevents auto-migration.
func TestMigrationSafety_SQLiteGuardPreventsAutoMigration(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Setup: explicit sqlite backend + SQLite with data
	cfg := &configfile.Config{
		Database: "beads.db",
		Backend:  configfile.BackendSQLite,
	}
	if err := cfg.Save(beadsDir); err != nil {
		t.Fatal(err)
	}

	sqlitePath := filepath.Join(beadsDir, "beads.db")
	createTestSQLiteDB(t, sqlitePath, "guarded", 3)

	// Act: attempt auto-migration
	doAutoMigrateSQLiteToDolt(beadsDir)

	// Verify: SQLite is untouched
	if _, err := os.Stat(sqlitePath); err != nil {
		t.Error("beads.db should still exist when backend=sqlite")
	}

	// Verify: no backup created (migration didn't even start)
	entries, _ := os.ReadDir(beadsDir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "backup-pre-dolt-") {
			t.Error("no backup should be created when backend=sqlite — migration should be skipped entirely")
		}
	}

	// Verify: metadata unchanged
	loadedCfg, _ := configfile.Load(beadsDir)
	if loadedCfg.GetBackend() != configfile.BackendSQLite {
		t.Errorf("backend should still be 'sqlite', got %q", loadedCfg.GetBackend())
	}

	// Verify: no dolt/ directory
	if _, err := os.Stat(filepath.Join(beadsDir, "dolt")); !os.IsNotExist(err) {
		t.Error("dolt/ should not exist when backend=sqlite")
	}

	// Verify: data intact
	count, err := countSQLiteIssues(sqlitePath)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("expected 3 issues intact, got %d", count)
	}
}

// TestMigrationSafety_BackupContentMatchesSource verifies that the backup
// created during migration is a byte-for-byte copy of the original SQLite.
func TestMigrationSafety_BackupContentMatchesSource(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	sqlitePath := filepath.Join(beadsDir, "beads.db")
	createTestSQLiteDB(t, sqlitePath, "backup-verify", 3)

	// Read original content before migration touches anything
	originalContent, err := os.ReadFile(sqlitePath)
	if err != nil {
		t.Fatal(err)
	}

	// Create backup using our safety function
	backupPath, err := backupSQLite(sqlitePath)
	if err != nil {
		t.Fatal(err)
	}

	// Verify: backup is byte-for-byte identical
	backupContent, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}

	if len(originalContent) != len(backupContent) {
		t.Errorf("backup size (%d) != original size (%d)", len(backupContent), len(originalContent))
	}

	for i := range originalContent {
		if originalContent[i] != backupContent[i] {
			t.Errorf("backup differs at byte %d", i)
			break
		}
	}

	// Verify: original still exists
	if _, err := os.Stat(sqlitePath); err != nil {
		t.Error("original should still exist after backup")
	}
}

// TestMigrationSafety_MetadataJSONIsValidAfterMigration verifies the
// migrated metadata.json is well-formed JSON with all expected fields.
func TestMigrationSafety_MetadataJSONIsValidAfterMigration(t *testing.T) {
	if testDoltServerPort == 0 {
		t.Skip("Dolt test server not available, skipping")
	}

	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	cfg := &configfile.Config{
		Database:       "beads.db",
		Backend:        "",
		DoltMode:       configfile.DoltModeServer,
		DoltServerHost: "127.0.0.1",
		DoltServerPort: testDoltServerPort,
	}
	if err := cfg.Save(beadsDir); err != nil {
		t.Fatal(err)
	}

	sqlitePath := filepath.Join(beadsDir, "beads.db")
	createTestSQLiteDB(t, sqlitePath, "jsonvalid", 2)

	doAutoMigrateSQLiteToDolt(beadsDir)

	// Read and validate metadata.json
	data, err := os.ReadFile(filepath.Join(beadsDir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Must be valid JSON
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Errorf("metadata.json is not valid JSON: %v\nContent: %s", err, data)
	}

	// Must have required fields
	for _, key := range []string{"database", "backend", "dolt_database"} {
		if _, ok := result[key]; !ok {
			t.Errorf("metadata.json missing required field %q", key)
		}
	}

	// backend must be "dolt"
	if result["backend"] != "dolt" {
		t.Errorf("backend = %v, want 'dolt'", result["backend"])
	}

	// dolt_database must match prefix
	if result["dolt_database"] != "jsonvalid" {
		t.Errorf("dolt_database = %v, want 'jsonvalid'", result["dolt_database"])
	}

	dropTestDatabase("jsonvalid", testDoltServerPort)
}

// countSQLiteIssues opens a SQLite database and counts the issues.
func countSQLiteIssues(dbPath string) (int, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM issues").Scan(&count)
	return count, err
}
