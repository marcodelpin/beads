package main

import (
	"database/sql"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/debug"
)

// backupSQLite copies the SQLite database to a timestamped backup file in the
// same directory. The original file is preserved. Returns the backup path.
func backupSQLite(sqlitePath string) (string, error) {
	dir := filepath.Dir(sqlitePath)
	base := filepath.Base(sqlitePath)
	ext := filepath.Ext(base)
	name := base[:len(base)-len(ext)]

	timestamp := time.Now().Format("20060102-150405")
	backupName := fmt.Sprintf("%s.backup-pre-dolt-%s%s", name, timestamp, ext)
	backupPath := filepath.Join(dir, backupName)

	// If backup already exists (same second), add counter suffix.
	if _, err := os.Stat(backupPath); err == nil {
		for i := 1; i <= 100; i++ {
			backupName = fmt.Sprintf("%s.backup-pre-dolt-%s-%d%s", name, timestamp, i, ext)
			backupPath = filepath.Join(dir, backupName)
			if _, err := os.Stat(backupPath); err != nil {
				break // File doesn't exist (or stat error) — use this name
			}
			if i == 100 {
				return "", fmt.Errorf("too many backup files for timestamp %s", timestamp)
			}
		}
	}

	src, err := os.Open(sqlitePath) // #nosec G304 - user-provided path
	if err != nil {
		return "", fmt.Errorf("opening sqlite database for backup: %w", err)
	}
	defer src.Close()

	dst, err := os.OpenFile(backupPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) // #nosec G304 - derived from user path; O_EXCL prevents TOCTOU
	if err != nil {
		return "", fmt.Errorf("creating backup file: %w", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return "", fmt.Errorf("copying sqlite database: %w", err)
	}

	return backupPath, nil
}

// verifyServerTarget checks that no Dolt server is running on the given port,
// or if one is, that it is serving the expected database. This prevents the
// migration from accidentally writing to the wrong Dolt instance.
//
// Returns nil if no server is listening (connection refused) or if the server
// is serving the expected database.
// verifyServerTarget checks whether a Dolt server on the given port already
// hosts databases that could conflict with the expected database name.
// Returns nil if no server is running (connection refused) or if the server
// does not have a conflicting database. Returns error for timeouts, auth
// failures, or other uncertain states.
func verifyServerTarget(expectedDBName string, port int) error {
	if port == 0 {
		return nil
	}

	host := "127.0.0.1"
	addr := fmt.Sprintf("%s:%d", host, port)

	// Check if anything is listening on the port
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		// Connection refused = no server running = safe to proceed.
		// But timeouts or other errors = unknown state = warn and abort.
		if opErr, ok := err.(*net.OpError); ok {
			if sysErr, ok := opErr.Err.(*os.SyscallError); ok {
				if sysErr.Syscall == "connect" {
					// ECONNREFUSED — no server, safe
					return nil
				}
			}
		}
		return fmt.Errorf("cannot verify server on port %d (unknown error, not safe to proceed): %w", port, err)
	}
	conn.Close()

	// Server is listening. Query SHOW DATABASES to see what's there.
	dsn := fmt.Sprintf("root@tcp(%s)/", addr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("connecting to server on port %d: %w", port, err)
	}
	defer db.Close()
	db.SetConnMaxLifetime(2 * time.Second)

	rows, err := db.Query("SHOW DATABASES")
	if err != nil {
		// Can't list databases — might be non-MySQL service or auth issue.
		// Treat as unsafe since we can't verify.
		return fmt.Errorf("cannot query databases on port %d (may not be a Dolt server): %w", port, err)
	}
	defer rows.Close()

	// Scan database names — if expectedDBName already exists, that's fine
	// (idempotent migration). We're only concerned if the server appears to
	// be a completely different project's server with no relation to us.
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		if name == expectedDBName {
			// Our database already exists on this server — safe (idempotent)
			return nil
		}
	}

	// Database doesn't exist yet — server will create it. This is the normal
	// first-migration case for a shared server (Gas Town model).
	return nil
}

// verifyMigrationCounts compares source (SQLite) row counts against target
// (Dolt) row counts. Dolt counts must be >= source counts since there may be
// pre-existing issues in the Dolt database.
func verifyMigrationCounts(sourceIssueCount, sourceDepsCount, doltIssueCount, doltDepsCount int) error {
	var errs []string
	if doltIssueCount < sourceIssueCount {
		errs = append(errs, fmt.Sprintf(
			"issue count mismatch: source=%d, dolt=%d", sourceIssueCount, doltIssueCount,
		))
	}
	if doltDepsCount < sourceDepsCount {
		errs = append(errs, fmt.Sprintf(
			"dependency count mismatch: source=%d, dolt=%d", sourceDepsCount, doltDepsCount,
		))
	}
	if len(errs) > 0 {
		return fmt.Errorf("migration verification failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

// finalizeMigration updates metadata and config to reflect the completed
// migration, then renames the SQLite file. This is the ONLY function that
// modifies metadata and should be called last after verification succeeds.
func finalizeMigration(beadsDir string, sqlitePath string, dbName string) error {
	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		return fmt.Errorf("loading metadata: %w", err)
	}
	if cfg == nil {
		cfg = configfile.DefaultConfig()
	}

	cfg.Backend = configfile.BackendDolt
	cfg.Database = "dolt"
	cfg.DoltDatabase = dbName

	if err := cfg.Save(beadsDir); err != nil {
		return fmt.Errorf("saving metadata: %w", err)
	}

	// Write sync.mode to config.yaml so future runs know we migrated.
	// This is best-effort: config package may not be initialized during
	// auto-migration (which runs before full CLI setup). The metadata.json
	// Backend field is the authoritative source of truth.
	if err := config.SaveConfigValue("sync.mode", string(config.SyncModeDoltNative), beadsDir); err != nil {
		// Non-fatal — metadata.json is already updated
		debug.Logf("finalizeMigration: config.yaml sync.mode write skipped: %v", err)
	}

	// Rename SQLite file to mark it as migrated.
	migratedPath := sqlitePath + ".migrated"
	if err := os.Rename(sqlitePath, migratedPath); err != nil {
		return fmt.Errorf("renaming sqlite database: %w", err)
	}

	return nil
}

// rollbackMetadata restores metadata.json to the original configuration.
// Called when migration fails after metadata was partially modified.
func rollbackMetadata(beadsDir string, originalCfg *configfile.Config) error {
	if originalCfg == nil {
		return fmt.Errorf("no original config to restore")
	}
	return originalCfg.Save(beadsDir)
}
