package schema

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

func MigrateOnBranch(ctx context.Context, conn *sql.Conn, defaultBranch string) (int, error) {
	// Bootstrap schema_migrations on the default branch and commit it before
	// any further work. Otherwise a failure later in this function leaks the
	// table as untracked working-set state — the deferred cleanup deletes the
	// generated branch (where the real migrations would have lived), and the
	// next init sees an untracked schema_migrations on the default branch with
	// no committed schema behind it.
	if _, err := conn.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", defaultBranch); err != nil {
		return 0, fmt.Errorf("checkout %q: %w", defaultBranch, err)
	}
	if _, err := conn.ExecContext(ctx, schemaMigrationsBootstrapSQL); err != nil {
		return 0, fmt.Errorf("creating schema_migrations table: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "CALL DOLT_ADD('-A')"); err != nil {
		return 0, fmt.Errorf("stage schema_migrations on %q: %w", defaultBranch, err)
	}
	if _, err := conn.ExecContext(ctx, "CALL DOLT_COMMIT('-m', 'schema: bootstrap schema_migrations')"); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "nothing to commit") {
			return 0, fmt.Errorf("commit schema_migrations on %q: %w", defaultBranch, err)
		}
	}

	var current int
	if err := conn.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&current); err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("reading current migration version: %w", err)
	}
	if current >= LatestVersion() {
		return 0, nil
	}

	generated := fmt.Sprintf("bd-schema-init-%d", time.Now().UnixNano())
	if _, err := conn.ExecContext(ctx, "CALL DOLT_BRANCH(?, ?)", generated, defaultBranch); err != nil {
		return 0, fmt.Errorf("create branch %q from %q: %w", generated, defaultBranch, err)
	}

	defer func() {
		if _, err := conn.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", defaultBranch); err != nil {
			log.Printf("schema: cleanup checkout %q: %v", defaultBranch, err)
		}
		if _, err := conn.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", generated); err != nil {
			log.Printf("schema: cleanup delete %q: %v", generated, err)
		}
	}()

	if _, err := conn.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", generated); err != nil {
		return 0, fmt.Errorf("checkout %q: %w", generated, err)
	}

	// Re-materialize ignored tables on the generated branch before applying
	// migrations — but only when upgrading (current > 0). Dolt-ignored tables
	// (wisps, local_metadata, repo_mtimes, …) live only in the working set and
	// don't transfer when branching, so later migrations that reference them
	// (e.g. 0035's INSERT INTO wisps SELECT … FROM issues) would otherwise
	// fail with "table not found".
	//
	// On a fresh init (current == 0) we skip this: the generated branch has no
	// tables at all, and the ignored-table DDL set includes ALTERs against
	// `issues` (migrations 23, 27) that would fail. MigrateUp will run every
	// migration from 1 onward and create the ignored tables in order.
	if current > 0 {
		if err := EnsureIgnoredTables(ctx, conn); err != nil {
			return 0, fmt.Errorf("ensure ignored tables on %q: %w", generated, err)
		}
	}

	applied, err := MigrateUp(ctx, conn)
	if err != nil {
		return 0, fmt.Errorf("migrate: %w", err)
	}

	if applied > 0 {
		if _, err := conn.ExecContext(ctx, "CALL DOLT_ADD('-A')"); err != nil {
			return 0, fmt.Errorf("stage: %w", err)
		}
		if _, err := conn.ExecContext(ctx, "CALL DOLT_COMMIT('-m', 'schema: apply migrations')"); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "nothing to commit") {
				return 0, fmt.Errorf("commit: %w", err)
			}
		}
	}

	if _, err := conn.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", defaultBranch); err != nil {
		return 0, fmt.Errorf("checkout %q (post-migrate): %w", defaultBranch, err)
	}
	if _, err := conn.ExecContext(ctx, "CALL DOLT_MERGE(?)", generated); err != nil {
		return 0, fmt.Errorf("merge %q into %q: %w", generated, defaultBranch, err)
	}

	// Re-materialize ignored tables on the default branch after the merge.
	// The merge brings over committed state only, so the working-set-only
	// ignored tables would otherwise be missing for the caller.
	if err := EnsureIgnoredTables(ctx, conn); err != nil {
		return 0, fmt.Errorf("ensure ignored tables on %q: %w", defaultBranch, err)
	}

	return applied, nil
}
