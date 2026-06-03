package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/storage/dberrors"
	"github.com/steveyegge/beads/internal/types"
)

func (r *issueSQLRepositoryImpl) GetDescendants(ctx context.Context, rootID string, filter types.IssueFilter) ([]*types.Issue, error) {
	levelFilter := filter
	levelFilter.ParentID = nil
	levelFilter.Limit = 0
	levelFilter.Offset = 0

	issueWhereClauses, issueArgs, err := buildIssueFilterClauses("", levelFilter, issuesFilterTables)
	if err != nil {
		return nil, fmt.Errorf("descendants: issues filter: %w", err)
	}

	wispDepsExist, err := r.optionalTableExists(ctx, "wisp_dependencies")
	if err != nil {
		return nil, fmt.Errorf("descendants: wisp_dependencies probe: %w", err)
	}
	walkWisps := wispDepsExist
	if walkWisps {
		empty, probeErr := r.wispsTableEmptyOrMissing(ctx)
		if probeErr != nil {
			return nil, fmt.Errorf("descendants: wisps table probe: %w", probeErr)
		}
		walkWisps = !empty
	}

	var wispWhereClauses []string
	var wispArgs []any
	if walkWisps {
		wispWhereClauses, wispArgs, err = buildIssueFilterClauses("", levelFilter, wispsFilterTables)
		if err != nil {
			return nil, fmt.Errorf("descendants: wisps filter: %w", err)
		}
	}

	issueAllowedID := ""
	if len(issueWhereClauses) > 0 {
		issueAllowedID = fmt.Sprintf(" AND i.id IN (SELECT id FROM issues WHERE %s)",
			strings.Join(issueWhereClauses, " AND "))
	}
	wispAllowedID := ""
	if walkWisps && len(wispWhereClauses) > 0 {
		wispAllowedID = fmt.Sprintf(" AND w.id IN (SELECT id FROM wisps WHERE %s)",
			strings.Join(wispWhereClauses, " AND "))
	}

	cte := buildDescendantsCTE(walkWisps, issueAllowedID, wispAllowedID)

	var allArgs []any
	allArgs = append(allArgs, rootID)
	allArgs = append(allArgs, issueArgs...)
	if walkWisps {
		allArgs = append(allArgs, rootID)
		allArgs = append(allArgs, wispArgs...)
	}
	allArgs = append(allArgs, issueArgs...)
	if walkWisps {
		allArgs = append(allArgs, wispArgs...)
	}

	rows, err := r.runner.QueryContext(ctx, cte, allArgs...)
	if err != nil {
		return nil, fmt.Errorf("descendants: query: %w", err)
	}
	page, err := scanIDSrcPage(rows, false)
	if err != nil {
		return nil, fmt.Errorf("descendants: %w", err)
	}

	issuesByID, err := r.fetchIssuesByIDs(ctx, page.issueIDs, issuesFilterTables, filter)
	if err != nil {
		return nil, fmt.Errorf("descendants: hydrate issues: %w", err)
	}

	var wispsByID map[string]*types.Issue
	if len(page.wispIDs) > 0 {
		wispsByID, err = r.fetchIssuesByIDs(ctx, page.wispIDs, wispsFilterTables, filter)
		if err != nil && !dberrors.IsTableNotExist(err) {
			return nil, fmt.Errorf("descendants: hydrate wisps: %w", err)
		}
	}

	return reassembleBySrc(page.ordered, issuesByID, wispsByID), nil
}

func buildDescendantsCTE(walkWisps bool, issueAllowedID, wispAllowedID string) string {
	var b strings.Builder
	b.WriteString("WITH RECURSIVE descendants AS (\n")

	fmt.Fprintf(&b, `    SELECT i.id, 'i' AS src
    FROM issues i
    JOIN dependencies d ON d.issue_id = i.id
    WHERE d.type = 'parent-child'
      AND COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id) = ?
      %s`, issueAllowedID)

	if walkWisps {
		b.WriteString("\n    UNION ALL\n")
		fmt.Fprintf(&b, `    SELECT w.id, 'w' AS src
    FROM wisps w
    JOIN wisp_dependencies wd ON wd.issue_id = w.id
    WHERE wd.type = 'parent-child'
      AND COALESCE(wd.depends_on_issue_id, wd.depends_on_wisp_id) = ?
      %s`, wispAllowedID)
	}

	b.WriteString("\n    UNION ALL\n")

	fmt.Fprintf(&b, `    SELECT i.id, 'i' AS src
    FROM issues i
    JOIN dependencies d ON d.issue_id = i.id
    JOIN descendants p ON COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id) = p.id
    WHERE d.type = 'parent-child'
      %s`, issueAllowedID)

	if walkWisps {
		b.WriteString("\n    UNION ALL\n")
		fmt.Fprintf(&b, `    SELECT w.id, 'w' AS src
    FROM wisps w
    JOIN wisp_dependencies wd ON wd.issue_id = w.id
    JOIN descendants p ON COALESCE(wd.depends_on_issue_id, wd.depends_on_wisp_id) = p.id
    WHERE wd.type = 'parent-child'
      %s`, wispAllowedID)
	}

	b.WriteString("\n)\nSELECT id, src FROM descendants\n")
	return b.String()
}
