package httpapi

import (
	"errors"
	"net/http"

	"github.com/steveyegge/beads/internal/httpapi/apigen"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// defaultTreeDepth is the document's `max_depth` default, and it is the CLI's
// too (`bd dep tree --max-depth`, 50).
//
// It is a FRONT-DOOR default rather than a role default on purpose: the role
// refuses a zero depth outright, because an uninitialized recursive walk must
// not mean "walk the whole graph". Each front door supplies its own visible
// default instead, and the two happen to agree because the number is the same
// safety limit.
const defaultTreeDepth = 50

// handleDependencyTree answers GET /v0/beads/dependencies/tree.
//
// WHAT IS NOT HERE IS THE POINT, as it is for the cycle report next door. No
// graph is walked, no depth is counted, no visited set is kept, no `both` merge
// is performed and no status prune is applied: every one of those is inside
// issueops.TreeWalker's implementation, which `bd dep tree` reaches through the
// same accessor. A handler that merged the two directions for itself would be a
// second definition of what `direction=both` means, and the two surfaces would
// drift the first time either was edited.
//
// The element type is an ALIAS of types.TreeNode — the same struct the CLI's
// --json marshals — so there is no second wire struct here and there must never
// be one.
func (s *Server) handleDependencyTree(w http.ResponseWriter, r *http.Request) {
	q := newQuery(r.URL.Query())

	req := issueops.WalkTreeRequest{
		RootID: q.str("root_id"),
		Direction: issueops.TreeDirection(q.oneOf("direction", string(issueops.TreeDown),
			string(issueops.TreeDown), string(issueops.TreeUp), string(issueops.TreeBoth))),
		MaxDepth: defaultTreeDepth,
		Status:   types.Status(q.str("status")),
	}
	if depth := q.integer("max_depth"); depth != nil {
		req.MaxDepth = *depth
	}

	if !s.acceptQuery(w, r, q) {
		return
	}
	// `root_id` is required by the document, and its absence is refused HERE
	// rather than left to the role. The role would refuse it too — an empty root
	// is ErrValidation — but a missing REQUIRED PARAMETER and a parameter sent
	// empty are one fact on the wire and the client's recovery is the same, so
	// naming the parameter is worth more than forwarding the role's prose.
	if req.RootID == "" {
		requestInfo(r.Context()).refuse("root_id")
		s.fail(w, r, InvalidArgument("root_id", ReasonInvalidValue, "root_id is required and must be an exact issue id"))
		return
	}

	walker, err := s.treeWalker(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	result, err := walker.WalkTree(r.Context(), req)
	if err != nil {
		s.failTreeWalkErr(w, r, err, req)
		return
	}

	writeJSON(w, apigen.DependencyTreePage{
		Items: wireTreeNodes(result.Nodes),
		// Always false: this operation takes no limit, so nothing truncates the
		// answer — `max_depth` bounds the DESCENT instead. It is emitted rather
		// than omitted because the document types it as required, and because a
		// client must not have to tell "not truncated" from "this server does not
		// say".
		HasMore: false,
	})
}

// failTreeWalkErr answers a failed walk.
//
// The role's request-validation refusals are the caller's own input and reach
// the client as the 400 they are; an absent root is the 404 it is; everything
// else keeps going through the one mapping in problem.go.
//
// The 400 names `direction` or `max_depth` rather than always naming the same
// parameter, because `param` is what a client dispatches on and the two have
// different recoveries. Which one comes from the REQUEST the handler still
// holds, the way failEdgeReadErr reads its own: only a non-positive depth can
// have produced a depth refusal, and only a value outside the closed set a
// direction one.
//
// Both of those are cases the handler could almost have caught itself — q.oneOf
// already refuses an unknown direction — and that is deliberate rather than
// redundant. The role is the definition of the vocabulary; this mapping exists
// so that a refusal reaching the wire from BELOW the handler still arrives
// attributed instead of as an unattributed 400.
func (s *Server) failTreeWalkErr(w http.ResponseWriter, r *http.Request, err error, req issueops.WalkTreeRequest) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		s.fail(w, r, NotFound())
	case errors.Is(err, issueops.ErrValidation):
		param := "root_id"
		switch {
		case req.MaxDepth < 1:
			param = "max_depth"
		case req.Direction != issueops.TreeDown && req.Direction != issueops.TreeUp &&
			req.Direction != issueops.TreeBoth:
			param = "direction"
		}
		requestInfo(r.Context()).refuse(param)
		s.fail(w, r, InvalidArgument(param, ReasonInvalidValue, err.Error()))
	default:
		s.failErr(w, r, err)
	}
}

// wireTreeNodes projects the role's flat node list onto the generated envelope's
// element type, which is an ALIAS of the canonical struct — so this dereferences
// pointers and does nothing else.
//
// It exists for the one thing that is not free: the document says `items` is an
// empty array and never null, and the role's slice is already empty rather than
// nil for a successful call. Making the guarantee here as well means the wire
// promise does not depend on the role keeping its own.
func wireTreeNodes(nodes []*issueops.TreeNode) []apigen.TreeNode {
	out := make([]apigen.TreeNode, 0, len(nodes))
	for _, node := range nodes {
		if node == nil {
			continue
		}
		out = append(out, *node)
	}
	return out
}
