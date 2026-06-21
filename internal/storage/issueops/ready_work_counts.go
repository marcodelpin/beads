package issueops

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
)

// CountReadyWorkInTx returns the number of ready issues (plus ready wisps)
// matching filter, WITHOUT hydrating any rows. It builds the SAME ready WHERE
// clause GetReadyWork uses (via sqlbuild.BuildReadyWorkWhere) and runs a single
// `SELECT COUNT(*) FROM issues WHERE <ready predicate>` — no ORDER BY, no LIMIT,
// no labels/deps/comment JSON aggregation.
//
// This is the cheap path the truncation footer ("Showing N of M ready issues")
// needs: the old code re-ran GetReadyWorkWithCounts with Limit=0, which forced
// the counts mega-query (5 full-table GROUP BY LEFT JOINs) to hydrate ALL ready
// rows just to learn their cardinality (sys-56cls: ~12s of a 21s `bd ready -n5
// --json` on the System db's 1636 ready issues). The filter's Limit is ignored
// here — a count is over the whole matching set by definition.
//
// NOTE: it builds the WHERE args itself (BuildReadyWorkWhere) rather than reusing
// buildReadyWorkPredicates, because the latter appends ORDER BY args to its arg
// slice; a COUNT(*) has no ORDER BY, so those surplus args would mismatch the ?
// placeholders.
func CountReadyWorkInTx(ctx context.Context, tx DBTX, filter types.WorkFilter) (int, error) {
	countFilter := filter
	countFilter.Limit = 0 // count the whole matching set, never a page

	var inputs sqlbuild.ReadyWorkWhereInputs
	if !countFilter.IncludeDeferred {
		deferredChildIDs, dcErr := getChildrenOfDeferredParentsInTx(ctx, tx)
		if dcErr != nil {
			return 0, fmt.Errorf("count ready work: compute deferred parent children: %w", dcErr)
		}
		inputs.DeferredChildIDs = deferredChildIDs
	}
	if countFilter.ParentID != nil {
		descendantIDs, descErr := GetDescendantIDsInTx(ctx, tx, *countFilter.ParentID, 0)
		if descErr != nil {
			return 0, fmt.Errorf("count ready work: get parent descendants: %w", descErr)
		}
		inputs.ParentDescendantIDs = descendantIDs
	}

	whereSQL, args, err := sqlbuild.BuildReadyWorkWhere(countFilter, IssuesFilterTables, inputs)
	if err != nil {
		return 0, fmt.Errorf("count ready work: build where: %w", err)
	}

	//nolint:gosec // G201: whereSQL is built from hardcoded fragments + ? placeholders.
	countSQL := fmt.Sprintf("SELECT COUNT(*) FROM issues %s", whereSQL)
	var issueCount int
	if err := tx.QueryRowContext(ctx, countSQL, args...).Scan(&issueCount); err != nil {
		return 0, fmt.Errorf("count ready work: %w", err)
	}

	// Ready wisps get the same in-Go filtering GetReadyWork applies, so reuse it
	// rather than a divergent COUNT. On a wisp-free db (the common case) the wisp
	// probe short-circuits to empty, so this adds one cheap LIMIT-1 probe.
	wisps, err := getReadyWispsInTx(ctx, tx, countFilter, inputs.DeferredChildIDs)
	if err != nil {
		return 0, fmt.Errorf("count ready work: ready wisps: %w", err)
	}

	return issueCount + len(wisps), nil
}

func GetReadyWorkWithCountsInTx(ctx context.Context, tx *sql.Tx, filter types.WorkFilter) ([]*types.IssueWithCounts, error) {
	wispDepsExist, err := optionalTableExistsInTx(ctx, tx, "wisp_dependencies")
	if err != nil {
		return nil, fmt.Errorf("get ready work with counts: wisp dependency probe: %w", err)
	}

	issuePreds, err := buildReadyWorkPredicates(ctx, tx, filter, IssuesFilterTables)
	if err != nil {
		return nil, err
	}
	out, err := runSearchQueryInTx(ctx, tx, IssuesFilterTables, issuePreds.whereSQL, issuePreds.orderBySQL, issuePreds.limitSQL, issuePreds.args, wispDepsExist, false)
	if err != nil {
		return nil, err
	}

	empty, probeErr := wispsTableEmptyOrMissingInTx(ctx, tx)
	if probeErr != nil {
		return nil, fmt.Errorf("get ready work with counts: wisp probe: %w", probeErr)
	}
	if empty {
		return out, nil
	}
	if !wispDepsExist {
		return out, nil
	}

	wispPreds, err := buildReadyWorkPredicates(ctx, tx, filter, WispsFilterTables)
	if err != nil {
		return nil, err
	}
	wisps, err := runSearchQueryInTx(ctx, tx, WispsFilterTables, wispPreds.whereSQL, wispPreds.orderBySQL, wispPreds.limitSQL, wispPreds.args, true, false)
	if err != nil {
		if isTableNotExistError(err) {
			return out, nil
		}
		return nil, err
	}
	if len(wisps) == 0 {
		return out, nil
	}

	// Prefer the canonical wisp record when an ID exists in both tables (be-iabdi).
	wispByID := make(map[string]struct{}, len(wisps))
	for _, w := range wisps {
		if w != nil && w.Issue != nil {
			wispByID[w.Issue.ID] = struct{}{}
		}
	}
	var kept []*types.IssueWithCounts
	for _, iwc := range out {
		if iwc == nil || iwc.Issue == nil {
			kept = append(kept, iwc)
			continue
		}
		if _, dup := wispByID[iwc.Issue.ID]; !dup {
			kept = append(kept, iwc)
		}
	}
	kept = append(kept, wisps...)
	sortIssuesWithCountsByPolicy(kept, filter.SortPolicy)
	if filter.Limit > 0 && len(kept) > filter.Limit {
		kept = kept[:filter.Limit]
	}
	return kept, nil
}

func sortIssuesWithCountsByPolicy(items []*types.IssueWithCounts, policy types.SortPolicy) {
	if len(items) <= 1 {
		return
	}
	issues := make([]*types.Issue, 0, len(items))
	for _, item := range items {
		if item == nil || item.Issue == nil {
			continue
		}
		issues = append(issues, item.Issue)
	}
	if len(issues) != len(items) {
		return
	}
	sortReadyIssues(issues, policy)
	byID := make(map[string]int, len(issues))
	for i, iss := range issues {
		byID[iss.ID] = i
	}
	sorted := make([]*types.IssueWithCounts, len(items))
	for _, item := range items {
		sorted[byID[item.Issue.ID]] = item
	}
	copy(items, sorted)
}

// ScanReadyWorkRowWithCounts scans one row of the counts mega-query
// (sqlbuild.SearchCountsSQL): IssueSelectColumns followed by labels JSON,
// dep/rdep/comment counts, parent ID, and dependency JSON. Exported so the
// domain/db stack hydrates counts rows through the exact same code path.
func ScanReadyWorkRowWithCounts(rows *sql.Rows) (*types.IssueWithCounts, error) {
	var labelsJSON, depsJSON sql.NullString
	var parentID sql.NullString
	var depCount, rdepCount, commentCount sql.NullInt64

	composite := &compositeReadyRow{
		row: rows,
		extra: []any{
			&labelsJSON,
			&depCount,
			&rdepCount,
			&commentCount,
			&parentID,
			&depsJSON,
		},
	}
	issue, err := ScanIssueFrom(composite)
	if err != nil {
		return nil, fmt.Errorf("scan issue with counts: %w", err)
	}

	if labelsJSON.Valid && labelsJSON.String != "" {
		var labels []string
		if err := json.Unmarshal([]byte(labelsJSON.String), &labels); err != nil {
			return nil, fmt.Errorf("scan issue with counts: parse labels_json: %w", err)
		}
		sort.Strings(labels)
		issue.Labels = labels
	}

	if depsJSON.Valid && depsJSON.String != "" {
		var deps []*types.Dependency
		if err := json.Unmarshal([]byte(depsJSON.String), &deps); err != nil {
			return nil, fmt.Errorf("scan issue with counts: parse deps_json: %w", err)
		}
		issue.Dependencies = deps
	}

	iwc := &types.IssueWithCounts{
		Issue:           issue,
		DependencyCount: int(depCount.Int64),
		DependentCount:  int(rdepCount.Int64),
		CommentCount:    int(commentCount.Int64),
	}
	if parentID.Valid {
		s := parentID.String
		iwc.Parent = &s
	}
	return iwc, nil
}

type compositeReadyRow struct {
	row   *sql.Rows
	extra []any
}

func (c *compositeReadyRow) Scan(dest ...any) error {
	combined := make([]any, 0, len(dest)+len(c.extra))
	combined = append(combined, dest...)
	combined = append(combined, c.extra...)
	return c.row.Scan(combined...)
}
