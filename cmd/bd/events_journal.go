package main

import (
	"context"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/eventsjournal"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/uow"
)

// Events-journal activation, applied in ONE place: the factories that construct
// a store or a unit-of-work provider (store_factory.go, store_factory_nocgo.go,
// uow_factory.go, and bd doctor's repair handlers through the same package).
// Every plumbing bd opens goes through one of those, so a new command cannot
// acquire a store that silently records nothing.
//
// It did not start there, and the three ways it was wrong are the argument for
// the guard that now holds it. First a process-global switch. Then two root
// pre-run call sites, which covered the CLI's own store and its proxied
// provider and missed `bd serve` (which builds its own provider for a
// server-mode workspace), routed creates and remote-cache hydration (which open
// a SECOND store for another workspace), the pluggable backend registry arm,
// and the personal-migration planning store. Then a blanket exemption for bd
// doctor, whose stated reason — "workspace repairs, not bead mutations" — was
// false for three of its repair handlers. Each miss ran with the journal off
// while every command reported success, because an empty journal is
// indistinguishable from a quiet one.
// TestEveryStoreConstructionActivatesTheEventsJournal keeps it centralized.
//
// The policy itself lives in internal/eventsjournal so bd doctor's fix package,
// which cannot import package main, applies the identical rule.

// eventsJournalEnabled reports the activation the PROCESS resolved, for the
// paths that have no particular workspace in hand.
func eventsJournalEnabled() bool {
	return config.GetBool(eventsjournal.ConfigKey)
}

// activateEventsJournalStore is eventsjournal.ActivateStore under the name the
// construction guard matches. Kept as a wrapper rather than called directly so
// cmd/bd has one spelling of the idiom and the guard has one name to look for.
func activateEventsJournalStore(beadsDir string, s storage.DoltStorage, err error) (storage.DoltStorage, error) {
	return eventsjournal.ActivateStore(beadsDir, s, err)
}

// activateEventsJournalProvider is the same for a unit-of-work provider — bd's
// second write plumbing, with its own transactions and its own activation.
func activateEventsJournalProvider(ctx context.Context, beadsDir string, p uow.UnitOfWorkProvider, err error) (uow.UnitOfWorkProvider, error) {
	if err != nil || p == nil {
		return p, err
	}
	configurer, _ := p.(storage.EventsJournalConfigurer)
	if cfgErr := eventsjournal.Apply(configurer, eventsjournal.EnabledFor(beadsDir)); cfgErr != nil {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), providerCloseTimeout)
		defer cancel()
		_ = p.Close(closeCtx)
		return nil, cfgErr
	}
	return p, nil
}
