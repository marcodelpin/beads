package issueops

import (
	"context"
	"errors"
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
	return RecordEventInTx(ctx, tx, EventUpdate, issueID, actor)
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
	return RecordEventInTx(ctx, tx, EventUpdate, issueID, actor)
}

// renameLabelPlanes pairs each label table with the event table journaling
// its issues, mirroring WispTableRouting's two known planes. Declared once so
// RenameLabelInTx sweeps both without hardcoding the pairing twice.
var renameLabelPlanes = [2]struct {
	labelTable string
	eventTable string
}{
	{labelTable: "labels", eventTable: "events"},
	{labelTable: "wisp_labels", eventTable: "wisp_events"},
}

// ErrRenameLabelSameName is returned when RenameLabelInTx is asked to rename
// a label to itself. Refused before any write: the merge branch below treats
// every carrier of oldLabel as already carrying newLabel (they are the same
// string), so "insert the merge row, then drop the stale oldLabel row"
// degenerates into "insert a row that already exists via INSERT IGNORE, then
// delete every row carrying this label" -- the label is wiped, not renamed.
// Compared after trimming so incidental leading/trailing whitespace on one
// side does not mask the same no-op; the comparison itself stays
// case-sensitive, matching label identity everywhere else in this store.
var ErrRenameLabelSameName = errors.New("rename label: old and new label are the same")

// RenameLabelInTx renames a label across every issue and wisp that carries
// it, sweeping the labels table and then the wisp_labels table within an
// existing transaction. Design: docs/label-taxonomy-best-practices.md Unit A.
//
// The rename DEGRADES TO A MERGE, Linear-style: when an issue already
// carries newLabel, the stale oldLabel row is simply dropped rather than
// raising a duplicate-key error. merged is the subset of renamed where that
// happened, so a caller can report "N issues relabeled, M already had <new>"
// honestly instead of papering over the collision.
//
// oldLabel carried by zero issues is an honest no-op: renamed and merged are
// both 0 and err is nil, matching AddLabelInTx/RemoveLabelInTx's own
// no-op-is-not-an-error convention for a label edit that changes nothing.
//
// ids returns every touched issue and wisp id, both planes concatenated, in
// the order the sweep found them - for a caller that needs to fire a
// per-issue side effect (a hook, a dry-run preview) on exactly what changed.
//
// Emits one label_renamed event plus one full-snapshot journal row per
// touched row, the same per-issue journal shape AddLabelInTx and
// RemoveLabelInTx use (see TestEveryBeadMutatorJournalsOrIsExempt in
// journal_completeness_test.go). A single batch event was considered and
// rejected: the journal completeness guard requires every touched
// work-bead row to be individually replayable, and a batch event has no
// per-issue row to attach that journal entry to.
//
// oldLabel and newLabel equal after trimming is refused with
// ErrRenameLabelSameName rather than treated as a no-op -- see that error's
// doc for why silently proceeding would wipe the label instead of leaving
// it alone.
func RenameLabelInTx(ctx context.Context, tx DBTX, oldLabel, newLabel, actor string) (renamed, merged int, ids []string, err error) {
	if strings.TrimSpace(oldLabel) == strings.TrimSpace(newLabel) {
		return 0, 0, nil, ErrRenameLabelSameName
	}
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
	// Every id in oldIDs now carries newLabel too - freshly inserted above, or
	// already present pre-rename - so dropping every oldLabel row in one
	// DELETE is safe regardless of the merge split computed above.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM %s WHERE label = ?`, labelTable), oldLabel); err != nil {
		return 0, 0, nil, fmt.Errorf("delete old label rows: %w", err)
	}
	comment := fmt.Sprintf("Renamed label: '%s' to '%s'", oldLabel, newLabel)
	// KNOWN COST: this loop is O(len(oldIDs)) SQL round trips, not one batch -
	// InsertDerivedEvent does its own dedup SELECT (a real cross-replica
	// convergence check, Protocol v0.1 C2.3, not a redundant one this
	// transaction could skip: two independently-created rows with identical
	// content must land on the same content-derived id) plus an INSERT, and
	// RecordEventInTx does its own snapshot SELECT plus an INSERT - up to
	// four round trips per touched issue, inside one long transaction. On a
	// huge label population that lengthens the transaction, which raises the
	// odds an optimistic-concurrency retry has to redo the whole rename from
	// scratch rather than just its own small write.
	//
	// Not batched: no batch primitive exists for this shape anywhere in this
	// package (issueops), and building one here would be new shared
	// infrastructure grown for a single caller rather than a mitigation
	// scoped to this fix - AddLabelInTx/RemoveLabelInTx pay the identical
	// per-call cost for their one issue today, and this loop is exactly that
	// cost repeated once per touched issue. Left as a disclosed limitation.
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
		if err := RecordEventInTx(ctx, tx, EventUpdate, id, actor); err != nil {
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
