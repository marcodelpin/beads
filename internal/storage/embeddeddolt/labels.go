//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"

	"github.com/steveyegge/beads/internal/storage/issueops"
)

func (s *EmbeddedDoltStore) GetLabels(ctx context.Context, issueID string) ([]string, error) {
	var labels []string
	err := s.withConn(ctx, false, func(regularTx, ignoredTx *sql.Tx) error {
		var err error
		labels, err = issueops.GetLabelsInTx(ctx, regularTx, "", issueID)
		return err
	})
	return labels, err
}

func (s *EmbeddedDoltStore) AddLabel(ctx context.Context, issueID, label, actor string) error {
	return s.withConn(ctx, true, func(regularTx, ignoredTx *sql.Tx) error {
		return issueops.AddLabelInTx(ctx, regularTx, "", "", issueID, label, actor)
	})
}

// RemoveLabel removes a label from an issue.
func (s *EmbeddedDoltStore) RemoveLabel(ctx context.Context, issueID, label, actor string) error {
	return s.withConn(ctx, true, func(regularTx, ignoredTx *sql.Tx) error {
		return issueops.RemoveLabelInTx(ctx, regularTx, "", "", issueID, label, actor)
	})
}
