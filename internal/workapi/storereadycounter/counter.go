// Package storereadycounter holds the store-backed implementation of
// issueops.ReadyCounter: one shared body that every store-shaped backend's
// ReadyCounter accessor hands back.
//
// It is a package of its own for the reason internal/workapi/storereader and
// internal/workapi/storecounter are — see either package's doc. A constructor
// sitting in internal/workapi would be a one-line drop-in for
// store.ReadyCounter() from any front door, and one that silently skips the
// decorators, because a decorator adds its layer in its own accessor. Down
// here the only importers are the two Dolt store packages, and the
// cmd-bd-role-constructors depguard rule in .golangci.yml makes a front door
// importing it a lint failure rather than a review comment.
//
// The accessor is the door. This is the thing behind it.
package storereadycounter

import (
	"context"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/workapi"
	"github.com/steveyegge/beads/issueops"
)

// New returns the ready-count surface backed by a store handle. *DoltStore and
// *EmbeddedDoltStore answer identically because the difference between them is
// below storage.DoltStorage, not above it.
//
// THE PARAMETER IS THE CHEAP SEAM ITSELF, not storage.DoltStorage, and that is
// the one thing this body does differently from its siblings. `bd ready --json`
// used to ask for that seam at RUNTIME — unwrap the decorators, type-assert to
// storage.ReadyWorkCounter, and fall back to an unbounded ready query when the
// assertion missed — because the value it held was a decorated interface that
// could not answer for its own inner store. Reached through an accessor the
// question is settled at COMPILE time: a store that cannot size the ready set
// cannot construct this body, so there is no silent fallback to a mega-query
// and no branch nothing exercises.
func New(store storage.ReadyWorkCounter) (issueops.ReadyCounter, error) {
	if store == nil {
		return nil, &issueops.ErrUnsupported{Op: "storereadycounter.New", Backend: "nil"}
	}
	return &storeReadyCounter{store: store}, nil
}

type storeReadyCounter struct{ store storage.ReadyWorkCounter }

var _ issueops.ReadyCounter = (*storeReadyCounter)(nil)

// CountReady sizes the ready set with the store's indexed COUNT(*) path.
//
// The filter comes from the shared builder, so the predicate counted here and
// the predicate Reader.Ready lists are one predicate, and the identity
// CountReady promises is a property of the query rather than of this file:
// storage.ReadyWorkCounter is documented as identical to
// len(GetReadyWorkWithCounts(filter with Limit=0)), which is the same filter
// this builder produces for the same request.
func (c *storeReadyCounter) CountReady(ctx context.Context, req issueops.ReadyRequest) (issueops.ReadyCountResult, error) {
	filter, err := workapi.BuildReadyCountFilter(req)
	if err != nil {
		return issueops.ReadyCountResult{}, err
	}
	total, err := c.store.CountReadyWork(ctx, filter)
	if err != nil {
		return issueops.ReadyCountResult{}, err
	}
	return issueops.ReadyCountResult{Total: int64(total)}, nil
}
