package uow

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/workapi"
	publicops "github.com/steveyegge/beads/issueops"
)

// IssueReaderSource is the capability accessor a unit-of-work provider offers.
// It is named here so a consumer that holds a provider by interface can ask
// for the role the same way a consumer holding a store does, instead of
// reaching for a constructor: the accessor IS the API on both seams, and a
// provider that cannot answer says so with an error rather than being wired
// around.
type IssueReaderSource interface {
	IssueReader() (publicops.Reader, error)
}

// issueReader runs public issue queries through a unit of work.
type issueReader struct {
	provider UnitOfWorkProvider
}

// IssueReader returns the guarded issue-query surface for this provider. A
// unit of work is not a special case: callers reach the queries through the
// same accessor they use on a store.
func (p *doltSQLProvider) IssueReader() (publicops.Reader, error) {
	return NewIssueReader(p)
}

// NewIssueReader constructs public issue queries backed by provider.
func NewIssueReader(provider UnitOfWorkProvider) (publicops.Reader, error) {
	if isNilUnitOfWorkProvider(provider) {
		return nil, fmt.Errorf("new issue reader: unit-of-work provider must not be nil")
	}
	return &issueReader{provider: provider}, nil
}

var _ publicops.Reader = (*issueReader)(nil)

// Ready answers one ready-work query inside one unit of work.
//
// ONE UOW PER CALL is the whole transaction model here, and it is a
// consequence of the role's shape rather than a policy bolted onto it: the
// methods are request-granular, so a request and a transaction are the same
// span. There is no commit — reads never commit — and RunTxRead owns the
// detached rollback, so a caller that hangs up mid-response cannot burn a
// pinned session.
func (r *issueReader) Ready(ctx context.Context, req publicops.ReadyRequest) (publicops.IssuePage, error) {
	return RunTxRead(ctx, r.provider, func(ctx context.Context, uw UnitOfWork) (publicops.IssuePage, error) {
		filter, err := workapi.BuildReadyFilter(req)
		if err != nil {
			return publicops.IssuePage{}, err
		}
		page, err := uw.IssueUseCase().GetReadyWorkWithCounts(ctx, filter)
		if err != nil {
			return publicops.IssuePage{}, err
		}
		return readerPage(page.Items, page.HasMore), nil
	})
}

func (r *issueReader) List(ctx context.Context, req publicops.ListRequest) (publicops.IssuePage, error) {
	return RunTxRead(ctx, r.provider, func(ctx context.Context, uw UnitOfWork) (publicops.IssuePage, error) {
		// The config source comes from the unit of work this call already
		// holds, so a caller reaching this method through the role has nothing
		// to supply and no step to skip. (`bd list --proxied-server` opens its
		// own unit of work and loads the same config directly — see
		// issueops.Reader's doc comment for why those two paging commands are
		// still off the role.)
		cfg, err := workapi.LoadUOWListConfig(ctx, uw)
		if err != nil {
			return publicops.IssuePage{}, err
		}
		filter, err := workapi.BuildListFilter(req, cfg)
		if err != nil {
			return publicops.IssuePage{}, err
		}
		// WHICH QUERY is the only thing --ready changes. The epilogue below is
		// deliberately outside the branch: when it lived inside the non-ready
		// arm only, the two arms of one contract method answered in different
		// orders, and a --ready page under a sort the database cannot express
		// came back untrimmed. The sibling implementation
		// (workapi.storeReader.List) sorts and trims both arms, so the split
		// was drift between two implementations of one method — seeded inside
		// the seam built to eliminate drift.
		var page domain.SearchCountsPage
		if req.ReadyFlag {
			page, err = uw.IssueUseCase().GetReadyWorkWithCounts(ctx, workapi.ReadyFilterFromIssueFilter(filter))
		} else {
			page, err = uw.IssueUseCase().SearchIssuesWithCounts(ctx, "", filter)
		}
		if err != nil {
			return publicops.IssuePage{}, err
		}

		// This seam reports HasMore natively, so there is no over-fetch to
		// trim — but the display order still has to be applied here, because a
		// sort the database cannot express left the query unlimited and this is
		// where the requested order first exists. The trim then cuts on that
		// order.
		workapi.SortIssuesWithCounts(page.Items, req.SortBy, req.Reverse)
		items, hasMore := page.Items, page.HasMore
		if limit := workapi.PageLimit(req); limit > 0 && len(items) > limit {
			items, hasMore = items[:limit], true
		}
		return readerPage(items, hasMore), nil
	})
}

func (r *issueReader) Get(ctx context.Context, req publicops.GetRequest) (*publicops.IssueDetails, error) {
	return RunTxRead(ctx, r.provider, func(ctx context.Context, uw UnitOfWork) (*publicops.IssueDetails, error) {
		src := workapi.NewUOWDetailSource(uw)
		issue, isWisp, err := workapi.GetIssueOrWisp(ctx, src, req.ID)
		if err != nil {
			return nil, err
		}
		return workapi.BuildIssueDetails(ctx, src, issue, isWisp, workapi.DetailOptions{
			IncludeDependents: req.IncludeDependents,
			IncludeComments:   req.IncludeComments,
		})
	})
}

// readerPage normalizes a domain page onto the contract's page. Items is never
// nil on the way out, so a caller never has to tell null from empty to learn
// that nothing matched.
func readerPage(items []*publicops.IssueWithCounts, hasMore bool) publicops.IssuePage {
	if items == nil {
		items = []*publicops.IssueWithCounts{}
	}
	return publicops.IssuePage{Items: items, HasMore: hasMore}
}
