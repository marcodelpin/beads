package dolt

import (
	"context"
	"database/sql"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// DefineLabel adds a row to the opt-in label vocabulary registry (bd label
// define). See issueops.DefineLabelInTx for the case-collision discipline.
func (s *DoltStore) DefineLabel(ctx context.Context, label, description, actor string) error {
	return s.withRetryTx(ctx, func(tx *sql.Tx) error {
		return issueops.DefineLabelInTx(ctx, tx, label, description, actor)
	})
}

// UndefineLabel removes a row from the label vocabulary registry (bd label
// undefine).
func (s *DoltStore) UndefineLabel(ctx context.Context, label string) error {
	return s.withRetryTx(ctx, func(tx *sql.Tx) error {
		return issueops.UndefineLabelInTx(ctx, tx, label)
	})
}

// ListLabelDefinitions returns every row in the label vocabulary registry,
// sorted by label.
func (s *DoltStore) ListLabelDefinitions(ctx context.Context) ([]types.LabelDefinition, error) {
	var defs []types.LabelDefinition
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		defs, err = issueops.ListLabelDefinitionsInTx(ctx, tx)
		return err
	})
	return defs, err
}
