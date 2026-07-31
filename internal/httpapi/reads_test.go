package httpapi

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
)

// recordingIssues captures the filter the reader built, which is the only way
// to see what a handler actually asked storage for. Everything else about a
// read is observable from the response; the FILTER is not, and the filter is
// where the drift this design exists to prevent would live.
type recordingIssues struct {
	domain.IssueUseCase

	mu    sync.Mutex
	ready []types.WorkFilter
	items []*types.IssueWithCounts
}

func (f *recordingIssues) GetReadyWorkWithCounts(_ context.Context, filter types.WorkFilter) (domain.SearchCountsPage, error) {
	f.mu.Lock()
	f.ready = append(f.ready, filter)
	f.mu.Unlock()
	return domain.SearchCountsPage{Items: f.items}, nil
}

func (f *recordingIssues) readyFilters() []types.WorkFilter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]types.WorkFilter(nil), f.ready...)
}

func newReadServer(t *testing.T, cfg Config) (*testServer, *recordingIssues) {
	t.Helper()
	rec := &recordingIssues{}
	cfg.Provider = &fakeProvider{issues: &fakeIssues{}, readIssues: rec}
	return newTestServer(t, cfg), rec
}

// TestReadyForwardsAnExplicitSortPolicy is the guard on the one default that
// changes the item SET rather than just its order.
//
// The storage layer maps an EMPTY sort policy to hybrid. A handler that
// forwarded an absent `sort` as "" would therefore serve hybrid while the
// frozen document still read `default: priority` — and hybrid demotes older
// high-priority work, so as soon as `limit` truncates, the page contains
// DIFFERENT ISSUES from the ones `bd ready` shows. The document tests pin the
// document; only this pins the handler.
//
// The wanted value is the LITERAL, not readySortDefault. Comparing the filter
// against the same constant the handler read would pass for every value that
// constant could take, including hybrid — the assertion would say only that
// the handler forwards its own default, which is not the property at risk.
// TestDefaultsMatchCLIFlags ties that literal to `bd ready --sort`'s
// registered default and to the frozen document, so all three move together or
// one of them fails.
func TestReadyForwardsAnExplicitSortPolicy(t *testing.T) {
	ts, rec := newReadServer(t, Config{})

	if resp := ts.get(t, "/v0/beads/ready"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	filters := rec.readyFilters()
	if len(filters) != 1 {
		t.Fatalf("%d ready queries, want 1", len(filters))
	}
	if got := filters[0].SortPolicy; got != types.SortPolicy("priority") {
		t.Errorf("SortPolicy = %q, want \"priority\" — an empty policy is the storage layer's hybrid fallback, and hybrid re-SELECTS the page once the limit truncates",
			got)
	}
	// The shared limit default reaches storage too: the document states the
	// number, the CLI flag registers the same constant, and this is where the
	// server proves it uses that constant rather than a literal of its own.
	if got, want := filters[0].Limit, workapi.DefaultReadyLimit; got != want {
		t.Errorf("Limit = %d, want the shared default %d", got, want)
	}
}

// TestReadySortIsValidatedAgainstTheDocumentedEnum: an unrecognized policy is a
// 400 rather than a silent fallback, because a silent fallback would answer a
// question the client did not ask.
func TestReadySortIsValidatedAgainstTheDocumentedEnum(t *testing.T) {
	ts, rec := newReadServer(t, Config{})

	resp := ts.get(t, "/v0/beads/ready?sort=newest")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body := decodeBody(t, resp)
	if body["code"] != string(CodeInvalidArgument) || body["param"] != "sort" || body["reason"] != string(ReasonInvalidValue) {
		t.Errorf("body = %v, want invalid_argument on param sort with reason invalid_value", body)
	}
	if n := len(rec.readyFilters()); n != 0 {
		t.Errorf("%d ready queries ran; a refused request must not reach storage", n)
	}
}

// TestUnknownReadParameterIsRefusedByName: silently ignoring an unrecognized
// FILTER parameter WIDENS the result set, so a client one version ahead
// receives rows it believes it filtered out.
func TestUnknownReadParameterIsRefusedByName(t *testing.T) {
	for _, path := range []string{"/v0/beads/ready?bogus=1", "/v0/beads/issues?bogus=1", "/v0/beads/issues/bd-1?bogus=1"} {
		ts, _ := newReadServer(t, Config{})
		resp := ts.get(t, path)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s: status = %d, want 400", path, resp.StatusCode)
			continue
		}
		body := decodeBody(t, resp)
		if body["param"] != "bogus" || body["reason"] != string(ReasonUnknownParameter) {
			t.Errorf("GET %s: body = %v, want param=bogus reason=unknown_parameter", path, body)
		}
	}
}

// TestAMalformedKnownParameterIsNotReportedAsVersionSkew: the two 400 reasons
// carry opposite client recoveries — unknown_parameter says "this server is
// older than you think, degrade", invalid_value says "send something else" —
// so a bad value on a parameter the server DOES know must not be reported as
// the former just because the request also has to be checked for the latter.
func TestAMalformedKnownParameterIsNotReportedAsVersionSkew(t *testing.T) {
	ts, _ := newReadServer(t, Config{})

	resp := ts.get(t, "/v0/beads/ready?limit=-1&bogus=1")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body := decodeBody(t, resp)
	if body["param"] != "limit" || body["reason"] != string(ReasonInvalidValue) {
		t.Errorf("body = %v, want the malformed known parameter reported first", body)
	}
}

// TestUnlimitedReadsAreLoopbackOnly pins the one mode-dependent refusal: an
// unlimited read buffers the whole active set and its JSON encoding inside one
// shared process, which must not be reachable by arbitrary network peers.
func TestUnlimitedReadsAreLoopbackOnly(t *testing.T) {
	t.Run("loopback allows it", func(t *testing.T) {
		ts, rec := newReadServer(t, Config{})
		if resp := ts.get(t, "/v0/beads/ready?limit=0"); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		filters := rec.readyFilters()
		if len(filters) != 1 || filters[0].Limit != 0 {
			t.Errorf("filters = %v, want one query with Limit 0 (unlimited passes through untouched)", filters)
		}
	})

	t.Run("a non-loopback bind refuses it", func(t *testing.T) {
		ts, rec := newReadServer(t, Config{Addr: "127.0.0.1:0", AllowNonLoopback: true})
		resp := ts.get(t, "/v0/beads/ready?limit=0")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		body := decodeBody(t, resp)
		if body["param"] != "limit" || body["reason"] != string(ReasonInvalidValue) {
			t.Errorf("body = %v, want invalid_argument on param limit", body)
		}
		if n := len(rec.readyFilters()); n != 0 {
			t.Errorf("%d queries ran; the refusal must happen before the database is touched", n)
		}
	})
}

// TestGetIssueRefusesAnImpossibleIDFromTheEdge: an id longer than the column,
// or one carrying a control character a percent-escape decoded to, names no row
// that can exist. Answering it from the edge costs nothing and tells the caller
// exactly what a read would have — and the SAME 404 a real miss gets, so a
// caller cannot map the server's notion of a well-formed id.
func TestGetIssueRefusesAnImpossibleIDFromTheEdge(t *testing.T) {
	long := ""
	for range types.MaxFieldLen + 1 {
		long += "x"
	}
	for _, id := range []string{long, "bd-%01"} {
		ts, rec := newReadServer(t, Config{})
		resp := ts.get(t, "/v0/beads/issues/"+id)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET issue %q: status = %d, want 404", id, resp.StatusCode)
			continue
		}
		if body := decodeBody(t, resp); body["code"] != string(CodeNotFound) {
			t.Errorf("GET issue %q: code = %v, want not_found", id, body["code"])
		}
		if n := len(rec.readyFilters()); n != 0 {
			t.Errorf("GET issue %q reached storage", id)
		}
	}
}

// TestCursorRoundTrips: the token is opaque and server-private, and the only
// thing that invalidates one is a change to the encoding.
func TestCursorRoundTrips(t *testing.T) {
	items := []*types.IssueWithCounts{{Issue: &types.Issue{ID: "bd-7"}}}
	items[0].CreatedAt = items[0].CreatedAt.UTC()

	token := cursorFor(items)
	if token == "" {
		t.Fatal("cursorFor returned no token for a nonempty page")
	}
	pos, ok := decodeCursor(token)
	if ok {
		if pos.ID != "bd-7" {
			t.Errorf("decoded id = %q, want bd-7", pos.ID)
		}
	} else if !items[0].CreatedAt.IsZero() {
		t.Error("a token this server minted did not decode")
	}

	for _, bad := range []string{"", "v0.abc", "v1.!!!", "v1.", "not-a-cursor"} {
		if _, ok := decodeCursor(bad); ok {
			t.Errorf("decodeCursor(%q) succeeded; every unreadable token is the same client situation", bad)
		}
	}
}
