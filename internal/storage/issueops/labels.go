package issueops

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/labelns"
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
	if err := EnforceExclusiveLabelInTx(ctx, tx, labelTable, issueID, label); err != nil {
		return err
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

// EnforceExclusiveLabelInTx rejects the add when the label falls in a
// configured exclusive namespace (labels.exclusive-prefixes, bd-7u5ki) the
// issue already carries a different label in. It lives at the shared insert
// choke point so every interactive mutation path - bd label add, bd update
// --add-label, bd label propagate, bd set-state - gets the same guard. With
// no prefixes configured (the default) only the config lookup runs.
//
// EXPORTED so internal/storage/domain/db.labelSQLRepositoryImpl.Insert (the
// proxied/UOW write path's raw INSERT IGNORE) can call the SAME check the
// classic AddLabelInTx uses, rather than duplicating it - db.Runner and this
// package's DBTX are structurally identical (see blocked_state.go's DBTX
// doc), so a *sql.Tx from the embedded path and a uow db.Runner both satisfy
// this signature unchanged.
//
// ACQUIRES THE PER-(issueID, namespace) LOCK BEFORE READING existing labels,
// closing the check-then-insert race the review of upstream PR #4757
// (bd-7u5ki) found: two concurrent writers adding DIFFERENT labels into the
// SAME exclusive namespace insert DIFFERENT primary keys in the labels
// table, so neither commit conflicts with the other and both writers'
// "no violation" read stays true forever. Rewriting the lock row's row_lock
// cell forces both writers to collide on ONE shared cell instead, so Dolt's
// commit-time merge surfaces a 1213/1205 serialization error for the loser -
// withRetryTx/RunTx replay the WHOLE transaction against a fresh read, which
// now observes the winner's committed label and correctly rejects it. See
// migration 0066 (label_namespace_locks) for the full mechanism writeup.
//
// KNOWN WINDOW (bda-uu6j): a write whose snapshot predates a concurrent
// `bd config set labels.exclusive-prefixes` enable reads no prefixes here,
// returns before touching the namespace lock, and can commit a violation
// just after enforcement turned on. Closing it would take a global
// config-vs-label-write rendezvous - the whole-row contention the
// per-(issue,namespace) lock exists to avoid - so the window is DISCLOSED
// instead: the config-set side-effect hint and bd doctor's
// FindExclusiveLabelViolations are the detection path.
func EnforceExclusiveLabelInTx(ctx context.Context, tx DBTX, labelTable, issueID, label string) error {
	raw, err := GetConfigInTx(ctx, tx, labelns.ConfigKey)
	if err != nil {
		return fmt.Errorf("add label: %w", err)
	}
	prefixes := labelns.ParsePrefixes(raw)
	if len(prefixes) == 0 {
		return nil
	}
	prefix := labelns.Match(prefixes, label)
	if prefix == "" {
		return nil
	}
	if err := acquireLabelNamespaceLockInTx(ctx, tx, issueID, prefix); err != nil {
		return fmt.Errorf("add label: %w", err)
	}
	existing, err := GetLabelsInTx(ctx, tx, labelTable, issueID)
	if err != nil {
		return fmt.Errorf("add label: %w", err)
	}
	for _, have := range existing {
		if have != label && labelns.Match(prefixes, have) == prefix {
			return fmt.Errorf("cannot add label %q to %s: namespace %q is exclusive (%s) and the issue already has %q - remove it first, or swap in one step with 'bd label add --replace'",
				label, issueID, prefix, labelns.ConfigKey, have)
		}
	}
	return nil
}

// acquireLabelNamespaceLockInTx rewrites the (issueID, namespace) lock row's
// row_lock cell to a fresh value, INSIDE the caller's transaction, mirroring
// the issues/wisps row_lock CAS (see lease.go freshRowLock). The token's
// value carries no meaning; the write's only job is to touch the SAME cell a
// concurrent writer into the same namespace also touches, so the two
// transactions collide at commit time instead of merging silently. Scoped to
// (issueID, namespace) rather than the whole issue row so an unrelated
// status/assignee update never collides with a label add, and vice versa.
func acquireLabelNamespaceLockInTx(ctx context.Context, tx DBTX, issueID, namespace string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO label_namespace_locks (issue_id, namespace, row_lock) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE row_lock = VALUES(row_lock)
	`, issueID, namespace, freshRowLock()); err != nil {
		return fmt.Errorf("acquire label namespace lock: %w", err)
	}
	return nil
}

// exclusiveLabelPrefixFilter builds the SQL fragment restricting the label
// scan to the exclusive namespaces: labels outside them can never contribute
// to a conflict, and on a large corpus they are the overwhelming majority of
// rows. The fragment holds only fixed `l.label LIKE ?` placeholders; prefix
// values travel as args, LIKE metacharacters escaped.
func exclusiveLabelPrefixFilter(prefixes []string) (string, []any) {
	likeClauses := make([]string, 0, len(prefixes))
	args := make([]any, 0, len(prefixes))
	escaper := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	for _, p := range prefixes {
		likeClauses = append(likeClauses, `l.label LIKE ?`)
		args = append(args, escaper.Replace(p)+"%")
	}
	return strings.Join(likeClauses, " OR "), args
}

// ExclusiveLabelViolation reports an issue carrying more than one label in a
// configured exclusive namespace.
type ExclusiveLabelViolation struct {
	IssueID string
	Prefix  string
	Labels  []string
}

// FindExclusiveLabelViolations scans the permanent labels table for
// non-closed issues violating the given exclusive prefixes. bd doctor uses it
// so adopters can find pre-existing violations before (or after) enabling
// labels.exclusive-prefixes — write-path enforcement only guards new label
// additions. Closed issues are skipped (no routing consumer reads them), as
// are wisp labels (wisps are ephemeral and expire via TTL compaction).
// retain bounds how many violations are RETAINED for display; the total is
// always counted in full. retain <= 0 keeps every violation (small corpora,
// tests). The bound exists because the violation list is the one remaining
// allocation that scales with corpus damage (bda-9krh): the doctor check
// shows a fixed number of detail lines and needs only the true total beside
// them.
func FindExclusiveLabelViolations(ctx context.Context, db DBTX, prefixes []string, retain int) ([]ExclusiveLabelViolation, int, error) {
	if len(prefixes) == 0 {
		return nil, 0, nil
	}
	filterSQL, args := exclusiveLabelPrefixFilter(prefixes)
	//nolint:gosec // G202: the concatenated fragment is built from fixed
	// `l.label LIKE ?` placeholders only; prefix values travel as args.
	rows, err := db.QueryContext(ctx, `
		SELECT l.issue_id, l.label
		FROM labels l JOIN issues i ON i.id = l.issue_id
		WHERE i.status != 'closed' AND (`+filterSQL+`)
		ORDER BY l.issue_id, l.label`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("scan exclusive labels: %w", err)
	}
	defer rows.Close()

	// The rows arrive grouped by issue_id, so one issue's labels are held at
	// a time instead of the whole corpus (bda-9krh: the previous map-of-all-
	// issues made routine `bd doctor` memory scale with total label count).
	var violations []ExclusiveLabelViolation
	total := 0
	var curIssue string
	var curLabels []string
	flush := func() {
		for _, c := range labelns.Conflicts(prefixes, curLabels) {
			total++
			if retain <= 0 || len(violations) < retain {
				violations = append(violations, ExclusiveLabelViolation{IssueID: curIssue, Prefix: c.Prefix, Labels: c.Labels})
			}
		}
	}
	for rows.Next() {
		var issueID, label string
		if err := rows.Scan(&issueID, &label); err != nil {
			return nil, 0, fmt.Errorf("scan exclusive labels: %w", err)
		}
		if issueID != curIssue {
			if curIssue != "" {
				flush()
			}
			curIssue = issueID
			curLabels = curLabels[:0]
		}
		curLabels = append(curLabels, label)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("scan exclusive labels: %w", err)
	}
	if curIssue != "" {
		flush()
	}
	return violations, total, nil
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
