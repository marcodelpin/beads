package issueops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
)

// GetIssueInTx retrieves a single issue by ID within an existing transaction,
// including its labels. Automatically routes to the wisps/wisp_labels tables
// if the ID is an active wisp. Returns storage.ErrNotFound (wrapped) if the
// issue does not exist in either table.
func GetIssueInTx(ctx context.Context, tx DBTX, id string) (*types.Issue, error) {
	return getIssueInTx(ctx, tx, id, "")
}

// GetIssueForUpdateInTx is GetIssueInTx with a row-level write lock
// (SELECT … FOR UPDATE). Read-merge-write updates (metadata edits, note
// appends) MUST read through this so the read itself serializes against
// concurrent writers on every backend:
//
//   - Postgres (READ COMMITTED): FOR UPDATE blocks on a concurrent writer's
//     row lock and returns the NEW row version after it commits, so the merge
//     never runs against a stale snapshot.
//   - MySQL (REPEATABLE READ): FOR UPDATE is a locking "current read" that
//     returns the latest committed row regardless of the transaction's
//     consistent-read snapshot.
//   - SQLite: has no row locks; sqlitedialect strips the clause. Safe because
//     _txlock=immediate takes the database write lock at BEGIN, before this
//     read runs.
//   - Dolt: parses FOR UPDATE as a no-op (no real row locking); the dolt store
//     relies on its commit-time conflict detection + withRetryTx rerun instead.
//
// A plain GetIssueInTx read followed by an UPDATE is NOT safe on Postgres or
// MySQL: the read happens before the write blocks on the row lock, so the
// merge is computed from a stale row and silently erases the concurrent
// writer's committed keys.
func GetIssueForUpdateInTx(ctx context.Context, tx DBTX, id string) (*types.Issue, error) {
	return getIssueInTx(ctx, tx, id, " FOR UPDATE")
}

func getIssueInTx(ctx context.Context, tx DBTX, id, lockSuffix string) (*types.Issue, error) {
	issue, err := getIssueFromTableInTx(ctx, tx, "issues", "labels", id, lockSuffix)
	if err == nil {
		return issue, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}

	issue, err = getIssueFromTableInTx(ctx, tx, "wisps", "wisp_labels", id, lockSuffix)
	if err == nil {
		return issue, nil
	}
	if errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("%w: issue %s", storage.ErrNotFound, id)
	}
	return nil, err
}

func getIssueFromTableInTx(ctx context.Context, tx DBTX, issueTable, labelTable, id, lockSuffix string) (*types.Issue, error) {
	//nolint:gosec // G201: issueTable is a hardcoded literal supplied by getIssueInTx ("issues" or "wisps"); lockSuffix is "" or " FOR UPDATE". IssueSelectColumns needs LeaseJoin for the lease columns (sqlbuild contract).
	row := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %s %s WHERE id = ?%s`,
		IssueSelectColumns, issueTable, sqlbuild.LeaseJoin(issueTable), lockSuffix), id)
	issue, err := ScanIssueFrom(row)
	if err == sql.ErrNoRows || isTableNotExistError(err) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get issue: %w", err)
	}

	// Fetch labels in the same transaction to avoid MaxOpenConns=1 deadlock.
	labels, err := GetLabelsInTx(ctx, tx, labelTable, id)
	if err != nil {
		return nil, fmt.Errorf("get issue labels: %w", err)
	}
	issue.Labels = labels

	return issue, nil
}
