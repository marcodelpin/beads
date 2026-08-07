package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/steveyegge/beads/internal/httpapi/apigen"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// The write side of the dependency graph: the wire adapters over
// issueops.DependencyEditor.
//
// They share the claim's posture exactly. The actor is caller-ASSERTED
// provenance and not authenticated identity; hooks do not fire and the
// per-command auto-commit machinery never runs. The only durable effect is the
// single storage commit the role makes inside its own transaction.
//
// Everything above the role here is argument validation: the media type, the
// body shape, the actor rules and the id bounds. The graph itself — the cycle
// gate, the hierarchy rule, the type conflict, the endpoint existence checks,
// the plane routing and the event stream — belongs to the role.

// removeDependencyMembers is the document's member list for the removal. The
// schema is additionalProperties: false, so anything else is refused BY NAME,
// which is why the body is decoded as raw members first.
var removeDependencyMembers = []string{"actor", "depends_on_id", "issue_id"}

// handleRemoveDependency removes exactly the edge the body names.
//
// It is idempotent at the role: an edge that is not there is `removed: false`
// with a 200, not a refusal, so a replayed teardown does not have to classify
// an error to discover it already ran. Nothing on this path probes whether
// either endpoint exists, which is why the operation has no 404 to give.
func (s *Server) handleRemoveDependency(w http.ResponseWriter, r *http.Request) {
	if !s.requireNoQuery(w, r) {
		return
	}
	if !s.requireJSONContent(w, r) {
		return
	}
	request, ok := s.removeDependencyRequest(w, r)
	if !ok {
		return
	}

	editor, err := s.dependencyEditor(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	result, err := editor.RemoveDependency(r.Context(), request)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	writeJSON(w, apigen.RemoveDependencyResponse{Removed: result.Removed})
}

// removeDependencyRequest decodes and validates the body, and reports whether
// the request may proceed. Every refusal here happens before any database work.
func (s *Server) removeDependencyRequest(w http.ResponseWriter, r *http.Request) (issueops.RemoveDependencyRequest, bool) {
	members, res := decodeJSONObject(w, r, maxJSONBodyBytes)
	if res != nil {
		s.fail(w, r, *res)
		return issueops.RemoveDependencyRequest{}, false
	}
	if offender, unknown := unknownMember(members, removeDependencyMembers); unknown {
		s.failUnknownMember(w, r, offender, removeDependencyMembers)
		return issueops.RemoveDependencyRequest{}, false
	}
	actor, ok := s.bodyActor(w, r, members)
	if !ok {
		return issueops.RemoveDependencyRequest{}, false
	}
	issueID, res := requiredEndpointMember(members, "issue_id")
	if res != nil {
		s.fail(w, r, *res)
		return issueops.RemoveDependencyRequest{}, false
	}
	dependsOnID, res := requiredEndpointMember(members, "depends_on_id")
	if res != nil {
		s.fail(w, r, *res)
		return issueops.RemoveDependencyRequest{}, false
	}
	return issueops.RemoveDependencyRequest{
		Actor:       actor,
		IssueID:     issueID,
		DependsOnID: dependsOnID,
	}, true
}

// requiredEndpointMember reads one end of an edge out of a decoded body.
//
// The id is bounded HERE for the reason the claim's path id is: the ids are
// EXACT canonical ids, `issues.id` is VARCHAR(255), and a longer value — or one
// carrying a control character — names no row that can exist. Answering it from
// the edge costs the server nothing. Unlike the claim's, this refusal is a 400
// rather than a 404: the id is in the BODY, so there is no resource this
// request failed to address and nothing a caller could learn from the answer
// that its own request does not already say.
func requiredEndpointMember(members map[string]json.RawMessage, member string) (string, *Result) {
	refuse := func(detail string) *Result {
		res := InvalidArgument(member, ReasonInvalidValue, detail)
		return &res
	}
	raw, ok := members[member]
	if !ok {
		return "", refuse("`" + member + "` is required")
	}
	// Through a POINTER so that `null` reaches the type-mismatch branch, for
	// the reason bodyActor gives.
	var id *string
	if err := json.Unmarshal(raw, &id); err != nil || id == nil {
		return "", refuse("`" + member + "` must be a string")
	}
	if res := checkEndpointID(member, *id); res != nil {
		return "", res
	}
	return *id, nil
}

// checkEndpointID applies the id bounds an edge endpoint carries wherever it is
// spelled — a member of the removal's body, or a member of one of the add's
// edges — so the two operations refuse the same values.
//
// It does NOT trim. An id is an exact canonical id, and trimming one would
// silently accept a value the caller did not send; the actor is trimmed because
// the document says so for that member alone.
func checkEndpointID(param, id string) *Result {
	refuse := func(detail string) *Result {
		res := InvalidArgument(param, ReasonInvalidValue, detail)
		return &res
	}
	switch {
	case id == "":
		return refuse("`" + param + "` must not be empty")
	case types.CheckFieldLen(param, id) != nil:
		return refuse(fmt.Sprintf("`%s` is longer than the %d characters storage holds", param, types.MaxFieldLen))
	case strings.ContainsFunc(id, isControlChar):
		return refuse("`" + param + "` must not contain control characters")
	}
	return nil
}
