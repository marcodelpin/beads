package uow

import (
	"context"
	"time"

	"github.com/cenkalti/backoff/v4"

	"github.com/steveyegge/beads/internal/storage/domain/db"
	"github.com/steveyegge/beads/internal/storage/issueops"
)

type Tx interface {
	Runner() db.Runner
	Commit(ctx context.Context, message string) error
	Rollback(ctx context.Context) error
	RollbackUnlessCommitted(ctx context.Context)
}

type TxProvider interface {
	BeginTx(ctx context.Context) (Tx, error)
}

const (
	txRetryInitialInterval = 25 * time.Millisecond
	txRetryMaxElapsed      = 15 * time.Second
	// txCloseTimeout bounds the detached per-attempt close below.
	txCloseTimeout = 5 * time.Second
)

// closeAttempt rolls an attempt's unit of work back on a context that outlives
// the caller's.
//
// Close sends ROLLBACK on the pinned connection, and the transaction layer
// POISONS that connection when the send fails (doltserver_tx.go) — go-sql-driver's
// session reset does not clear an open transaction, so a session that may still
// be in one must never go back to the pool. Correctness is safe either way; what
// is not safe is closing with an ALREADY-CANCELED context, which fails the
// ROLLBACK immediately and burns one pinned session every time a caller's
// context is done — an HTTP client that hangs up mid-write, or an expired
// deadline. The timeout keeps a hung rollback from blocking the caller forever.
//
// All three entry points below close through here. The hazard is not specific
// to the HTTP claim: the ~nine proxied CLI commands that reach storage through
// RunTx and RunTxRead burn a session the same way when a user interrupts one
// mid-write.
func closeAttempt(ctx context.Context, uw UnitOfWork) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), txCloseTimeout)
	defer cancel()
	uw.Close(closeCtx)
}

type TxFunc func(ctx context.Context, uw UnitOfWork) (commitMsg string, err error)

type TxFuncResult[T any] func(ctx context.Context, uw UnitOfWork) (result T, commitMsg string, err error)

type TxReadFunc[T any] func(ctx context.Context, uw UnitOfWork) (T, error)

func RunTx(ctx context.Context, p UnitOfWorkProvider, work TxFunc) error {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = txRetryInitialInterval
	bo.MaxElapsedTime = txRetryMaxElapsed

	return backoff.Retry(func() error {
		uw, err := p.NewUOW(ctx)
		if err != nil {
			if isSerializationError(err) {
				return err
			}
			return backoff.Permanent(err)
		}
		defer closeAttempt(ctx, uw)

		commitMsg, err := work(ctx, uw)
		if err != nil {
			if isSerializationError(err) {
				return err
			}
			return backoff.Permanent(err)
		}

		if commitMsg == "" {
			return nil
		}

		if err := uw.Commit(ctx, commitMsg); err != nil {
			if issueops.IsNothingToCommitError(err) {
				return nil
			}
			if isSerializationError(err) {
				return err
			}
			return backoff.Permanent(err)
		}

		return nil
	}, backoff.WithContext(bo, ctx))
}

func RunTxResult[T any](ctx context.Context, p UnitOfWorkProvider, work TxFuncResult[T]) (T, error) {
	var result T
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = txRetryInitialInterval
	bo.MaxElapsedTime = txRetryMaxElapsed

	err := backoff.Retry(func() error {
		uw, err := p.NewUOW(ctx)
		if err != nil {
			if isSerializationError(err) {
				return err
			}
			return backoff.Permanent(err)
		}
		defer closeAttempt(ctx, uw)

		r, commitMsg, err := work(ctx, uw)
		if err != nil {
			if isSerializationError(err) {
				return err
			}
			return backoff.Permanent(err)
		}

		if commitMsg == "" {
			result = r
			return nil
		}

		if err := uw.Commit(ctx, commitMsg); err != nil {
			if issueops.IsNothingToCommitError(err) {
				result = r
				return nil
			}
			if isSerializationError(err) {
				return err
			}
			return backoff.Permanent(err)
		}

		result = r
		return nil
	}, backoff.WithContext(bo, ctx))

	return result, err
}

func RunTxRead[T any](ctx context.Context, p UnitOfWorkProvider, work TxReadFunc[T]) (T, error) {
	var zero T
	uw, err := p.NewUOW(ctx)
	if err != nil {
		return zero, err
	}
	defer closeAttempt(ctx, uw)

	return work(ctx, uw)
}
