//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

func (s *EmbeddedDoltStore) GetIssue(ctx context.Context, id string) (*types.Issue, error) {
	var issue *types.Issue
	err := s.withConn(ctx, false, func(regularTx, ignoredTx *sql.Tx) error {
		var err error
		issue, err = issueops.GetIssueInTx(ctx, regularTx, id)
		return err
	})
	return issue, err
}
