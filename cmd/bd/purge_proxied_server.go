package main

import (
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/issueops"
)

// proxiedSweeper hands back the guarded bulk-clearance surface for the
// proxied-server provider, through the provider's OWN capability accessor —
// the same two-step proxiedCounter performs, and for the same reason: the
// accessor is where each layer is added.
//
// This file used to hold a second copy of `bd purge` and `bd prune`: it parsed
// the flags again, opened a unit of work, built the candidate filter, matched
// the glob, re-applied the pinned and closed_at rechecks, ran its own
// reference scan, and then printed 190 lines of output that had to be kept
// character-identical to the direct route's by hand. All of that is behind the
// role now, so the proxied route differs from the direct one by which accessor
// it asks and nothing else.
func proxiedSweeper() (issueops.Sweeper, error) {
	if uowProvider == nil {
		return nil, errors.New("proxied-server UOW provider not initialized")
	}
	src, ok := uowProvider.(uow.SweeperSource)
	if !ok {
		return nil, fmt.Errorf("proxied-server provider %T does not offer the bulk-clearance surface", uowProvider)
	}
	return src.Sweeper()
}
