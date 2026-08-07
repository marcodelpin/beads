package httpapi

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// The pins for the two include parameters on GET /v0/beads/issues/{id}.
//
// The handler is a decoder and nothing else — the row lists are the ROLE's to
// populate — so the property under test is that each parameter reaches
// issueops.GetRequest and that neither is set when the caller did not ask.
// Both halves are asserted: the request the role received, and the body the
// client got, because a flag forwarded to a role whose answer nobody reads
// would satisfy the first alone.

// includeAwareReader answers a detail view the way the role contract says a
// reader answers one: the two expensive row lists are present when the request
// asked for them and absent when it did not — the contract
// backend/conformance/reader_contract.go holds the real implementations to. It
// models that and nothing else, because that is exactly what these parameters
// select.
type includeAwareReader struct {
	roleReader
}

func (r *includeAwareReader) Get(ctx context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
	if _, err := r.roleReader.Get(ctx, req); err != nil {
		return nil, err
	}
	one := int64(1)
	omitted := true
	details := &issueops.IssueDetails{
		Issue:           *seededIssue(req.ID, "", types.StatusOpen),
		CommentCount:    &one,
		DependentCount:  &one,
		CommentsOmitted: &omitted,
	}
	if req.IncludeComments {
		details.Comments = []*types.Comment{{
			ID:        "c-1",
			IssueID:   req.ID,
			Author:    "alice",
			Text:      "the comment body",
			CreatedAt: time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
		}}
		// Never set alongside a populated list: the flag exists to tell "no
		// comments" from "not asked for", and a caller that asked has neither
		// question.
		details.CommentsOmitted = nil
	}
	if req.IncludeDependents {
		details.Dependents = []*types.IssueWithDependencyMetadata{{
			Issue:          *seededIssue("bd-2", "", types.StatusOpen),
			DependencyType: types.DepBlocks,
		}}
	}
	return details, nil
}

func newGetIssueServer(t *testing.T) (*testServer, *includeAwareReader) {
	t.Helper()
	rd := &includeAwareReader{}
	return newTestServer(t, rolesConfig(Config{Reader: rd})), rd
}

// TestGetIssueWithoutTheIncludeParametersIsUnchanged is the no-regression half
// of adding them: a caller that sends neither gets the request the role saw
// before this operation had parameters at all, so nothing about the response
// can have moved.
//
// The second half asserts the DEFAULT rather than the decode. Spelling both
// parameters `false` must answer the same body as omitting them; a default
// that drifted to true would still satisfy the explicit-spelling cases below.
func TestGetIssueWithoutTheIncludeParametersIsUnchanged(t *testing.T) {
	ts, rd := newGetIssueServer(t)

	resp := ts.get(t, "/v0/beads/issues/bd-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
	}
	omitted := decodeBody(t, resp)

	reqs := rd.getRequests()
	if len(reqs) != 1 {
		t.Fatalf("%d detail reads, want 1", len(reqs))
	}
	if want := (issueops.GetRequest{ID: "bd-1"}); reqs[0] != want {
		t.Errorf("GetRequest = %+v, want %+v — a caller that asks for neither row list must not pay for either", reqs[0], want)
	}
	if _, ok := omitted["comments"]; ok {
		t.Error("`comments` is populated without include_comments")
	}
	if _, ok := omitted["dependents"]; ok {
		t.Error("`dependents` is populated without include_dependents")
	}

	explicit := ts.get(t, "/v0/beads/issues/bd-1?include_comments=false&include_dependents=false")
	if explicit.StatusCode != http.StatusOK {
		t.Fatalf("explicit false: status = %d, want 200: %s", explicit.StatusCode, readAll(t, explicit))
	}
	if got := decodeBody(t, explicit); !reflect.DeepEqual(got, omitted) {
		t.Errorf("spelling both parameters false answered %v, want the body an omitted parameter answers: %v", got, omitted)
	}
}

// TestGetIssueIncludeParametersReachTheRole drives each parameter alone and
// both together. Alone matters: the two row lists are independent reads, and a
// handler that decoded one into the other's field would answer every
// single-parameter request with the wrong list and still look correct on the
// both-parameters case.
//
// The values exercise strconv.ParseBool's vocabulary rather than "true" three
// times, because that vocabulary is what the shared decoder publishes.
func TestGetIssueIncludeParametersReachTheRole(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  issueops.GetRequest
	}{
		{
			name:  "comments alone",
			query: "?include_comments=true",
			want:  issueops.GetRequest{ID: "bd-1", IncludeComments: true},
		},
		{
			name:  "dependents alone",
			query: "?include_dependents=1",
			want:  issueops.GetRequest{ID: "bd-1", IncludeDependents: true},
		},
		{
			name:  "both",
			query: "?include_comments=TRUE&include_dependents=t",
			want:  issueops.GetRequest{ID: "bd-1", IncludeComments: true, IncludeDependents: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, rd := newGetIssueServer(t)

			resp := ts.get(t, "/v0/beads/issues/bd-1"+tc.query)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
			}

			reqs := rd.getRequests()
			if len(reqs) != 1 {
				t.Fatalf("%d detail reads, want 1", len(reqs))
			}
			if reqs[0] != tc.want {
				t.Errorf("GetRequest = %+v, want %+v", reqs[0], tc.want)
			}

			body := decodeBody(t, resp)
			if _, ok := body["comments"]; ok != tc.want.IncludeComments {
				t.Errorf("`comments` present = %v, want %v", ok, tc.want.IncludeComments)
			}
			if _, ok := body["dependents"]; ok != tc.want.IncludeDependents {
				t.Errorf("`dependents` present = %v, want %v", ok, tc.want.IncludeDependents)
			}
			if tc.want.IncludeComments {
				if _, ok := body["comments_omitted"]; ok {
					t.Error("`comments_omitted` is set on a response that carries the comment bodies")
				}
			}
		})
	}
}

// TestGetIssueRefusesAMalformedIncludeParameter: a bad boolean is now this
// operation's OWN 400, not the document-level unknown-parameter rule, and the
// two carry opposite client recoveries — `invalid_value` says "send something
// else", `unknown_parameter` says "this server is older than you think".
func TestGetIssueRefusesAMalformedIncludeParameter(t *testing.T) {
	for _, param := range []string{"include_comments", "include_dependents"} {
		t.Run(param, func(t *testing.T) {
			ts, rd := newGetIssueServer(t)

			resp := ts.get(t, "/v0/beads/issues/bd-1?"+param+"=maybe")
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, readAll(t, resp))
			}
			body := decodeBody(t, resp)
			if body["code"] != string(CodeInvalidArgument) || body["param"] != param || body["reason"] != string(ReasonInvalidValue) {
				t.Errorf("body = %v, want invalid_argument on param %s with reason invalid_value", body, param)
			}
			if n := len(rd.getRequests()); n != 0 {
				t.Errorf("%d detail reads ran; a refused request must not reach the database", n)
			}
		})
	}
}

// TestGetIssueStaysStrictAboutUnknownParameters is the guard on what having a
// parameter table could have cost. This operation used to reject every query
// key outright; now the table is the whole allowlist, and a key outside it must
// still be refused by name — silently ignoring one would hand a client the
// count-only body it believed it had asked to have filled in.
//
// The last case is the interaction: a request carrying BOTH a malformed known
// parameter and an unknown one is reported as the malformed value, because
// reporting it as version skew would send the client to degrade a parameter
// this server does in fact have.
func TestGetIssueStaysStrictAboutUnknownParameters(t *testing.T) {
	for _, tc := range []struct {
		name   string
		query  string
		param  string
		reason Reason
	}{
		{"an unknown key alone", "?bogus=1", "bogus", ReasonUnknownParameter},
		{"an unknown key beside a known one", "?include_comments=true&bogus=1", "bogus", ReasonUnknownParameter},
		{"a near miss on a real parameter", "?include_comment=true", "include_comment", ReasonUnknownParameter},
		{"a malformed known key beside an unknown one", "?include_comments=maybe&bogus=1", "include_comments", ReasonInvalidValue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, rd := newGetIssueServer(t)

			resp := ts.get(t, "/v0/beads/issues/bd-1"+tc.query)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, readAll(t, resp))
			}
			body := decodeBody(t, resp)
			if body["code"] != string(CodeInvalidArgument) || body["param"] != tc.param || body["reason"] != string(tc.reason) {
				t.Errorf("body = %v, want invalid_argument on param %s with reason %s", body, tc.param, tc.reason)
			}
			if n := len(rd.getRequests()); n != 0 {
				t.Errorf("%d detail reads ran; a refused request must not reach the database", n)
			}
		})
	}
}

// TestGetIssueAnswersAQueryRefusalBeforeTheIDBound pins the order the operation
// already had. When it took no parameters, the query string was refused first,
// so an unknown key on an id no row could hold was a 400 — and a client that
// sends one gets the answer that tells it what to fix. Deciding the id first
// would turn that request into a 404 and lose the refusal.
func TestGetIssueAnswersAQueryRefusalBeforeTheIDBound(t *testing.T) {
	ts, rd := newGetIssueServer(t)

	resp := ts.get(t, "/v0/beads/issues/bd-%01?bogus=1")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, readAll(t, resp))
	}
	body := decodeBody(t, resp)
	if body["param"] != "bogus" || body["reason"] != string(ReasonUnknownParameter) {
		t.Errorf("body = %v, want param=bogus reason=unknown_parameter", body)
	}
	if n := len(rd.getRequests()); n != 0 {
		t.Errorf("%d detail reads ran for a refused request", n)
	}
}
