package issueops

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

func SearchIssuesWithCountsInTx(ctx context.Context, tx *sql.Tx, query string, filter types.IssueFilter) ([]*types.IssueWithCounts, error) {
	wispDepsExist, err := optionalTableExistsInTx(ctx, tx, "wisp_dependencies")
	if err != nil {
		return nil, fmt.Errorf("search issues with counts: wisp dependency probe: %w", err)
	}

	if filter.Ephemeral != nil && *filter.Ephemeral {
		empty, probeErr := wispsTableEmptyOrMissingInTx(ctx, tx)
		if probeErr != nil {
			return nil, fmt.Errorf("search issues with counts: ephemeral wisp probe: %w", probeErr)
		}
		if empty || !wispDepsExist {
			return nil, nil
		}
		wisps, err := runFilterSearchQueryInTx(ctx, tx, query, filter, WispsFilterTables, true)
		if err != nil {
			return nil, err
		}
		return finishSearchIssuesWithCounts(wisps, filter), nil
	}

	out, err := runFilterSearchQueryInTx(ctx, tx, query, filter, IssuesFilterTables, wispDepsExist)
	if err != nil {
		return nil, err
	}

	// Skip wisps merge entirely when caller opts out (Q2: perf escape hatch).
	if filter.SkipWisps {
		return finishSearchIssuesWithCounts(out, filter), nil
	}

	empty, probeErr := wispsTableEmptyOrMissingInTx(ctx, tx)
	if probeErr != nil {
		return nil, fmt.Errorf("search issues with counts: wisp probe: %w", probeErr)
	}
	if empty {
		return finishSearchIssuesWithCounts(out, filter), nil
	}
	if !wispDepsExist {
		return finishSearchIssuesWithCounts(out, filter), nil
	}

	wisps, err := runFilterSearchQueryInTx(ctx, tx, query, filter, WispsFilterTables, true)
	if err != nil {
		if isTableNotExistError(err) {
			return finishSearchIssuesWithCounts(out, filter), nil
		}
		return nil, err
	}
	if len(wisps) == 0 {
		return finishSearchIssuesWithCounts(out, filter), nil
	}

	seen := make(map[string]struct{}, len(out))
	for _, iwc := range out {
		if iwc != nil && iwc.Issue != nil {
			seen[iwc.Issue.ID] = struct{}{}
		}
	}
	for _, w := range wisps {
		if w == nil || w.Issue == nil {
			continue
		}
		if _, dup := seen[w.Issue.ID]; dup {
			return nil, fmt.Errorf("search issues with counts: id %q exists in both issues and wisps", w.Issue.ID)
		}
		out = append(out, w)
	}
	return finishSearchIssuesWithCounts(out, filter), nil
}

func runFilterSearchQueryInTx(ctx context.Context, tx *sql.Tx, query string, filter types.IssueFilter, tables FilterTables, includeWispReverseDeps bool) ([]*types.IssueWithCounts, error) {
	whereClauses, args, err := BuildIssueFilterClauses(query, filter, tables)
	if err != nil {
		return nil, err
	}
	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + joinAnd(whereClauses)
	}
	limitSQL := ""
	if filter.Limit > 0 {
		limitSQL = fmt.Sprintf("LIMIT %d", filter.Limit)
	}
	orderBy := issueOpsOrderBy(filter.SortBy, filter.SortDesc, "i")
	return runSearchQueryInTx(ctx, tx, tables, whereSQL, orderBy, limitSQL, args, includeWispReverseDeps, filter.SkipLabels)
}

//nolint:gosec // G201: SQL fragments are caller-built from hardcoded shapes
func runSearchQueryInTx(ctx context.Context, tx *sql.Tx, tables FilterTables, whereSQL, orderBySQL, limitSQL string, args []interface{}, includeWispReverseDeps bool, skipLabels bool) ([]*types.IssueWithCounts, error) {
	reverseBlockerSelect := `
				SELECT COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) AS dep_id
				FROM dependencies WHERE type = 'blocks'
	`
	if includeWispReverseDeps {
		reverseBlockerSelect += `
				UNION ALL
				SELECT COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) AS dep_id
				FROM wisp_dependencies WHERE type = 'blocks'
		`
	}

	labelsSelect := "l.labels_json AS labels_json"
	labelsJoin := fmt.Sprintf(`
		LEFT JOIN (
			SELECT issue_id, JSON_ARRAYAGG(label) AS labels_json
			FROM %s
			GROUP BY issue_id
		) l ON l.issue_id = i.id`, tables.Labels)
	if skipLabels {
		labelsSelect = "NULL AS labels_json"
		labelsJoin = ""
	}

	searchSQL := fmt.Sprintf(`
		SELECT %s,
			%s,
			COALESCE(dc.cnt, 0) AS dep_count,
			COALESCE(rc.cnt, 0) AS rdep_count,
			COALESCE(cc.cnt, 0) AS comment_count,
			pc.parent_id     AS parent_id,
			d.deps_json      AS deps_json
		FROM %s i
		%s
		LEFT JOIN (
			SELECT issue_id, COUNT(*) AS cnt
			FROM %s
			WHERE type = 'blocks'
			GROUP BY issue_id
		) dc ON dc.issue_id = i.id
		LEFT JOIN (
			SELECT dep_id, COUNT(*) AS cnt FROM (
				%s
			) all_blockers GROUP BY dep_id
		) rc ON rc.dep_id = i.id
		LEFT JOIN (
			SELECT issue_id, COUNT(*) AS cnt
			FROM %s
			GROUP BY issue_id
		) cc ON cc.issue_id = i.id
		LEFT JOIN (
			SELECT issue_id,
			       MIN(COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external)) AS parent_id
			FROM %s
			WHERE type = 'parent-child'
			GROUP BY issue_id
		) pc ON pc.issue_id = i.id
		LEFT JOIN (
			SELECT issue_id, JSON_ARRAYAGG(%s) AS deps_json
			FROM %s
			GROUP BY issue_id
		) d ON d.issue_id = i.id
		%s
		%s
		%s
	`,
		readyWorkIssueColumns,
		labelsSelect,
		tables.Main,
		labelsJoin,
		tables.Dependencies,
		reverseBlockerSelect,
		tables.Comments,
		tables.Dependencies,
		readyWorkDepJSONObject,
		tables.Dependencies,
		whereSQL,
		orderBySQL,
		limitSQL,
	)

	rows, err := tx.QueryContext(ctx, searchSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("search count %s: %w", tables.Main, err)
	}
	defer func() { _ = rows.Close() }()

	var out []*types.IssueWithCounts
	seen := make(map[string]bool)
	for rows.Next() {
		iwc, scanErr := scanReadyWorkRowWithCounts(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if iwc == nil || iwc.Issue == nil {
			continue
		}
		if seen[iwc.Issue.ID] {
			continue
		}
		seen[iwc.Issue.ID] = true
		out = append(out, iwc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search count %s: rows: %w", tables.Main, err)
	}
	return out, nil
}

func finishSearchIssuesWithCounts(items []*types.IssueWithCounts, filter types.IssueFilter) []*types.IssueWithCounts {
	sortSearchIssuesWithCounts(items, filter.SortBy, filter.SortDesc)
	if filter.Limit > 0 && len(items) > filter.Limit {
		return items[:filter.Limit]
	}
	return items
}

// sortSearchIssuesWithCounts must order the merged issues+wisps rows the same
// way issueOpsOrderBy orders each per-table query; otherwise the limit cut in
// finishSearchIssuesWithCounts keeps a different row set than SQL selected.
func sortSearchIssuesWithCounts(items []*types.IssueWithCounts, sortBy string, sortDesc bool) {
	if len(items) <= 1 {
		return
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a == nil || a.Issue == nil {
			return false
		}
		if b == nil || b.Issue == nil {
			return true
		}
		return issueOpsLess(a.Issue, b.Issue, sortBy, sortDesc)
	})
}

func issueOpsLess(a, b *types.Issue, sortBy string, sortDesc bool) bool {
	if sortBy == "id" {
		return a.ID < b.ID
	}
	def, ok := issueOpsSortDefs[sortBy]
	if !ok {
		def = issueOpsSortDefs[""]
		sortBy = ""
	}
	descending := def.defaultDir == "DESC"
	if sortDesc {
		descending = !descending
	}
	if c := issueOpsSortKeyCompare(a, b, sortBy); c != 0 {
		if descending {
			return c > 0
		}
		return c < 0
	}
	if (sortBy == "" || sortBy == "priority") && !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID < b.ID
}

// issueOpsSortKeyCompare three-way compares the primary sort column in
// ascending order, with MySQL NULL-first semantics for nullable columns.
func issueOpsSortKeyCompare(a, b *types.Issue, sortBy string) int {
	switch sortBy {
	case "created":
		return compareTimesAsc(a.CreatedAt, b.CreatedAt)
	case "updated":
		return compareTimesAsc(a.UpdatedAt, b.UpdatedAt)
	case "closed":
		switch {
		case a.ClosedAt == nil && b.ClosedAt == nil:
			return 0
		case a.ClosedAt == nil:
			return -1
		case b.ClosedAt == nil:
			return 1
		}
		return compareTimesAsc(*a.ClosedAt, *b.ClosedAt)
	case "status":
		return strings.Compare(string(a.Status), string(b.Status))
	case "type":
		return strings.Compare(string(a.IssueType), string(b.IssueType))
	case "assignee":
		return strings.Compare(a.Assignee, b.Assignee)
	case "title":
		return strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title))
	}
	return a.Priority - b.Priority
}

func compareTimesAsc(a, b time.Time) int {
	switch {
	case a.Before(b):
		return -1
	case a.After(b):
		return 1
	}
	return 0
}

func joinAnd(clauses []string) string {
	switch len(clauses) {
	case 0:
		return ""
	case 1:
		return clauses[0]
	}
	total := 0
	for _, c := range clauses {
		total += len(c)
	}
	total += 5 * (len(clauses) - 1)
	buf := make([]byte, 0, total)
	for i, c := range clauses {
		if i > 0 {
			buf = append(buf, " AND "...)
		}
		buf = append(buf, c...)
	}
	return string(buf)
}
