package versioncontrolops

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Domain-aware auto-merge (federation ask #1, the flagship).
//
// Dolt merges disjoint writes cleanly, but beads stamps `issues.updated_at` on
// EVERY mutation, so any two edits to the same issue on two replicas between
// syncs collide on that cell — even when the semantic fields are disjoint
// (machine A adds a comment, machine B adds a label, and the issues row
// conflicts on nothing but the timestamp both bumped). The observed conflict
// rate is therefore far higher than the semantic-conflict rate, and the
// original row-level LWW resolver could only take the safe half of it: it
// declined whenever BOTH sides had moved updated_at past the merge base,
// because taking one side's whole row would silently drop the other side's
// field-level edits.
//
// This file replaces that with a FIELD-level three-way merge, which encodes
// beads' actual write semantics:
//
//   - a column only one side changed relative to the merge base keeps that
//     side's value — no edit is dropped, whatever the timestamps say;
//   - a column both sides changed to DIFFERENT values is the only genuine
//     conflict, and it is settled last-write-wins by `updated_at` (the ask's
//     rule for status/assignee/updated_at, applied uniformly);
//   - `updated_at` itself therefore merges to max(ours, theirs), since
//     whichever side is newer either wins the cell outright (both moved) or is
//     the only side that moved it.
//
// A row is left for the operator when it is not modify/modify (add/add,
// delete/modify), or when a genuinely conflicting cell cannot be settled
// because the two sides' `updated_at` values are equal or unparseable — the
// ambiguity that LWW has no answer for.
//
// The companion tables merge by the semantics the ask names:
//
//   - labels: SET-UNION. `labels` is all key columns (issue_id, label), so
//     two sides adding DIFFERENT labels are disjoint rows dolt already unions,
//     and a conflict can only mean the same (issue_id, label) on both sides —
//     identical data, resolvable by keeping it.
//   - comments/events: APPEND-ONLY UNION. Rows are insert-only and keyed by a
//     per-machine-unique id, so creation is disjoint and dolt unions it; a
//     same-id conflict whose columns agree is the same append on both sides
//     and is likewise resolvable by keeping it.
//
// For all three, a conflict where the row is missing on one side (a deletion
// racing an insert — compaction, or a label removal) or where the columns of a
// supposedly immutable row disagree is NOT unioned: it goes to the operator,
// because both "presence wins" and "deletion wins" would silently discard a
// real intent.

// unionConflictKeyColumns lists the primary-key columns of the tables merged by
// union semantics. The key columns are what identify a conflicted row for the
// dolt_conflicts_<table> delete that signals resolution.
var unionConflictKeyColumns = map[string][]string{
	"labels":   {"issue_id", "label"},
	"comments": {"id"},
	"events":   {"id"},
}

// issuesKeyColumn is the issues-table primary key, used both to identify a
// conflicted row and to exclude the key from the merge write-back.
const issuesKeyColumn = "id"

// issuesRowMerge is the field-level merge decision for one conflicted issues
// row: the columns whose merged value differs from OUR working-set value, and
// the raw values to write.
type issuesRowMerge struct {
	ourKey  any
	columns []string
	values  []any
}

// loadConflictRows reads every live conflict row of table in raw scanned form.
func loadConflictRows(ctx context.Context, db DBConn, table string) ([]rawConflictRow, error) {
	if err := ValidateConflictTable(table); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "SELECT * FROM `dolt_conflicts_"+table+"`") //nolint:gosec // table validated as an identifier above
	if err != nil {
		return nil, fmt.Errorf("query conflicts for table %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("conflict columns for table %s: %w", table, err)
	}
	var out []rawConflictRow
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("scan conflict row for table %s: %w", table, err)
		}
		out = append(out, rawConflictRow{cols: cols, vals: vals})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conflicts for table %s: %w", table, err)
	}
	return out, nil
}

// dataColumns returns the row's data column names (conflict metadata and the
// named excluded columns dropped), in conflict-table order and de-duplicated.
// A column is only reported when the row actually carries a value for it on
// every side that matters; callers read the sides they need with value().
func (r rawConflictRow) dataColumns(exclude ...string) []string {
	skip := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		skip[e] = true
	}
	seen := make(map[string]bool, len(r.cols))
	var out []string
	for _, c := range r.cols {
		side, field, ok := splitConflictColumn(c)
		if !ok || side != "our" || conflictMetaSuffixes[field] || skip[field] || seen[field] {
			continue
		}
		seen[field] = true
		out = append(out, field)
	}
	return out
}

// sidesPresent reports whether the row exists on the base, our, and their
// sides, judged by the key columns (a dolt conflict row NULLs out every column
// of a side that has no row).
func (r rawConflictRow) sidesPresent(keyCols []string) (base, ours, theirs bool) {
	present := func(side string) bool {
		for _, k := range keyCols {
			v, ok := r.value(side, k)
			if !ok || v == nil {
				return false
			}
		}
		return true
	}
	return present("base"), present("our"), present("their")
}

// conflictCellsEqual compares two raw conflict cell values through the same
// normalization the presentation layer uses, so a driver returning []byte for
// one side and string for the other does not read as a difference. SQL NULL is
// distinct from the empty string.
func conflictCellsEqual(a, b any) bool {
	x, y := formatConflictValue(a), formatConflictValue(b)
	if x == nil || y == nil {
		return x == nil && y == nil
	}
	return *x == *y
}

// conflictTimestampLayouts are the shapes an `updated_at` cell can arrive in:
// RFC3339 (what formatConflictValue renders a driver-parsed time.Time as) and
// the two MySQL DATETIME text forms (drivers configured without parseTime).
var conflictTimestampLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
}

// parseConflictTimestamp parses a raw conflict cell as a timestamp. ok is
// false for NULL or any unrecognized shape — an unparseable timestamp must
// make LWW decline, never guess.
func parseConflictTimestamp(v any) (time.Time, bool) {
	if t, isTime := v.(time.Time); isTime {
		return t.UTC(), true
	}
	s := formatConflictValue(v)
	if s == nil || *s == "" {
		return time.Time{}, false
	}
	for _, layout := range conflictTimestampLayouts {
		if t, err := time.Parse(layout, *s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// mergeIssuesConflictRow computes the field-level three-way merge of one
// conflicted issues row. ok is false when the row must be left for the
// operator: not modify/modify, or a cell both sides changed differently whose
// LWW tiebreak is ambiguous (equal or unparseable updated_at).
//
// It is pure, so every merge rule is unit-testable without a database.
func mergeIssuesConflictRow(row rawConflictRow) (issuesRowMerge, bool) {
	baseOK, ourOK, theirOK := row.sidesPresent([]string{issuesKeyColumn})
	if !baseOK || !ourOK || !theirOK {
		// add/add (no base row) or delete/modify (one side removed it):
		// neither has a field-level answer.
		return issuesRowMerge{}, false
	}
	ourKey, _ := row.value("our", issuesKeyColumn)

	ourUpdated, ourTimeOK := parseConflictTimestamp(mustValue(row, "our", "updated_at"))
	theirUpdated, theirTimeOK := parseConflictTimestamp(mustValue(row, "their", "updated_at"))

	merge := issuesRowMerge{ourKey: ourKey}
	for _, col := range row.dataColumns(issuesKeyColumn) {
		ourVal, ourHas := row.value("our", col)
		theirVal, theirHas := row.value("their", col)
		if !ourHas || !theirHas {
			// A column present on one side only means the two sides' schemas
			// diverged; that is a schema merge, not a row merge.
			return issuesRowMerge{}, false
		}
		if conflictCellsEqual(ourVal, theirVal) {
			continue // both sides agree on this cell
		}
		baseVal, baseHas := row.value("base", col)
		if !baseHas {
			return issuesRowMerge{}, false
		}
		switch {
		case conflictCellsEqual(theirVal, baseVal):
			// Only we changed it: our working-set value already stands.
			continue
		case conflictCellsEqual(ourVal, baseVal):
			// Only they changed it: take their edit. This is the case
			// row-level LWW used to lose.
			merge.columns = append(merge.columns, col)
			merge.values = append(merge.values, theirVal)
		default:
			// Both sides changed the same cell to different values — the only
			// genuine conflict. Settle it last-write-wins by updated_at.
			if !ourTimeOK || !theirTimeOK || ourUpdated.Equal(theirUpdated) {
				return issuesRowMerge{}, false
			}
			if theirUpdated.After(ourUpdated) {
				merge.columns = append(merge.columns, col)
				merge.values = append(merge.values, theirVal)
			}
		}
	}
	return merge, true
}

// mustValue reads a side's column, returning nil when the conflict table has
// no such column (the caller's rules then treat it as absent/unparseable).
func mustValue(row rawConflictRow, side, col string) any {
	v, _ := row.value(side, col)
	return v
}

// issuesConflictsAreFieldMergeable reports whether every live issues conflict
// can be settled by mergeIssuesConflictRow, and returns the merge plan so the
// resolution pass does not recompute it.
func issuesConflictsAreFieldMergeable(ctx context.Context, db DBConn) ([]issuesRowMerge, bool, error) {
	rows, err := loadConflictRows(ctx, db, "issues")
	if err != nil {
		return nil, false, err
	}
	plan := make([]issuesRowMerge, 0, len(rows))
	for _, row := range rows {
		merged, ok := mergeIssuesConflictRow(row)
		if !ok {
			return nil, false, nil
		}
		plan = append(plan, merged)
	}
	return plan, true, nil
}

// resolveIssuesFieldMerge applies a plan from issuesConflictsAreFieldMergeable.
//
// DOLT_CONFLICTS_RESOLVE is table-level (--ours/--theirs), which cannot express
// a per-cell merge, so this uses dolt's manual-resolution path: write the
// merged values over our working-set row, then DELETE the conflict row — the
// delete is what tells dolt the row is settled, so it must come last. A row
// whose merge equals our side needs no write at all.
func resolveIssuesFieldMerge(ctx context.Context, db DBConn, plan []issuesRowMerge) error {
	for _, m := range plan {
		if m.ourKey == nil {
			return fmt.Errorf("unexpected conflict row with no issue id (safety check bypassed)")
		}
		if len(m.columns) > 0 {
			sets := make([]string, len(m.columns))
			args := make([]any, 0, len(m.columns)+1)
			for i, col := range m.columns {
				// MySQL cannot bind an identifier and a peer's schema merge can
				// extend the conflict table's columns, so gate every name the
				// same way the table name is gated.
				if err := ValidateConflictTable(col); err != nil {
					return fmt.Errorf("refusing to write unexpected column %q of issues: %w", col, err)
				}
				sets[i] = fmt.Sprintf("`%s` = ?", col)
				args = append(args, m.values[i])
			}
			args = append(args, m.ourKey)
			stmt := fmt.Sprintf("UPDATE `issues` SET %s WHERE `%s` = ?", strings.Join(sets, ", "), issuesKeyColumn) //nolint:gosec // identifiers validated above
			res, err := db.ExecContext(ctx, stmt, args...)
			if err != nil {
				return fmt.Errorf("apply merged values for issue %v: %w", m.ourKey, err)
			}
			// Zero rows means the row we planned against is gone — another
			// session deleted it between the read and the write. Clearing the
			// conflict now would discard their side undetectably.
			if n, err := res.RowsAffected(); err == nil && n == 0 {
				return fmt.Errorf("merged values for issue %v matched no row (was it deleted concurrently?); conflict left unresolved", m.ourKey)
			}
		}
		res, err := db.ExecContext(ctx,
			"DELETE FROM dolt_conflicts_issues WHERE our_"+issuesKeyColumn+" = ?", m.ourKey)
		if err != nil {
			return fmt.Errorf("clear conflict for issue %v: %w", m.ourKey, err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return fmt.Errorf("conflict for issue %v was not cleared (no conflict row deleted)", m.ourKey)
		}
	}
	return nil
}

// unionConflictsAreSafe reports whether every live conflict of a union-merged
// table (labels, comments, events) is the same row on both sides with matching
// columns — the only class where "union" has an unambiguous answer. A row
// missing on one side (a deletion racing an insert) or diverging columns in a
// supposedly immutable row goes to the operator.
func unionConflictsAreSafe(ctx context.Context, db DBConn, table string) (bool, error) {
	keyCols, ok := unionConflictKeyColumns[table]
	if !ok {
		return false, fmt.Errorf("table %s is not union-mergeable", table)
	}
	rows, err := loadConflictRows(ctx, db, table)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		_, ourOK, theirOK := row.sidesPresent(keyCols)
		if !ourOK || !theirOK {
			return false, nil
		}
		for _, col := range row.dataColumns() {
			ourVal, ourHas := row.value("our", col)
			theirVal, theirHas := row.value("their", col)
			if !ourHas || !theirHas || !conflictCellsEqual(ourVal, theirVal) {
				return false, nil
			}
		}
	}
	return true, nil
}

// resolveUnionConflicts settles the conflicts unionConflictsAreSafe validated.
// Both sides hold the same row, so our working set already carries the union:
// deleting the conflict row is the whole resolution.
func resolveUnionConflicts(ctx context.Context, db DBConn, table string) error {
	keyCols, ok := unionConflictKeyColumns[table]
	if !ok {
		return fmt.Errorf("table %s is not union-mergeable", table)
	}
	rows, err := loadConflictRows(ctx, db, table)
	if err != nil {
		return err
	}
	for _, row := range rows {
		preds := make([]string, 0, len(keyCols))
		args := make([]any, 0, len(keyCols))
		for _, k := range keyCols {
			v, has := row.value("our", k)
			if !has || v == nil {
				return fmt.Errorf("unexpected %s conflict row with no our_%s (safety check bypassed)", table, k)
			}
			preds = append(preds, "`our_"+k+"` = ?")
			args = append(args, v)
		}
		//nolint:gosec // table and key columns come from the unionConflictKeyColumns allowlist.
		stmt := "DELETE FROM `dolt_conflicts_" + table + "` WHERE " + strings.Join(preds, " AND ")
		res, err := db.ExecContext(ctx, stmt, args...)
		if err != nil {
			return fmt.Errorf("clear %s conflict: %w", table, err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return fmt.Errorf("a %s conflict was not cleared (no conflict row deleted)", table)
		}
	}
	return nil
}
