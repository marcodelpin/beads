package uow

import (
	"context"
	"database/sql"
)

type Tx interface {
	Begin(ctx context.Context) (*sql.Tx, error)
	Commit(ctx context.Context, message string) error
	Rollback(ctx context.Context) error
	RollbackUnlessCommitted(ctx context.Context)
}

type TxProvider interface {
	NewTx(ctx context.Context) (Tx, error)
}
