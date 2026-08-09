package domain

import (
	"context"

	"github.com/steveyegge/beads/internal/storage"
)

// EventsJournalSQLRepository is the transaction-bound persistence seam for
// reading and pruning the durable mutation journal.
type EventsJournalSQLRepository interface {
	Read(ctx context.Context, since int64, limit int) ([]storage.EventsJournalRow, error)
	Prune(ctx context.Context, before int64, retainDays, retainRows int) (int64, error)
}

// EventsJournalUseCase exposes the bounded cursor operations needed by
// proxied-server callers without leaking a raw SQL connection.
type EventsJournalUseCase interface {
	Read(ctx context.Context, since int64, limit int) ([]storage.EventsJournalRow, error)
	Prune(ctx context.Context, before int64, retainDays, retainRows int) (int64, error)
}

func NewEventsJournalUseCase(repo EventsJournalSQLRepository) EventsJournalUseCase {
	return &eventsJournalUseCase{repo: repo}
}

type eventsJournalUseCase struct {
	repo EventsJournalSQLRepository
}

var _ EventsJournalUseCase = (*eventsJournalUseCase)(nil)

func (u *eventsJournalUseCase) Read(ctx context.Context, since int64, limit int) ([]storage.EventsJournalRow, error) {
	return u.repo.Read(ctx, since, limit)
}

func (u *eventsJournalUseCase) Prune(ctx context.Context, before int64, retainDays, retainRows int) (int64, error) {
	return u.repo.Prune(ctx, before, retainDays, retainRows)
}
