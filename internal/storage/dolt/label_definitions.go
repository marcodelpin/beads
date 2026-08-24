package dolt

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// DefineLabel adds a row to the opt-in label vocabulary registry (bd label
// define). See issueops.DefineLabelInTx for the case-collision discipline.
//
// The write DOLT-commits (bda-d1py): label_definitions is a main-plane table
// (no dolt_ignore, unlike label_namespace_locks) holding the workspace's
// shared curated vocabulary, so a row left in the working set never travels
// on push/clone - a peer under labels.vocabulary=enforce would run against an
// empty or stale registry. Same discipline as the sibling durable label
// mutations (AddLabel/RemoveLabel -> doltAddAndCommit).
func (s *DoltStore) DefineLabel(ctx context.Context, label, description, actor string) error {
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		if err := s.withRetryTx(ctx, func(tx *sql.Tx) error {
			return issueops.DefineLabelInTx(ctx, tx, label, description, actor)
		}); err != nil {
			return err
		}
		return s.doltAddAndCommit(ctx, []string{"label_definitions"}, fmt.Sprintf("bd: label define %s", label))
	})
}

// UndefineLabel removes a row from the label vocabulary registry (bd label
// undefine). Dolt-commits for the same reason DefineLabel does.
func (s *DoltStore) UndefineLabel(ctx context.Context, label string) error {
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		if err := s.withRetryTx(ctx, func(tx *sql.Tx) error {
			return issueops.UndefineLabelInTx(ctx, tx, label)
		}); err != nil {
			return err
		}
		return s.doltAddAndCommit(ctx, []string{"label_definitions"}, fmt.Sprintf("bd: label undefine %s", label))
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
