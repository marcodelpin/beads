package workapi

import (
	"context"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// NewStoreReader returns the issue-query surface backed by a store handle. It
// is what every store-shaped backend's IssueReader accessor hands back:
// *DoltStore and *EmbeddedDoltStore answer identically because the difference
// between them is below storage.DoltStorage, not above it.
//
// The reader supplies its own ConfigSource from the store it already holds, so
// a caller ON THE ROLE cannot half-perform the "load config, build filter,
// execute" ritual: it has no way to reach the pieces.
//
// THE BOUNDARY, stated once here because this is where the constructor lives:
// today the store-backed role answers `bd show --json`, and the HTTP surface
// reaches the uow-backed one. `bd ready` and `bd list` still call the builders
// in this package directly — they consume the FILTER for the max-rows cap,
// --claim, --watch, the hierarchical --parent tree and the text renderings —
// so Ready and List below are reached only by tests until a front door moves.
// They are covered by reader_store_test.go for exactly that reason. See
// issueops.Reader's doc comment for why routing only those commands' JSON
// paths through the role would be worse.
//
// This constructor is exported because both Dolt store packages need one shared
// body, and it therefore sits in the package all 16 cmd/bd front-door files
// already import. A command could mint a reader from it and skip the telemetry
// decorator's reader-level spans. Nothing does; the accessor is the door.
func NewStoreReader(store storage.DoltStorage) (issueops.Reader, error) {
	if store == nil {
		return nil, &storage.ErrUnsupported{Op: "NewStoreReader", Backend: "nil"}
	}
	return &storeReader{store: store}, nil
}

type storeReader struct{ store storage.DoltStorage }

var _ issueops.Reader = (*storeReader)(nil)

func (r *storeReader) Ready(ctx context.Context, req issueops.ReadyRequest) (issueops.IssuePage, error) {
	filter, err := BuildReadyFilter(req)
	if err != nil {
		return issueops.IssuePage{}, err
	}
	limit := filter.Limit
	if limit > 0 {
		// The store seam has no HasMore of its own, so ask for one row past
		// the page and let its presence be the answer.
		filter.Limit = limit + 1
	}
	items, err := r.store.GetReadyWorkWithCounts(ctx, filter)
	if err != nil {
		return issueops.IssuePage{}, err
	}
	return page(items, limit), nil
}

func (r *storeReader) List(ctx context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
	cfg, err := LoadStoreListConfig(ctx, r.store)
	if err != nil {
		return issueops.IssuePage{}, err
	}
	filter, err := BuildListFilter(req, cfg)
	if err != nil {
		return issueops.IssuePage{}, err
	}

	var items []*types.IssueWithCounts
	if req.ReadyFlag {
		items, err = r.store.GetReadyWorkWithCounts(ctx, ReadyFilterFromIssueFilter(WithFetchOneExtra(filter)))
	} else {
		items, err = r.store.SearchIssuesWithCounts(ctx, "", WithFetchOneExtra(filter))
	}
	if err != nil {
		return issueops.IssuePage{}, err
	}

	// The display order is applied here, after the fetch, because it is the
	// order the trim below has to cut on: filter.Limit is already 0 for a sort
	// the database cannot express (SQLLimit), so those queries return
	// everything and this is where the requested order first exists.
	SortIssuesWithCounts(items, req.SortBy, req.Reverse)
	return page(items, PageLimit(req)), nil
}

func (r *storeReader) Get(ctx context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
	src := NewStoreDetailSource(r.store)
	issue, isWisp, err := GetIssueOrWisp(ctx, src, req.ID)
	if err != nil {
		return nil, err
	}
	return BuildIssueDetails(ctx, src, issue, isWisp, DetailOptions{
		IncludeDependents: req.IncludeDependents,
		IncludeComments:   req.IncludeComments,
	})
}

// page trims an over-fetched result back to the requested limit and reports
// whether the trim removed anything. A limit of 0 is unlimited, so nothing is
// trimmed and there is by definition nothing more.
//
// Items is never nil on the way out: an empty page is an empty array on every
// surface that serializes one, and a caller must not have to tell null from
// empty to learn that nothing matched.
func page(items []*types.IssueWithCounts, limit int) issueops.IssuePage {
	hasMore := limit > 0 && len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	if items == nil {
		items = []*types.IssueWithCounts{}
	}
	return issueops.IssuePage{Items: items, HasMore: hasMore}
}
