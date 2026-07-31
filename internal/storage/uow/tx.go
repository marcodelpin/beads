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

const txRetryInitialInterval = 25 * time.Millisecond

// DefaultTxRetryMaxElapsed is how long the retry loop keeps redoing an attempt
// that loses Dolt's commit-time merge before giving up. Exported so callers
// that pass their own budget to RunTxResultWithin can derive it from this one
// instead of restating the number and drifting from it.
const DefaultTxRetryMaxElapsed = 15 * time.Second

type TxFunc func(ctx context.Context, uw UnitOfWork) (commitMsg string, err error)

type TxFuncResult[T any] func(ctx context.Context, uw UnitOfWork) (result T, commitMsg string, err error)

type TxReadFunc[T any] func(ctx context.Context, uw UnitOfWork) (T, error)

// RunTx is RunTxResult for work that produces no result.
func RunTx(ctx context.Context, p UnitOfWorkProvider, work TxFunc) error {
	_, err := RunTxResult(ctx, p, func(ctx context.Context, uw UnitOfWork) (struct{}, string, error) {
		commitMsg, err := work(ctx, uw)
		return struct{}{}, commitMsg, err
	})
	return err
}

func RunTxResult[T any](ctx context.Context, p UnitOfWorkProvider, work TxFuncResult[T]) (T, error) {
	return RunTxResultWithin(ctx, p, DefaultTxRetryMaxElapsed, work)
}

// RunTxResultWithin is RunTxResult with an explicit retry budget, for callers
// whose deadline differs from the default and for tests that need the
// conflict-exhaustion path to arrive in milliseconds. When the budget runs out
// the last serialization failure is returned unwrapped, so callers can tell an
// exhausted write conflict from a permanent error.
func RunTxResultWithin[T any](ctx context.Context, p UnitOfWorkProvider, maxElapsed time.Duration, work TxFuncResult[T]) (T, error) {
	var result T
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = txRetryInitialInterval
	bo.MaxElapsedTime = maxElapsed

	err := backoff.Retry(func() error {
		uw, err := p.NewUOW(ctx)
		if err != nil {
			if isSerializationError(err) {
				return err
			}
			return backoff.Permanent(err)
		}
		defer uw.Close(ctx)

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
	defer uw.Close(ctx)

	return work(ctx, uw)
}
