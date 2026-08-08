//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
)

// EmbeddedDoltStore reads and prunes the durable events journal through its
// own per-operation connection (withConn), so the `bd events` CLI works in
// embedded mode — where there is no stable *sql.DB to reach via RawDBAccessor.
var _ storage.EventsJournalAccessor = (*EmbeddedDoltStore)(nil)

// ReadEventsJournal returns journal rows with seq greater than since. The
// read runs in a rolled-back transaction (no writes), matching every other
// read on this store.
func (s *EmbeddedDoltStore) ReadEventsJournal(ctx context.Context, since int64, limit int) ([]storage.EventsJournalRow, error) {
	var out []storage.EventsJournalRow
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var readErr error
		out, readErr = issueops.ReadEventsInTx(ctx, tx, since, limit)
		return readErr
	})
	return out, err
}

// PruneEventsJournal deletes journal rows below before, honoring the retain
// floors, and returns the number of rows deleted. The delete commits.
func (s *EmbeddedDoltStore) PruneEventsJournal(ctx context.Context, before int64, retainDays, retainRows int) (int64, error) {
	var n int64
	err := s.withConn(ctx, true, func(tx *sql.Tx) error {
		var pruneErr error
		n, pruneErr = issueops.PruneEventsInTx(ctx, tx, before, retainDays, retainRows, time.Now().UTC())
		return pruneErr
	})
	return n, err
}
