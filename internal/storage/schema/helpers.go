package schema

import (
	"context"
	"fmt"
)

var ignoredTableNames = []string{
	"local_metadata",
	"repo_mtimes",
	"wisps",
	"wisp_labels",
	"wisp_dependencies",
	"wisp_events",
	"wisp_comments",
}

func EnsureIgnoredTables(ctx context.Context, db DBConn) error {
	for _, table := range ignoredTableNames {
		ok, err := TableExists(ctx, db, table)
		if err != nil {
			return fmt.Errorf("check %s table: %w", table, err)
		}
		if !ok {
			return CreateIgnoredTables(ctx, db)
		}
	}
	return nil
}

func CreateIgnoredTables(ctx context.Context, db DBConn) error {
	for _, ddl := range IgnoredTableDDL() {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("create ignored table: %w", err)
		}
	}
	return nil
}

func TableExists(ctx context.Context, db DBConn, table string) (bool, error) {
	rows, err := db.QueryContext(ctx, "SHOW TABLES LIKE '"+table+"'") //nolint:gosec // G202: table name is an internal constant
	if err != nil {
		return false, fmt.Errorf("check table %s: %w", table, err)
	}
	defer rows.Close()
	return rows.Next(), nil
}
