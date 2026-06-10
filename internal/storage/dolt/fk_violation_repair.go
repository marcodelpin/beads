package dolt

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
)

// fkCascadeRepairDeletes maps each synced child table holding a FOREIGN KEY to
// issues(id) (migrations 0041/0042 added ON DELETE/UPDATE CASCADE; ignored
// migration 0002 covers child_counters) to the DELETE that applies the FK's
// cascade semantics by hand after a merge (bd-6dnrw.4).
//
// Dolt merges each table row-wise and never re-executes cascades, so "clone A
// deletes issue X" merged with "clone B inserts a child row referencing X"
// produces a child row whose parent is gone — a foreign-key constraint
// violation that makes the merge transaction roll back, and retrying can never
// converge. Deleting the dangling rows is the convergent repair: it is exactly
// what the cascade did on the deleting clone, and what the FK would have
// forced had the two writes been sequenced on one database.
var fkCascadeRepairDeletes = map[string]string{
	"dependencies": `DELETE FROM dependencies
		WHERE issue_id NOT IN (SELECT id FROM issues)
		   OR (depends_on_issue_id IS NOT NULL AND depends_on_issue_id NOT IN (SELECT id FROM issues))`,
	"labels":               `DELETE FROM labels WHERE issue_id NOT IN (SELECT id FROM issues)`,
	"comments":             `DELETE FROM comments WHERE issue_id NOT IN (SELECT id FROM issues)`,
	"events":               `DELETE FROM events WHERE issue_id NOT IN (SELECT id FROM issues)`,
	"issue_snapshots":      `DELETE FROM issue_snapshots WHERE issue_id NOT IN (SELECT id FROM issues)`,
	"compaction_snapshots": `DELETE FROM compaction_snapshots WHERE issue_id NOT IN (SELECT id FROM issues)`,
	"child_counters":       `DELETE FROM child_counters WHERE parent_id NOT IN (SELECT id FROM issues)`,
}

// tryRepairFKCascadeViolations repairs the post-merge foreign-key constraint
// violations produced by the delete-vs-insert cascade hazard (bd-6dnrw.4): for
// every violating table it deletes the rows whose issue reference dangles,
// clears that table's dolt_constraint_violations entries, and stages the
// table. The caller's transaction must run with
// @@dolt_force_transaction_commit=1 for the merge to survive long enough to be
// repaired, and must NOT commit when (repaired=false, had=true) — unrepaired
// violations are the operator's.
//
// Returns (repaired, had):
//   - (false, false): no violations — nothing to do.
//   - (true, true): every violation was an issues-FK violation on a known
//     synced child table, and all were repaired and cleared.
//   - (false, true): violations of another shape (different constraint type,
//     unknown table, FK to a different parent) — nothing was touched.
func (s *DoltStore) tryRepairFKCascadeViolations(ctx context.Context, tx *sql.Tx) (repaired, had bool, err error) {
	tables, err := constraintViolationTables(ctx, tx)
	if err != nil {
		return false, false, err
	}
	if len(tables) == 0 {
		return false, false, nil
	}

	// Validate every violating table before touching any of them.
	for _, t := range tables {
		if _, ok := fkCascadeRepairDeletes[t]; !ok {
			return false, true, nil
		}
		issueFKOnly, err := violationsAreIssueFKOnly(ctx, tx, t)
		if err != nil {
			return false, true, err
		}
		if !issueFKOnly {
			return false, true, nil
		}
	}

	for _, t := range tables {
		res, err := tx.ExecContext(ctx, fkCascadeRepairDeletes[t])
		if err != nil {
			return false, true, fmt.Errorf("cascade-repair %s: %w", t, err)
		}
		n, _ := res.RowsAffected()
		// t is from the fixed fkCascadeRepairDeletes allowlist, never user input.
		//nolint:gosec // G201/G202: hardcoded table name.
		if _, err := tx.ExecContext(ctx, "DELETE FROM dolt_constraint_violations_"+t); err != nil {
			return false, true, fmt.Errorf("clear %s constraint violations: %w", t, err)
		}
		//nolint:gosec // G202: hardcoded table name.
		if _, err := tx.ExecContext(ctx, "CALL DOLT_ADD('"+t+"')"); err != nil {
			return false, true, fmt.Errorf("stage repaired %s: %w", t, err)
		}
		fmt.Fprintf(os.Stderr,
			"Notice: pull merged %s row(s) referencing issue(s) deleted on another clone; applied the foreign key's cascade delete (%d row(s) removed)\n",
			t, n)
	}

	// The repair must leave nothing behind: a residual violation here means the
	// deletes above did not cover the constraint that fired, and committing
	// would persist a violated working set.
	remaining, err := constraintViolationTables(ctx, tx)
	if err != nil {
		return false, true, err
	}
	if len(remaining) > 0 {
		return false, true, nil
	}
	return true, true, nil
}

// constraintViolationTables lists the tables with outstanding constraint
// violations in the working set.
func constraintViolationTables(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT `table` FROM dolt_constraint_violations WHERE num_violations > 0")
	if err != nil {
		return nil, fmt.Errorf("query constraint violations: %w", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scan constraint violation: %w", err)
		}
		tables = append(tables, t)
	}
	return tables, rows.Err()
}

// violationsAreIssueFKOnly reports whether every constraint violation recorded
// for table is a foreign-key violation referencing issues — the only class the
// cascade repair understands. violation_info is Dolt's JSON descriptor; its
// ReferencedTable names the FK's parent.
func violationsAreIssueFKOnly(ctx context.Context, tx *sql.Tx, table string) (bool, error) {
	// table is from the fixed fkCascadeRepairDeletes allowlist, never user input.
	//nolint:gosec // G202: hardcoded table name.
	rows, err := tx.QueryContext(ctx,
		"SELECT violation_type, violation_info FROM dolt_constraint_violations_"+table)
	if err != nil {
		return false, fmt.Errorf("query %s constraint violations: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var vtype, vinfo string
		if err := rows.Scan(&vtype, &vinfo); err != nil {
			return false, fmt.Errorf("scan %s constraint violation: %w", table, err)
		}
		if vtype != "foreign key" {
			return false, nil
		}
		var info struct {
			ReferencedTable string `json:"ReferencedTable"`
		}
		if err := json.Unmarshal([]byte(vinfo), &info); err != nil {
			return false, nil // unknown descriptor shape — operator decides
		}
		if info.ReferencedTable != "issues" {
			return false, nil
		}
	}
	return true, rows.Err()
}
