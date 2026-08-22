package issueops

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/types"
)

// GetLabelsInTx retrieves all labels for an issue within an existing transaction.
// Automatically routes to wisp_labels if the ID is an active wisp.
// Returns labels sorted alphabetically.
func GetLabelsInTx(ctx context.Context, tx DBTX, table, issueID string) ([]string, error) {
	if table == "" {
		isWisp := IsActiveWispInTx(ctx, tx, issueID)
		_, table, _, _ = WispTableRouting(isWisp)
	}
	//nolint:gosec // G201: table is from WispTableRouting ("labels" or "wisp_labels")
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT label FROM %s WHERE issue_id = ? ORDER BY label`, table), issueID)
	if err != nil {
		return nil, fmt.Errorf("get labels: %w", err)
	}
	defer rows.Close()

	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, fmt.Errorf("get labels: scan: %w", err)
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

// GetLabelsForIssuesInTx fetches labels for multiple issues in a single transaction.
// Routes each ID to labels or wisp_labels based on wisp status.
// Uses a single batched wisp-partition query plus batched IN clauses per label
// table, so the number of round-trips is O(1 + N/queryBatchSize) rather than
// O(N). This matters on remote backends (Dolt) where per-ID round-trips would
// otherwise blow past the context deadline — see GH#3414.
//
// Callers hydrating multiple batches inside one tx may pass a precomputed
// active-wisp set scoped to issueIDs to avoid rebuilding it.
func GetLabelsForIssuesInTx(ctx context.Context, tx DBTX, issueIDs []string, wispSetOpt ...map[string]struct{}) (map[string][]string, error) {
	if len(issueIDs) == 0 {
		return make(map[string][]string), nil
	}

	var wispIDs, permIDs []string
	if len(wispSetOpt) > 0 && wispSetOpt[0] != nil {
		wispIDs, permIDs = partitionByWispSet(issueIDs, wispSetOpt[0])
	} else {
		var err error
		wispIDs, permIDs, err = PartitionWispIDsInTx(ctx, tx, issueIDs)
		if err != nil {
			return nil, err
		}
	}

	result := make(map[string][]string)
	if len(wispIDs) > 0 {
		if err := getLabelsIntoFromTable(ctx, tx, "wisp_labels", wispIDs, result); err != nil {
			return nil, err
		}
	}
	if len(permIDs) > 0 {
		if err := getLabelsIntoFromTable(ctx, tx, "labels", permIDs, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// GetLabelsForIssuesFromTableInTx is a fast path for callers that already know
// which label table applies to every ID in the batch (e.g. searchTableInTx,
// which queries either the issues or wisps table exclusively). It skips the
// wisp-partition round-trip entirely. labelTable must be "labels" or
// "wisp_labels"; callers route via FilterTables.
func GetLabelsForIssuesFromTableInTx(ctx context.Context, tx DBTX, labelTable string, issueIDs []string) (map[string][]string, error) {
	if len(issueIDs) == 0 {
		return make(map[string][]string), nil
	}
	result := make(map[string][]string)
	if err := getLabelsIntoFromTable(ctx, tx, labelTable, issueIDs, result); err != nil {
		return nil, err
	}
	return result, nil
}

// getLabelsIntoFromTable executes the batched SELECT for a single label table
// and accumulates results into the provided map.
//
//nolint:gosec // G201: labelTable is "labels" or "wisp_labels" (hardcoded by callers).
func getLabelsIntoFromTable(ctx context.Context, tx DBTX, labelTable string, ids []string, result map[string][]string) error {
	for start := 0; start < len(ids); start += queryBatchSize {
		end := start + queryBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(
			`SELECT issue_id, label FROM %s WHERE issue_id IN (%s) ORDER BY issue_id, label`,
			labelTable, strings.Join(placeholders, ",")), args...)
		if err != nil {
			return fmt.Errorf("get labels for issues from %s: %w", labelTable, err)
		}
		for rows.Next() {
			var issueID, label string
			if err := rows.Scan(&issueID, &label); err != nil {
				_ = rows.Close()
				return fmt.Errorf("get labels for issues: scan: %w", err)
			}
			result[issueID] = append(result[issueID], label)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("get labels for issues: rows: %w", err)
		}
	}
	return nil
}

// AddLabelInTx adds a label to an issue and records an event within an existing
// transaction. Automatically routes to wisp tables if the ID is an active wisp.
// Uses INSERT IGNORE for idempotency.
func AddLabelInTx(ctx context.Context, tx DBTX, labelTable, eventTable, issueID, label, actor string) error {
	// Reject an over-length label up front. The INSERT IGNORE below would
	// otherwise silently truncate it to the VARCHAR(255) column, storing a label
	// the caller never sent; a typed ErrFieldTooLong is the clean rejection.
	if err := types.CheckFieldLen("label", label); err != nil {
		return err
	}
	if labelTable == "" || eventTable == "" {
		isWisp := IsActiveWispInTx(ctx, tx, issueID)
		_, lt, et, _ := WispTableRouting(isWisp)
		if labelTable == "" {
			labelTable = lt
		}
		if eventTable == "" {
			eventTable = et
		}
	}
	//nolint:gosec // G201: labelTable is from WispTableRouting ("labels" or "wisp_labels")
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT IGNORE INTO %s (issue_id, label) VALUES (?, ?)`, labelTable), issueID, label); err != nil {
		return fmt.Errorf("add label: %w", err)
	}
	comment := "Added label: " + label
	if err := InsertDerivedEvent(ctx, tx, eventTable, AuxEvent{
		IssueID:   issueID,
		EventType: types.EventLabelAdded,
		Actor:     actor,
		Comment:   str(comment),
	}); err != nil {
		return fmt.Errorf("add label: record event: %w", err)
	}
	// A label is part of the bead snapshot, so a label write journals as an
	// update carrying the complete post-mutation set.
	return RecordEventInTx(ctx, tx, EventUpdate, issueID)
}

// RemoveLabelInTx removes a label from an issue and records an event within
// an existing transaction. Automatically routes to wisp tables if the ID is
// an active wisp.
//
//nolint:gosec // G201: table names come from WispTableRouting (hardcoded constants)
func RemoveLabelInTx(ctx context.Context, tx DBTX, labelTable, eventTable, issueID, label, actor string) error {
	if labelTable == "" || eventTable == "" {
		isWisp := IsActiveWispInTx(ctx, tx, issueID)
		_, lt, et, _ := WispTableRouting(isWisp)
		if labelTable == "" {
			labelTable = lt
		}
		if eventTable == "" {
			eventTable = et
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE issue_id = ? AND label = ?`, labelTable), issueID, label); err != nil {
		return fmt.Errorf("remove label: %w", err)
	}
	comment := "Removed label: " + label
	if err := InsertDerivedEvent(ctx, tx, eventTable, AuxEvent{
		IssueID:   issueID,
		EventType: types.EventLabelRemoved,
		Actor:     actor,
		Comment:   str(comment),
	}); err != nil {
		return fmt.Errorf("remove label: record event: %w", err)
	}
	return RecordEventInTx(ctx, tx, EventUpdate, issueID)
}

// renameLabelPlanes pairs each label table with the event table journaling
// its issues, so RenameLabelInTx sweeps both without hardcoding the pairing
// twice.
var renameLabelPlanes = [2]struct {
	labelTable string
	eventTable string
}{
	{labelTable: "labels", eventTable: "events"},
	{labelTable: "wisp_labels", eventTable: "wisp_events"},
}

// RenameLabelInTx renames a label across every issue and wisp that carries
// it. An issue that already carries newLabel is a merge: the stale oldLabel
// row is dropped rather than raising a duplicate-key error, and merged
// counts the subset of renamed where that happened. oldLabel carried by
// nothing is an honest no-op (renamed, merged both 0, err nil). ids lists
// every touched issue and wisp id, both planes concatenated.
func RenameLabelInTx(ctx context.Context, tx DBTX, oldLabel, newLabel, actor string) (renamed, merged int, ids []string, err error) {
	if err := types.CheckFieldLen("label", newLabel); err != nil {
		return 0, 0, nil, err
	}
	for _, plane := range renameLabelPlanes {
		r, m, planeIDs, err := renameLabelInPlane(ctx, tx, plane.labelTable, plane.eventTable, oldLabel, newLabel, actor)
		if err != nil {
			return 0, 0, nil, fmt.Errorf("rename label in %s: %w", plane.labelTable, err)
		}
		renamed += r
		merged += m
		ids = append(ids, planeIDs...)
	}
	return renamed, merged, ids, nil
}

// renameLabelInPlane sweeps one label table (and its matching event table)
// for oldLabel and returns the touched-id and already-had-newLabel counts.
//
//nolint:gosec // G201: labelTable/eventTable are hardcoded routing constants from renameLabelPlanes ("labels"/"events" or "wisp_labels"/"wisp_events").
func renameLabelInPlane(ctx context.Context, tx DBTX, labelTable, eventTable, oldLabel, newLabel, actor string) (renamed, merged int, ids []string, err error) {
	oldIDs, err := issueIDsWithLabelInTx(ctx, tx, labelTable, oldLabel)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("query issues carrying %q: %w", oldLabel, err)
	}
	if len(oldIDs) == 0 {
		return 0, 0, nil, nil
	}
	newIDs, err := issueIDsWithLabelInTx(ctx, tx, labelTable, newLabel)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("query issues carrying %q: %w", newLabel, err)
	}
	alreadyNew := make(map[string]struct{}, len(newIDs))
	for _, id := range newIDs {
		alreadyNew[id] = struct{}{}
	}
	for _, id := range oldIDs {
		if _, ok := alreadyNew[id]; ok {
			merged++
		}
	}
	// INSERT IGNORE so an issue that already carries newLabel silently no-ops
	// here instead of raising a duplicate-key error - this IS the merge.
	for start := 0; start < len(oldIDs); start += queryBatchSize {
		end := start + queryBatchSize
		if end > len(oldIDs) {
			end = len(oldIDs)
		}
		batch := oldIDs[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, 0, len(batch)*2)
		for i, id := range batch {
			placeholders[i] = "(?, ?)"
			args = append(args, id, newLabel)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`INSERT IGNORE INTO %s (issue_id, label) VALUES %s`,
			labelTable, strings.Join(placeholders, ",")), args...); err != nil {
			return 0, 0, nil, fmt.Errorf("insert renamed label: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM %s WHERE label = ?`, labelTable), oldLabel); err != nil {
		return 0, 0, nil, fmt.Errorf("delete old label rows: %w", err)
	}
	comment := fmt.Sprintf("Renamed label: '%s' to '%s'", oldLabel, newLabel)
	for _, id := range oldIDs {
		if err := InsertDerivedEvent(ctx, tx, eventTable, AuxEvent{
			IssueID:   id,
			EventType: types.EventLabelRenamed,
			Actor:     actor,
			OldValue:  str(oldLabel),
			NewValue:  str(newLabel),
			Comment:   str(comment),
		}); err != nil {
			return 0, 0, nil, fmt.Errorf("rename label: record event for %s: %w", id, err)
		}
		if err := RecordEventInTx(ctx, tx, EventUpdate, id); err != nil {
			return 0, 0, nil, fmt.Errorf("rename label: journal %s: %w", id, err)
		}
	}
	return len(oldIDs), merged, oldIDs, nil
}

// issueIDsWithLabelInTx returns every issue/wisp id carrying label in the
// given label table (unordered).
//
//nolint:gosec // G201: labelTable is a hardcoded routing constant ("labels" or "wisp_labels").
func issueIDsWithLabelInTx(ctx context.Context, tx DBTX, labelTable, label string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT issue_id FROM %s WHERE label = ?`, labelTable), label)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan issue id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
