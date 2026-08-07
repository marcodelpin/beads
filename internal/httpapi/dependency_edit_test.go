package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// The pins for the dependency-graph writes. What is asserted here is the WIRE
// EDGE — that the handlers decode the document's members into the role's
// request faithfully, refuse what the document refuses, map the role's TYPED
// refusals onto the frozen codes, and re-implement nothing the role owns.
//
// These are pure: the whole path runs over a real listener against a fake role,
// so it is covered on every pull request by the unconditional Go test job. What
// a fake structurally cannot prove is what the storage transaction did — that a
// removal really removed and that a refused batch left ZERO edges behind — and
// that lives in cmd/bd's proxied-server integration test against real Dolt.

const removeDependencyPath = "/v0/beads/dependencies:remove"

func newDependencyServer(t *testing.T, editor *roleDependencyEditor) *testServer {
	t.Helper()
	return newTestServer(t, rolesConfig(Config{DependencyEditor: editor}))
}

// TestRemoveDependencyPathReachesItsHandler: the path is a LITERAL
// collection-level custom method, registered beside three literal paths UNDER
// the same collection. ServeMux requires the separating slash, so a 404 here
// would mean the colon spelling is being routed as something else.
func TestRemoveDependencyPathReachesItsHandler(t *testing.T) {
	editor := &roleDependencyEditor{removed: true}
	ts := newDependencyServer(t, editor)

	resp := ts.claim(t, removeDependencyPath, `{"actor":"alice","issue_id":"bd-1","depends_on_id":"bd-2"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
	}
	if calls := editor.removeRequests(); len(calls) != 1 {
		t.Fatalf("the role was called %d times, want 1 — the path reached another handler", len(calls))
	}
}

// TestRemoveDependencyForwardsTheNamedEdgeToTheRole is the operation's central
// pin: both endpoints reach the role EXACTLY as sent, and the actor reaches it
// trimmed.
//
// The asymmetry is the point. `actor` is trimmed because the document says so
// for that member; an id is an EXACT canonical id, so trimming one would
// silently address a row the caller did not name.
func TestRemoveDependencyForwardsTheNamedEdgeToTheRole(t *testing.T) {
	editor := &roleDependencyEditor{removed: true}
	ts := newDependencyServer(t, editor)

	resp := ts.claim(t, removeDependencyPath, `{"actor":"  alice  ","issue_id":"bd-1","depends_on_id":"bd-2"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
	}
	calls := editor.removeRequests()
	if len(calls) != 1 {
		t.Fatalf("%d removals, want 1", len(calls))
	}
	want := issueops.RemoveDependencyRequest{Actor: "alice", IssueID: "bd-1", DependsOnID: "bd-2"}
	if calls[0] != want {
		t.Errorf("request = %+v, want %+v", calls[0], want)
	}
	if body := decodeBody(t, resp); body["removed"] != true {
		t.Errorf("removed = %v, want true", body["removed"])
	}
}

// TestRemoveDependencyAnswersAMissingEdgeWithSuccess is the operation's whole
// idempotence contract, and the reason its code set has no 404.
//
// A second teardown pass must not have to classify an error to discover it
// already ran, so an edge that was not there is `removed: false` inside a 200 —
// not `not_found`, which would make a replayed removal indistinguishable from a
// request that went to the wrong server.
func TestRemoveDependencyAnswersAMissingEdgeWithSuccess(t *testing.T) {
	editor := &roleDependencyEditor{removed: false}
	ts := newDependencyServer(t, editor)

	resp := ts.claim(t, removeDependencyPath, `{"actor":"alice","issue_id":"bd-1","depends_on_id":"bd-2"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
	}
	body := decodeBody(t, resp)
	if body["removed"] != false {
		t.Errorf("removed = %v, want false", body["removed"])
	}
	// The member is required by the document, so it must be on the wire even
	// when false: a client reading a missing member as "absent means unknown"
	// is exactly what a bare `omitempty` would produce.
	if _, ok := body["removed"]; !ok {
		t.Error("`removed` is absent from the body; the document requires it")
	}
}

// TestRemoveDependencyRefusesTheShapesTheDocumentRefuses walks the 400
// vocabulary. Every case also asserts the role was NOT called: each of these is
// decidable from the request alone, so none may buy a database transaction.
func TestRemoveDependencyRefusesTheShapesTheDocumentRefuses(t *testing.T) {
	for _, test := range []struct {
		name      string
		body      string
		wantParam string
	}{
		{"unknown member", `{"actor":"alice","issue_id":"bd-1","depends_on_id":"bd-2","force":true}`, "force"},
		{"no actor", `{"issue_id":"bd-1","depends_on_id":"bd-2"}`, "actor"},
		{"null actor", `{"actor":null,"issue_id":"bd-1","depends_on_id":"bd-2"}`, "actor"},
		{"blank actor", `{"actor":"   ","issue_id":"bd-1","depends_on_id":"bd-2"}`, "actor"},
		{"actor with a newline", `{"actor":"alice\nbd: forged","issue_id":"bd-1","depends_on_id":"bd-2"}`, "actor"},
		{"no issue_id", `{"actor":"alice","depends_on_id":"bd-2"}`, "issue_id"},
		{"null issue_id", `{"actor":"alice","issue_id":null,"depends_on_id":"bd-2"}`, "issue_id"},
		{"issue_id is not a string", `{"actor":"alice","issue_id":7,"depends_on_id":"bd-2"}`, "issue_id"},
		{"empty issue_id", `{"actor":"alice","issue_id":"","depends_on_id":"bd-2"}`, "issue_id"},
		{
			"over-long issue_id",
			`{"actor":"alice","issue_id":"` + strings.Repeat("x", types.MaxFieldLen+1) + `","depends_on_id":"bd-2"}`,
			"issue_id",
		},
		{"issue_id with a control character", `{"actor":"alice","issue_id":"bd-1\u0001x","depends_on_id":"bd-2"}`, "issue_id"},
		{"no depends_on_id", `{"actor":"alice","issue_id":"bd-1"}`, "depends_on_id"},
		{"empty depends_on_id", `{"actor":"alice","issue_id":"bd-1","depends_on_id":""}`, "depends_on_id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			editor := &roleDependencyEditor{}
			ts := newDependencyServer(t, editor)

			resp := ts.claim(t, removeDependencyPath, test.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, readAll(t, resp))
			}
			body := decodeBody(t, resp)
			if body["code"] != string(CodeInvalidArgument) {
				t.Errorf("code = %v, want %q", body["code"], CodeInvalidArgument)
			}
			if body["param"] != test.wantParam {
				t.Errorf("param = %v, want %q — a client dispatches on this rather than on the detail", body["param"], test.wantParam)
			}
			if calls := editor.removeRequests(); len(calls) != 0 {
				t.Errorf("the role was called %d times for a refused request; nothing may be removed", len(calls))
			}
		})
	}
}

// TestRemoveDependencyRequiresTheDocumentedMediaType: the media-type refusal is
// a CSRF control, so it holds on this write exactly as it does on the claim.
func TestRemoveDependencyRequiresTheDocumentedMediaType(t *testing.T) {
	editor := &roleDependencyEditor{}
	ts := newDependencyServer(t, editor)

	resp := ts.postBody(t, removeDependencyPath, "text/plain",
		`{"actor":"alice","issue_id":"bd-1","depends_on_id":"bd-2"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, readAll(t, resp))
	}
	if body := decodeBody(t, resp); body["param"] != "Content-Type" {
		t.Errorf("param = %v, want Content-Type", body["param"])
	}
	if calls := editor.removeRequests(); len(calls) != 0 {
		t.Error("the role was called for a request with the wrong media type")
	}
}

// TestRemoveDependencyTakesNoQueryParameters: the operation is in the
// document's no-parameter list, so any query key is the uniform 400.
func TestRemoveDependencyTakesNoQueryParameters(t *testing.T) {
	editor := &roleDependencyEditor{}
	ts := newDependencyServer(t, editor)

	resp := ts.claim(t, removeDependencyPath+"?force=1", `{"actor":"alice","issue_id":"bd-1","depends_on_id":"bd-2"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, readAll(t, resp))
	}
	body := decodeBody(t, resp)
	if body["param"] != "force" || body["reason"] != string(ReasonUnknownParameter) {
		t.Errorf("param/reason = %v/%v, want force/%s", body["param"], body["reason"], ReasonUnknownParameter)
	}
	if calls := editor.removeRequests(); len(calls) != 0 {
		t.Error("the role was called for a request carrying a query string")
	}
}

// TestRemoveDependencyMapsRoleFailuresThroughTheSharedClassifier: this
// operation has no failure path of its own — there is no refusal it can earn
// that the shared mapping does not already name — so a role error must land on
// the frozen code its sentinel implies rather than on a blanket 500.
func TestRemoveDependencyMapsRoleFailuresThroughTheSharedClassifier(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   Code
	}{
		{
			name:       "an exhausted retry budget is retryable",
			err:        &mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock"},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   CodeBusy,
		},
		{
			name:       "anything unrecognized is a 500",
			err:        errors.New("dependencies: unexpected"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   CodeInternal,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts := newDependencyServer(t, &roleDependencyEditor{removeErr: test.err})

			resp := ts.claim(t, removeDependencyPath, `{"actor":"alice","issue_id":"bd-1","depends_on_id":"bd-2"}`)
			if resp.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, test.wantStatus, readAll(t, resp))
			}
			if body := decodeBody(t, resp); body["code"] != string(test.wantCode) {
				t.Errorf("code = %v, want %q", body["code"], test.wantCode)
			}
		})
	}
}
