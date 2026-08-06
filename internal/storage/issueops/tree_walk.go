package issueops

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// The dependency-tree WALK: the shared body behind issueops.TreeWalker on all
// three backends, split into a pure half and a transactional half so the parts
// that decide what the answer MEANS are testable without a database.
//
// ValidateWalkTreeRequest holds the request vocabulary, PruneTreeByStatus the
// ancestor-keeping rule and MergeBidirectionalTree the two-walk concatenation;
// all three are pure and pinned in tree_walk_test.go. WalkDependencyTreeInTx is
// the only part that needs a transaction, and the reason it needs one is the
// whole reason this is not a store<role> package: the root probe, the recursion
// and the hydration must see one database state, and for a `both` walk that
// covers BOTH directions — which no arrangement of storage.DoltStorage calls can
// promise, because that interface publishes methods rather than transactions.

// ValidateWalkTreeRequest checks a walk request against the vocabulary
// issueops.WalkTreeRequest documents and returns the normalized direction.
//
// It is pure and shared so the refusal is one sentence rather than three: every
// implementation validates through here, and the conformance contract can cite
// the leaf doc knowing all three backends refuse in the same words.
func ValidateWalkTreeRequest(req publicops.WalkTreeRequest) (publicops.TreeDirection, error) {
	if req.RootID == "" {
		return "", fmt.Errorf("%w: root id must not be empty", publicops.ErrValidation)
	}
	direction := req.Direction
	if direction == "" {
		direction = publicops.TreeDown
	}
	switch direction {
	case publicops.TreeDown, publicops.TreeUp, publicops.TreeBoth:
	default:
		return "", fmt.Errorf("%w: direction %q must be one of %q, %q, %q",
			publicops.ErrValidation, req.Direction,
			publicops.TreeDown, publicops.TreeUp, publicops.TreeBoth)
	}
	if req.MaxDepth < 1 {
		return "", fmt.Errorf("%w: max depth must be at least 1, got %d", publicops.ErrValidation, req.MaxDepth)
	}
	if req.MaxRows < 0 {
		return "", fmt.Errorf("%w: max rows must not be negative, got %d", publicops.ErrValidation, req.MaxRows)
	}
	return direction, nil
}

// PruneTreeByStatus keeps every node carrying status, plus the ancestor chain of
// each survivor, in the walk order the nodes arrived in.
//
// KEEPING THE ANCESTORS IS WHAT MAKES THE ANSWER STILL A TREE. A bare filter
// would return nodes whose ParentID names something absent from the answer, and
// every renderer that rebuilds the shape from Depth and ParentID would then draw
// orphans at the wrong indentation.
//
// A PRUNE THAT MATCHES NOTHING RETURNS NOTHING, root included. The root survives
// only as somebody's ancestor, never for its own sake, so a tree with no
// matching member comes back empty rather than as a lone root — see
// issueops.WalkTreeRequest.Status, which states it as a promise.
func PruneTreeByStatus(nodes []*types.TreeNode, status types.Status) []*types.TreeNode {
	if len(nodes) == 0 {
		return nodes
	}
	keep := make(map[string]bool, len(nodes))
	parentOf := make(map[string]string, len(nodes))
	for _, node := range nodes {
		if node == nil {
			continue
		}
		if node.ParentID != "" && node.ParentID != node.ID {
			parentOf[node.ID] = node.ParentID
		}
	}
	for _, node := range nodes {
		if node == nil || node.Status != status || keep[node.ID] {
			continue
		}
		keep[node.ID] = true
		// Walk to the root, stopping on a repeat so a parent chain that loops
		// back on itself cannot spin here. The walk's own first-visit rule makes
		// that unreachable today; the guard is cheap and this function is also
		// called from tests with hand-built input.
		for current := node.ID; ; {
			parent, ok := parentOf[current]
			if !ok || parent == current || keep[parent] {
				break
			}
			keep[parent] = true
			current = parent
		}
	}
	filtered := make([]*types.TreeNode, 0, len(nodes))
	for _, node := range nodes {
		if node != nil && keep[node.ID] {
			filtered = append(filtered, node)
		}
	}
	return filtered
}

// MergeBidirectionalTree concatenates an up walk and a down walk into the one
// list a `both` request answers with: every up node except the root, then the
// whole down tree beginning with the root.
//
// THE ROOT IS TAKEN FROM THE DOWN WALK, once. Both walks start at it, so
// dropping it from the up half is what keeps it from appearing twice — and the
// down half is the one that keeps it because the down tree is the main tree
// every renderer draws, with the dependents shown above it.
//
// NOTHING ELSE IS DE-DUPLICATED. A node that both blocks the root and is blocked
// by it legitimately appears in both halves, with a different Depth and a
// different ParentID in each, because those are two different facts about it.
// issueops.TreeResult.Nodes says so, so a caller aggregating the list does not
// assume the ids are distinct.
//
// The up nodes are COPIED rather than shared, so a caller mutating one half
// cannot reach into the other walk's slice.
func MergeBidirectionalTree(downTree, upTree []*types.TreeNode, rootID string) []*types.TreeNode {
	merged := make([]*types.TreeNode, 0, len(downTree)+len(upTree))
	for _, node := range upTree {
		if node == nil || node.ID == rootID {
			continue
		}
		clone := *node
		merged = append(merged, &clone)
	}
	return append(merged, downTree...)
}

// WalkDependencyTreeInTx is the whole read: validate, probe the root, walk each
// requested direction, prune, and enforce the defensive cap.
//
// It is the single body behind all three implementations of
// issueops.TreeWalker. Each store package wraps it in five lines around its own
// transaction and the unit of work reaches it through the domain repository, so
// "three backends agree" here is ONE VOTE plus an engine check — the wrappers
// are the only part that differs.
func WalkDependencyTreeInTx(ctx context.Context, tx DBTX, req publicops.WalkTreeRequest) (publicops.TreeResult, error) {
	direction, err := ValidateWalkTreeRequest(req)
	if err != nil {
		return publicops.TreeResult{}, err
	}

	// The root's existence is probed BEFORE the walk and inside the same
	// transaction, so an absent root is ErrNotFound rather than an empty tree.
	// The walk itself would surface the same miss for a `down` or `up` request,
	// but not for `both`: two walks would each fail and the caller would see
	// whichever error came first, wrapped differently. One probe is one answer.
	if _, err := GetIssueInTx(ctx, tx, req.RootID); err != nil {
		return publicops.TreeResult{}, err
	}

	nodes, err := walkTreeDirectionsInTx(ctx, tx, req.RootID, req.MaxDepth, direction)
	if err != nil {
		return publicops.TreeResult{}, err
	}

	if req.Status != "" {
		nodes = PruneTreeByStatus(nodes, req.Status)
	}
	if nodes == nil {
		nodes = []*types.TreeNode{}
	}

	// The cap is checked LAST, on the nodes the caller would have been handed.
	// A tree walk threads no query filter, so this is post-hoc by construction
	// and issueops.WalkTreeRequest.MaxRows says so rather than implying it.
	if err := EnforceMaxRowsCap(len(nodes), req.MaxRows, req.MaxRowsSource); err != nil {
		return publicops.TreeResult{}, err
	}

	return publicops.TreeResult{Nodes: nodes}, nil
}

// walkTreeDirectionsInTx runs the one or two walks a direction asks for.
func walkTreeDirectionsInTx(ctx context.Context, tx DBTX, rootID string, maxDepth int, direction publicops.TreeDirection) ([]*types.TreeNode, error) {
	if direction != publicops.TreeBoth {
		return GetDependencyTreeInTx(ctx, tx, rootID, maxDepth, false, direction == publicops.TreeUp)
	}
	downTree, err := GetDependencyTreeInTx(ctx, tx, rootID, maxDepth, false, false)
	if err != nil {
		return nil, err
	}
	upTree, err := GetDependencyTreeInTx(ctx, tx, rootID, maxDepth, false, true)
	if err != nil {
		return nil, err
	}
	return MergeBidirectionalTree(downTree, upTree, rootID), nil
}
