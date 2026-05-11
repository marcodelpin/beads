package schema

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/steveyegge/beads/internal/types"
)

func EnsureBackfilledCustomStatusesCustomTypes(ctx context.Context, db DBConn) error {
	if err := backfillCustomTypes(ctx, db); err != nil {
		return fmt.Errorf("backfill custom_types: %w", err)
	}
	if err := backfillCustomStatuses(ctx, db); err != nil {
		return fmt.Errorf("backfill custom_statuses: %w", err)
	}
	return nil
}

func backfillCustomTypes(ctx context.Context, db DBConn) error {
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM custom_types").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	var value string
	err := db.QueryRowContext(ctx, "SELECT `value` FROM config WHERE `key` = 'types.custom'").Scan(&value)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if value == "" {
		return nil
	}

	for _, name := range parseTypesValue(value) {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, "INSERT IGNORE INTO custom_types (name) VALUES (?)", name); err != nil {
			return fmt.Errorf("inserting type %q: %w", name, err)
		}
	}
	return nil
}

func backfillCustomStatuses(ctx context.Context, db DBConn) error {
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM custom_statuses").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	var value string
	err := db.QueryRowContext(ctx, "SELECT `value` FROM config WHERE `key` = 'status.custom'").Scan(&value)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if value == "" {
		return nil
	}

	parsed, parseErr := types.ParseCustomStatusConfig(value)
	if parseErr != nil {
		log.Printf("schema: skipping invalid status.custom entries: %v", parseErr)
		return nil
	}
	for _, s := range parsed {
		if _, err := db.ExecContext(ctx, "INSERT IGNORE INTO custom_statuses (name, category) VALUES (?, ?)", s.Name, string(s.Category)); err != nil {
			return fmt.Errorf("inserting status %q: %w", s.Name, err)
		}
	}
	return nil
}

// parseTypesValue interprets types.custom as a JSON array first, falling back
// to a comma-separated list. Matches the behavior of the former compat
// migration helper of the same name.
func parseTypesValue(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var jsonTypes []string
	if err := json.Unmarshal([]byte(value), &jsonTypes); err == nil {
		return jsonTypes
	}
	parts := strings.Split(value, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
