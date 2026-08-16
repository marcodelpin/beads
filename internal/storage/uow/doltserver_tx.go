package uow

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"log"

	"github.com/steveyegge/beads/internal/storage/domain/db"
	"github.com/steveyegge/beads/internal/storage/issueops"
)

type doltServerTx struct {
	conn *sql.Conn
	done bool
	// clearJournalScope releases the events-journal activation BeginTx bound to
	// conn. It is called from the two places the connection leaves this tx —
	// releaseConn and poisonConn — so the activation entry cannot outlive the
	// transaction it describes, whichever way the transaction ends.
	clearJournalScope func()
}

var _ Tx = (*doltServerTx)(nil)

func (t *doltServerTx) Runner() db.Runner {
	return t.conn
}

// Commit closes the unit of work in two phases (the uow sibling of upstream
// #5740, mirroring DoltStore.runIssueOperationTxWithMessage /
// doltAddAndCommitPostTx): a plain SQL COMMIT first, then - outside any open
// transaction - the Dolt commit against the session's post-merge root.
//
// Ordering contract (lost-update fix, LatentLabsSpace/NEXUS#92): the Dolt
// commit MUST run after the SQL transaction commits, never inside it.
// DOLT_COMMIT('-Am') stages the ENTIRE working set from the session's
// transaction root; when it ran inside the still-open transaction, the Dolt
// commit was built from the pre-merge BEGIN-time snapshot with the
// then-current branch HEAD as its parent - so every row a concurrent writer
// had committed inside the transaction's window (a drain worker mutating a
// DIFFERENT issue id, whose rows never conflict) was silently written back to
// its BEGIN-time value. Committing the SQL transaction first lets Dolt's
// commit-time three-way merge reconcile this session's changes with
// concurrent writers; the post-commit DOLT_COMMIT then stages the merged
// state, which cannot resurrect stale rows.
func (t *doltServerTx) Commit(ctx context.Context, message string) error {
	if t.done {
		return errors.New("uow: commit: already done")
	}
	// An empty message selects the EPHEMERAL commit form (bd-aq0ql): a plain
	// SQL COMMIT persists the transaction's writes into the working set
	// without minting a Dolt commit or history. This exists for work that
	// touches ONLY dolt_ignored state - today the leases table (bd-lrgn1),
	// whose heartbeats must never create commits - and is only reachable via
	// uow.RunTxEphemeral: RunTx/RunTxResult treat an empty commitMsg as
	// "nothing to commit" and never call Commit at all.
	mintDoltCommit := false
	if message != "" {
		// Skip the guaranteed-empty DOLT_COMMIT when an idempotent write staged
		// nothing - e.g. a same-value REPLACE INTO metadata, or a re-claim whose
		// CAS UPDATE matched 0 rows, re-applied per-tick by orchestrators
		// converging desired state. Dolt rejects such empty commits server-side
		// with "nothing to commit", flooding the server log (observed ~99% of
		// log lines on a busy coordination DB) and burning CPU evaluating them.
		//
		// The gate runs DELIBERATELY BEFORE the SQL COMMIT, inside the open
		// transaction: there dolt_status reflects only this UOW's changes
		// (BeginTx pins t.conn and runs START TRANSACTION). After the COMMIT
		// the same query would read the SHARED branch working set, where a
		// concurrent writer's not-yet-Dolt-committed rows read as pending - a
		// no-op write on a busy branch would then mint a Dolt commit of a
		// neighbor's rows under this operation's message (phantom audit
		// evidence). HasPendingChanges mirrors what '-Am' sweeps up and
		// excludes dolt_ignore'd tables.
		pending, perr := issueops.HasPendingChanges(ctx, t.conn)
		if perr != nil {
			if isSerializationError(perr) {
				// As below: the server already rolled the transaction back and
				// the caller retries the whole unit of work, so leave the
				// pinned session in place for the retry.
				return perr
			}
			// A failed status check leaves the transaction open on the pinned
			// session, exactly like a failed COMMIT below: roll back before
			// release, or poison the connection.
			return t.closeOpenTxAfterFailure(ctx, perr)
		}
		mintDoltCommit = pending
	}
	// Phase 1: plain SQL COMMIT. This is where Dolt's commit-time three-way
	// merge runs and where a write conflict surfaces as a serialization error
	// - one statement earlier than the old in-tx DOLT_COMMIT shape, so the
	// retry classification in tx.go is unchanged.
	if _, err := t.conn.ExecContext(ctx, "COMMIT;"); err != nil {
		if isSerializationError(err) {
			// Serialization failures guarantee the transaction was already
			// rolled back and the caller retries them, so leave the pinned
			// session in place for the retry rather than tearing it down here.
			return err
		}
		// A non-serialization COMMIT failure leaves the transaction open on
		// the pinned session. Roll it back before releasing the connection so
		// the next borrower cannot inherit and implicitly commit the orphaned
		// writes. If the rollback also fails the session state is unknown, so
		// poison the connection and let the pool discard it instead of handing
		// it out again.
		return t.closeOpenTxAfterFailure(ctx, err)
	}
	if mintDoltCommit {
		t.doltCommitPostTx(ctx, message)
	}
	t.done = true
	t.releaseConn()
	return nil
}

// doltCommitPostTx is phase 2: mint the Dolt commit from the post-merge
// working root. The data transaction has already committed, so a failure here
// is logged and NOT propagated (mirroring runIssueOperationTxWithMessage):
// the mutation is durable in the branch working set and rides the next Dolt
// commit on the branch; surfacing an error would make every caller treat an
// applied mutation as failed (automated retries double-apply). This includes
// a residual "nothing to commit" - post-merge it means a concurrent writer's
// DOLT_COMMIT absorbed this operation's rows under its own message (or the
// merge was a no-net-change), not a lost update: durability was already
// decided by the phase-1 COMMIT, which does propagate failures. The old
// in-tx shape let "nothing to commit" surface as a lost-update signal; that
// signal was an artifact of the single-statement form and is superseded by
// the ordering fix.
func (t *doltServerTx) doltCommitPostTx(ctx context.Context, message string) {
	if _, err := t.conn.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', ?);", message); err != nil {
		if issueops.IsNothingToCommitError(err) {
			log.Printf("uow: post-tx dolt commit for %q absorbed by a concurrent writer (data committed; audit line rides that commit)", message)
		} else {
			log.Printf("uow: post-tx dolt commit failed for %q (data already committed; change rides the next dolt commit): %v", message, err)
		}
	}
}

// closeOpenTxAfterFailure finishes a Commit attempt that failed with the
// transaction still open on the pinned session: roll it back before releasing
// the connection so the next borrower cannot inherit and implicitly commit the
// orphaned writes, and poison the connection (pool discard) when even the
// rollback fails and the session state is unknown. Serialization failures are
// the caller's to handle first — those guarantee a server-side rollback and
// must leave the pinned session in place for the retry.
func (t *doltServerTx) closeOpenTxAfterFailure(ctx context.Context, err error) error {
	t.done = true
	if rbErr := t.rollbackConn(ctx); rbErr != nil {
		t.poisonConn()
	} else {
		t.releaseConn()
	}
	return err
}

func (t *doltServerTx) Rollback(ctx context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	err := t.rollbackConn(ctx)
	if err != nil {
		t.poisonConn()
	} else {
		t.releaseConn()
	}
	return err
}

func (t *doltServerTx) RollbackUnlessCommitted(ctx context.Context) {
	if !t.done {
		_ = t.Rollback(ctx)
	}
}

func (t *doltServerTx) rollbackConn(ctx context.Context) error {
	if t.conn == nil {
		return nil
	}
	_, err := t.conn.ExecContext(ctx, "ROLLBACK;")
	return err
}

func (t *doltServerTx) releaseConn() {
	t.releaseJournalScope()
	if t.conn != nil {
		_ = t.conn.Close()
		t.conn = nil
	}
}

// releaseJournalScope drops this transaction's events-journal activation entry.
// Idempotent: both connection-release paths call it, and Rollback may follow a
// failed Commit that already released.
func (t *doltServerTx) releaseJournalScope() {
	if t.clearJournalScope != nil {
		t.clearJournalScope()
		t.clearJournalScope = nil
	}
}

// poisonConn discards the pinned session instead of returning it to the pool.
// A session whose transaction may still be open must never be reused: because
// go-sql-driver's ResetSession only performs a liveness check (no
// COM_RESET_CONNECTION), the next borrower's implicit START TRANSACTION would
// commit the orphaned writes. Returning driver.ErrBadConn from Raw makes
// database/sql close the connection and drop it from the pool.
func (t *doltServerTx) poisonConn() {
	t.releaseJournalScope()
	if t.conn == nil {
		return
	}
	_ = t.conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = t.conn.Close()
	t.conn = nil
}
