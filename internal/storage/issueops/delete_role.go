package issueops

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
	publicops "github.com/steveyegge/beads/issueops"
)

// DeleteInTx is the store-backed body behind issueops.Deleter: the whole of
// `bd delete` from the existence probe to the reference rewrite, inside ONE
// transaction.
//
// It lives here rather than in an importable internal/workapi/store<role>
// package for the reason SweepInTx does: the work is several reads and several
// writes that must see one snapshot, and storage.DoltStorage publishes
// methods, not transactions. The two Dolt-backed stores share this body and
// differ only in how they reach a transaction, so they are ONE vote and the
// unit-of-work provider is the second. There is nothing here a front door
// could construct — the function takes a transaction — so no depguard entry is
// needed to keep it out of cmd/bd.
//
// It assumes a request already refused by workapi.ValidateDeleteRequest and
// already normalized by workapi.NormalizeDeleteIDs. The accessors do both
// BEFORE opening a transaction, so a malformed request costs no database work.
//
// THE REWRITE IS INSIDE THE TRANSACTION, and that is what this body adds over
// the code it replaces. The direct CLI route's batch path deleted the rows in
// one transaction and then rewrote the neighbors' text through the store
// afterwards, discarding each update's error; a failure between the two left a
// workspace whose rows were gone and whose descriptions still cited them.
func DeleteInTx(ctx context.Context, tx *sql.Tx, req publicops.DeleteRequest) (publicops.DeleteResult, error) {
	ids := req.IDs
	result := publicops.DeleteResult{DryRun: req.DryRun}

	// The existence probe comes FIRST, so `bd delete typo real` reports the
	// typo rather than whatever the graph says about the id that resolved.
	// It doubles as the hydration the rewrite needs later: an id present here
	// is a row in one of the two planes.
	wispSet, err := WispIDSetInTx(ctx, tx, ids)
	if err != nil {
		return publicops.DeleteResult{}, fmt.Errorf("delete: classify planes: %w", err)
	}
	found, err := GetIssuesByIDsInTx(ctx, tx, ids, wispSet)
	if err != nil {
		return publicops.DeleteResult{}, fmt.Errorf("delete: resolve ids: %w", err)
	}
	present := make(map[string]bool, len(found))
	for _, issue := range found {
		if issue != nil {
			present[issue.ID] = true
		}
	}
	var missing []string
	for _, id := range ids {
		if !present[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return publicops.DeleteResult{}, &publicops.NotFoundError{IDs: missing}
	}

	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}

	// The guard runs only when the request did not already say what to do
	// about dependents. Under Cascade there is nothing outside the set by
	// construction, which is why the expansion below is not asked about it.
	if !req.Cascade {
		_, regularIDs := partitionByWispSet(ids, wispSet)
		external, err := ExternalDependentsBySourceInTx(ctx, tx, regularIDs, idSet)
		if err != nil {
			return publicops.DeleteResult{}, fmt.Errorf("delete: check dependents: %w", err)
		}
		if !req.Force {
			// Request order, so the id a caller is told about is stable
			// across runs and across backends.
			for _, id := range ids {
				if deps := external[id]; len(deps) > 0 {
					return publicops.DeleteResult{}, &publicops.DependentsOutsideRequestError{
						IssueID:    id,
						Dependents: deps,
					}
				}
			}
		} else {
			orphaned := make(map[string]bool)
			for _, deps := range external {
				for _, id := range deps {
					orphaned[id] = true
				}
			}
			result.Orphaned = workapi.SortedDeleteIDs(orphaned)
		}
	}

	// The neighborhood is read BEFORE the deletion, because after it the
	// edges that identify a neighbor are gone. It is read against the whole
	// deletion set — the cascade closure, not just the named ids — so a row
	// citing a cascade-deleted id is rewritten too.
	deletionSet := ids
	if req.Cascade {
		closure, err := FindAllDependentsInTx(ctx, tx, ids)
		if err != nil {
			return publicops.DeleteResult{}, fmt.Errorf("delete: expand cascade: %w", err)
		}
		deletionSet = make([]string, 0, len(closure))
		for id := range closure {
			deletionSet = append(deletionSet, id)
		}
	}
	neighbors, err := deleteNeighborsInTx(ctx, tx, deletionSet)
	if err != nil {
		return publicops.DeleteResult{}, err
	}

	// force=true unconditionally: this body has ALREADY answered the guard
	// question above and holds the orphan list it produced, so letting the
	// shared batch delete ask it a second time would be one more scan of the
	// dependency tables on the path that is about to write.
	deleted, err := DeleteIssuesInTx(ctx, tx, ids, req.Cascade, true, req.DryRun)
	if err != nil {
		return publicops.DeleteResult{}, err
	}
	result.Deleted = deleted.DeletedCount
	result.Dependencies = deleted.DependenciesCount
	result.Labels = deleted.LabelsCount
	result.Events = deleted.EventsCount

	if req.DryRun {
		return result, nil
	}

	rewritten, err := RewriteDeletedReferencesInTx(ctx, tx, deletionSet, neighbors, req.Actor)
	if err != nil {
		return publicops.DeleteResult{}, err
	}
	result.ReferencesUpdated = rewritten
	return result, nil
}

// ExternalDependentsBySourceInTx reports, for each of ids, the DIRECT
// dependents that idSet does not contain — the rows a forced delete orphans
// and an unforced one refuses over.
//
// It reads both dependency planes and skips a missing wisp plane, the way
// every other cross-plane read here does. The per-source shape is what lets
// the unforced refusal name ONE blocked id instead of a flat union that
// answers "something is blocked".
//
//nolint:gosec // G201: inClause contains only ? placeholders
func ExternalDependentsBySourceInTx(ctx context.Context, tx DBTX, ids []string, idSet map[string]bool) (map[string][]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	bySource := make(map[string]map[string]bool)
	for i := 0; i < len(ids); i += deleteBatchSize {
		end := i + deleteBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		inClause, args := buildSQLInClause(ids[i:end])

		for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
			rows, err := tx.QueryContext(ctx,
				fmt.Sprintf(`SELECT %s AS depends_on_id, issue_id FROM %s WHERE %s`,
					DepTargetExpr, depTable, depTargetIn("", inClause)),
				args...)
			if err != nil {
				if optionalBlockedTable(depTable) && isTableNotExistError(err) {
					continue
				}
				return nil, fmt.Errorf("query dependents from %s: %w", depTable, err)
			}
			for rows.Next() {
				var target, dependent string
				if err := rows.Scan(&target, &dependent); err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("scan dependent: %w", err)
				}
				if idSet[dependent] {
					continue
				}
				if bySource[target] == nil {
					bySource[target] = make(map[string]bool)
				}
				bySource[target][dependent] = true
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("iterate dependents from %s: %w", depTable, err)
			}
		}
	}

	out := make(map[string][]string, len(bySource))
	for target, dependents := range bySource {
		out[target] = workapi.SortedDeleteIDs(dependents)
	}
	return out, nil
}

// deleteNeighborsInTx hydrates the SURVIVING rows joined to the deletion set
// by a dependency edge in either direction — the rows whose text the deletion
// rewrites.
//
// One query per plane over the whole set rather than the two per deleted id
// the CLI route used, because the neighborhood of a `--from-file` batch is
// asked for once here and the route asked for it 2N times.
//
//nolint:gosec // G201: inClause contains only ? placeholders
func deleteNeighborsInTx(ctx context.Context, tx DBTX, ids []string) ([]*types.Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	deleting := make(map[string]bool, len(ids))
	for _, id := range ids {
		deleting[id] = true
	}

	neighborIDs := make(map[string]bool)
	for i := 0; i < len(ids); i += deleteBatchSize {
		end := i + deleteBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		inClause, args := buildSQLInClause(ids[i:end])
		doubled := append(append([]interface{}{}, args...), args...)

		for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
			rows, err := tx.QueryContext(ctx,
				fmt.Sprintf(`SELECT issue_id, %s AS depends_on_id FROM %s WHERE issue_id IN (%s) OR %s`,
					DepTargetExpr, depTable, inClause, depTargetIn("", inClause)),
				doubled...)
			if err != nil {
				if optionalBlockedTable(depTable) && isTableNotExistError(err) {
					continue
				}
				return nil, fmt.Errorf("query neighbors from %s: %w", depTable, err)
			}
			for rows.Next() {
				var source, target string
				if err := rows.Scan(&source, &target); err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("scan neighbor: %w", err)
				}
				for _, candidate := range [2]string{source, target} {
					if candidate == "" || deleting[candidate] {
						continue
					}
					neighborIDs[candidate] = true
				}
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("iterate neighbors from %s: %w", depTable, err)
			}
		}
	}
	if len(neighborIDs) == 0 {
		return nil, nil
	}

	// Sorted so the rewrite touches rows in a stable order, which is what
	// makes a partially-applied failure reproducible.
	hydrate := workapi.SortedDeleteIDs(neighborIDs)
	// An `external:` target and a target belonging to another repository name
	// no row here; GetIssuesByIDsInTx simply does not return them.
	issues, err := GetIssuesByIDsInTx(ctx, tx, hydrate, nil)
	if err != nil {
		return nil, fmt.Errorf("hydrate neighbors: %w", err)
	}
	return issues, nil
}

// RewriteDeletedReferencesInTx replaces every word-boundary occurrence of a
// deleted id with `[deleted:<id>]` in each neighbor's description, notes,
// design and acceptance criteria, and reports how many ROWS it changed.
//
// Exported because the unit-of-work body needs the same rule and neither
// implementation may own it: a route that spelled the pattern differently
// would rewrite a different set of citations for the same deletion.
func RewriteDeletedReferencesInTx(ctx context.Context, tx DBTX, deletedIDs []string, neighbors []*types.Issue, actor string) (int, error) {
	if len(neighbors) == 0 {
		return 0, nil
	}
	touched := make(map[string]bool)
	for _, id := range deletedIDs {
		re := DeletedReferencePattern(id)
		replacement := `$1[deleted:` + id + `]$3`
		for _, neighbor := range neighbors {
			if neighbor == nil {
				continue
			}
			updates := make(map[string]interface{})
			for _, field := range []struct {
				column string
				value  *string
			}{
				{"description", &neighbor.Description},
				{"notes", &neighbor.Notes},
				{"design", &neighbor.Design},
				{"acceptance_criteria", &neighbor.AcceptanceCriteria},
			} {
				if *field.value == "" || !re.MatchString(*field.value) {
					continue
				}
				rewritten := re.ReplaceAllString(*field.value, replacement)
				updates[field.column] = rewritten
				// Write the rewrite back onto the in-memory row so a second
				// deleted id in the same field sees the first one's result
				// rather than re-reading the original.
				*field.value = rewritten
			}
			if len(updates) == 0 {
				continue
			}
			if _, err := UpdateIssueInTx(ctx, tx, neighbor.ID, updates, actor); err != nil {
				return 0, fmt.Errorf("rewrite references in %s: %w", neighbor.ID, err)
			}
			touched[neighbor.ID] = true
		}
	}
	return len(touched), nil
}

// DeletedReferencePattern is the citation rule, in one place: a literal id at
// ASCII word boundaries, where a word character includes the hyphen an id is
// full of. It matches `be-1` in "see (be-1)." and not inside `xbe-1` or
// `be-12`.
func DeletedReferencePattern(id string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[^A-Za-z0-9_-])(` + regexp.QuoteMeta(id) + `)($|[^A-Za-z0-9_-])`)
}
