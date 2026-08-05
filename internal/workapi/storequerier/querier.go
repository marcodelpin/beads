// Package storequerier holds the store-backed implementation of
// issueops.Querier: one shared body that every store-shaped backend's Querier
// accessor hands back.
//
// It is a package of its own for the reason internal/workapi/storereader is —
// see that package's doc. A constructor sitting in internal/workapi would be a
// one-line drop-in for store.Querier() from any front door, and one that
// silently skips the decorators, because a decorator adds its layer in its own
// accessor. Down here the only importers are the two Dolt store packages and
// the cmd-bd-role-constructors depguard rule in .golangci.yml makes a front
// door importing it a lint failure rather than a review comment.
//
// The accessor is the door. This is the thing behind it.
package storequerier

import (
	"context"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/workapi"
	"github.com/steveyegge/beads/issueops"
)

// New returns the boolean-query surface backed by a store handle. *DoltStore
// and *EmbeddedDoltStore answer identically because the difference between
// them is below storage.DoltStorage, not above it.
func New(store storage.DoltStorage) (issueops.Querier, error) {
	if store == nil {
		return nil, &issueops.ErrUnsupported{Op: "storequerier.New", Backend: "nil"}
	}
	return &storeQuerier{store: store}, nil
}

type storeQuerier struct{ store storage.DoltStorage }

var _ issueops.Querier = (*storeQuerier)(nil)

// offsetBackend names the backend an Offset refusal comes from, spelled as
// storereader spells it: one name for both engines, because it is the BODY —
// not the engine underneath it — that cannot page by offset.
const offsetBackend = "dolt-store"

func (q *storeQuerier) Query(ctx context.Context, req issueops.QueryRequest) (issueops.IssuePage, error) {
	plan, err := workapi.BuildQueryPlan(req)
	if err != nil {
		return issueops.IssuePage{}, err
	}
	// UNIFORM, not per-expression. This body could skip rows for a predicate
	// query, where the skip happens in Go — but which shape an expression
	// takes is the evaluator's decision, not the caller's, so an Offset that
	// worked for one expression and refused another would be unpredictable
	// from the outside (issueops/querier.go:70-86).
	if plan.Offset != 0 {
		return issueops.IssuePage{}, &issueops.ErrUnsupported{Op: "Querier.Query(Offset)", Backend: offsetBackend}
	}

	filter := plan.Filter
	if !plan.RequiresPredicate() {
		// The store seam has no HasMore of its own, so ask for one row past
		// the page and let its presence be the answer. A predicate query
		// carries no row limit at all, so there is nothing here to extend:
		// its verdict comes from the count of MATCHES below.
		filter = workapi.WithFetchOneExtra(filter)
	}

	rows, err := q.store.SearchIssuesWithCounts(ctx, "", filter)
	if err != nil {
		return issueops.IssuePage{}, err
	}
	rows = workapi.ApplyQueryPredicate(rows, plan.Predicate)

	// The shared epilogue, for the reason every other page read runs it: the
	// sort, the cut and the verdict written out longhand is how two routes of
	// one command came to disagree about `--sort id --limit 5`.
	items, hasMore := workapi.FinishPage(rows, plan.SortBy, plan.Reverse, plan.Limit, false)
	return issueops.IssuePage{Items: items, HasMore: hasMore}, nil
}
