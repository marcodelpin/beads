package main

import (
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/issueops"
)

// proxiedCounter hands back the guarded issue-count surface for the
// proxied-server provider, through the provider's OWN capability accessor —
// the same two-step proxiedCommenter performs, and for the same reason: the
// accessor is where each layer is added.
//
// This file used to hold a second copy of `bd count`: it parsed the flags
// again, opened a unit of work, loaded the workspace's list configuration and
// applied the infra policy, then executed against the use case. All of that is
// behind the role now, so the proxied route differs from the direct one by
// which accessor it asks and nothing else.
func proxiedCounter() (issueops.Counter, error) {
	if uowProvider == nil {
		return nil, errors.New("proxied-server UOW provider not initialized")
	}
	src, ok := uowProvider.(uow.CounterSource)
	if !ok {
		return nil, fmt.Errorf("proxied-server provider %T does not offer the issue-count surface", uowProvider)
	}
	return src.Counter()
}
