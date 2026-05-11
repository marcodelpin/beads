package uow

import "context"

type UnitOfWork interface {
	Close(ctx context.Context)
	Commit(ctx context.Context, message string) error
}

type UnitOfWorkProvider interface {
	NewUOW(ctx context.Context) (UnitOfWork, error)
}

func NewUOW(ctx context.Context, p TxProvider) (UnitOfWork, error) {
	tx, err := p.NewTx(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Begin(ctx); err != nil {
		return nil, err
	}
	return &baseUOW{tx: tx}, nil
}

type baseUOW struct {
	tx Tx
}

func (u *baseUOW) Commit(ctx context.Context, message string) error {
	return u.tx.Commit(ctx, message)
}

func (u *baseUOW) Close(ctx context.Context) {
	u.tx.RollbackUnlessCommitted(ctx)
}
