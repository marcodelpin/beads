package uow

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	db "github.com/steveyegge/beads/internal/storage/domain/db"
)

type doltServerTx struct {
	conn         *sql.Conn
	vc           db.DoltVersionControlSQLRepository
	tx           *sql.Tx
	branch       string
	targetBranch string
	done         bool
}

var _ Tx = (*doltServerTx)(nil)

func (t *doltServerTx) Begin(ctx context.Context) (*sql.Tx, error) {
	if t.tx != nil {
		return nil, errors.New("uow: Begin: already begun")
	}

	success := false
	defer func() {
		if !success {
			t.releaseConn()
		}
	}()

	branch, err := genRandomBranchName()
	if err != nil {
		return nil, err
	}

	if err := t.vc.Branch(ctx, branch, t.targetBranch); err != nil {
		return nil, fmt.Errorf("uow: create work branch %q from %q: %w", branch, t.targetBranch, err)
	}

	if err := t.vc.Checkout(ctx, branch); err != nil {
		_ = t.vc.Branch(ctx, "-d", branch)
		return nil, fmt.Errorf("uow: checkout work branch %q: %w", branch, err)
	}

	sqlTx, err := t.conn.BeginTx(ctx, nil)
	if err != nil {
		t.cleanupBranchBestEffort(ctx)
		return nil, fmt.Errorf("uow: begin sql tx: %w", err)
	}

	t.branch = branch
	t.tx = sqlTx
	success = true

	return sqlTx, nil
}

func (t *doltServerTx) Commit(ctx context.Context, message string) error {
	if t.done {
		return errors.New("uow: commit: already done")
	}

	t.done = true
	defer func() {
		t.cleanupBranchBestEffort(ctx)
		t.releaseConn()
	}()

	if err := t.tx.Commit(); err != nil {
		return fmt.Errorf("uow: commit sql tx: %w", err)
	}

	if err := t.vc.Add(ctx, "-A"); err != nil {
		return fmt.Errorf("uow: dolt add: %w", err)
	}

	if err := t.vc.Commit(ctx, "-m", message); err != nil && !isNothingToCommit(err) {
		return fmt.Errorf("uow: dolt commit: %w", err)
	}

	if err := t.vc.Checkout(ctx, t.targetBranch); err != nil {
		return fmt.Errorf("uow: checkout %s: %w", t.targetBranch, err)
	}

	if err := t.vc.Merge(ctx, t.branch); err != nil {
		return fmt.Errorf("uow: merge %s into %s: %w", t.branch, t.targetBranch, err)
	}

	if err := t.vc.Branch(ctx, "-d", t.branch); err != nil {
		return fmt.Errorf("uow: delete work branch %s: %w", t.branch, err)
	}

	return nil
}

func (t *doltServerTx) Rollback(ctx context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	defer t.releaseConn()

	var errs []error
	if err := t.tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		errs = append(errs, fmt.Errorf("uow: rollback sql tx: %w", err))
	}
	if err := t.vc.Checkout(ctx, t.targetBranch); err != nil {
		errs = append(errs, fmt.Errorf("uow: checkout %s on rollback: %w", t.targetBranch, err))
	}
	if err := t.vc.Branch(ctx, "-d", t.branch); err != nil {
		errs = append(errs, fmt.Errorf("uow: delete work branch %s on rollback: %w", t.branch, err))
	}
	return errors.Join(errs...)
}

func (t *doltServerTx) RollbackUnlessCommitted(ctx context.Context) {
	if !t.done {
		_ = t.Rollback(ctx)
	}
}

func (t *doltServerTx) cleanupBranchBestEffort(ctx context.Context) {
	_ = t.vc.Checkout(ctx, t.targetBranch)
	_ = t.vc.Branch(ctx, "-d", t.branch)
}

func (t *doltServerTx) releaseConn() {
	if t.conn != nil {
		_ = t.conn.Close()
		t.conn = nil
	}
}

func isNothingToCommit(err error) bool {
	return err != nil && strings.Contains(err.Error(), "nothing to commit")
}

func genRandomBranchName() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("uow: gen random branch name: %w", err)
	}
	return "bd-" + hex.EncodeToString(buf[:]), nil
}
