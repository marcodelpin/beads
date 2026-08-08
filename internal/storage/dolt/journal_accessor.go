package dolt

import (
	"context"
	"database/sql"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
)

// DoltStore reads and prunes the durable events journal through its own
// transaction helpers so the `bd events` CLI works against a server-mode
// store the same way it does against the embedded store.
var _ storage.EventsJournalAccessor = (*DoltStore)(nil)

// ReadEventsJournal returns journal rows with seq greater than since.
func (s *DoltStore) ReadEventsJournal(ctx context.Context, since int64, limit int) ([]storage.EventsJournalRow, error) {
	var out []storage.EventsJournalRow
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var readErr error
		out, readErr = issueops.ReadEventsInTx(ctx, tx, since, limit)
		return readErr
	})
	return out, err
}

// PruneEventsJournal deletes journal rows below before, honoring the retain
// floors, and returns the number of rows deleted.
func (s *DoltStore) PruneEventsJournal(ctx context.Context, before int64, retainDays, retainRows int) (int64, error) {
	var n int64
	err := s.withRetryTx(ctx, func(tx *sql.Tx) error {
		var pruneErr error
		n, pruneErr = issueops.PruneEventsInTx(ctx, tx, before, retainDays, retainRows, time.Now().UTC())
		return pruneErr
	})
	return n, err
}
