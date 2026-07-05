package main

import (
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// TestBuildCreateDepsEmptySideConvention verifies the empty-side convention:
// edges referencing the not-yet-created issue leave that side EMPTY so a
// spooled create can carry them in its payload and replay resolves them
// against the store-generated ID (GH#4378-review D1/D6).
func TestBuildCreateDepsEmptySideConvention(t *testing.T) {
	deps, err := buildCreateDeps("epic-1", []string{"dep-2", "blocks:dep-3", "discovered-from:dep-4"}, "gate-5", "")
	if err != nil {
		t.Fatalf("buildCreateDeps: %v", err)
	}
	if len(deps) != 5 {
		t.Fatalf("got %d deps, want 5", len(deps))
	}

	// --parent: new issue (empty) -> parent.
	if deps[0].IssueID != "" || deps[0].DependsOnID != "epic-1" || deps[0].Type != types.DepParentChild {
		t.Errorf("parent edge wrong: %+v", deps[0])
	}
	// plain dep: new issue (empty) -> dep-2, default blocks.
	if deps[1].IssueID != "" || deps[1].DependsOnID != "dep-2" || deps[1].Type != types.DepBlocks {
		t.Errorf("plain dep edge wrong: %+v", deps[1])
	}
	// blocks:X swaps direction: X -> new issue (empty).
	if deps[2].IssueID != "dep-3" || deps[2].DependsOnID != "" || deps[2].Type != types.DepBlocks {
		t.Errorf("swapped blocks edge wrong: %+v", deps[2])
	}
	// discovered-from keeps its type, new issue (empty) -> dep-4.
	if deps[3].IssueID != "" || deps[3].DependsOnID != "dep-4" || deps[3].Type != types.DepDiscoveredFrom {
		t.Errorf("discovered-from edge wrong: %+v", deps[3])
	}
	// --waits-for: new issue (empty) -> gate-5, default all-children gate.
	if deps[4].IssueID != "" || deps[4].DependsOnID != "gate-5" || deps[4].Type != types.DepWaitsFor {
		t.Errorf("waits-for edge wrong: %+v", deps[4])
	}
	if deps[4].Metadata == "" {
		t.Error("waits-for edge missing gate metadata")
	}
}

// TestResolveSpooledDepsSubstitutesNewID verifies replay-side resolution:
// every EMPTY side becomes the freshly generated issue ID, and the payload
// structs are not mutated (copies are returned).
func TestResolveSpooledDepsSubstitutesNewID(t *testing.T) {
	in := []*types.Dependency{
		{DependsOnID: "epic-1", Type: types.DepParentChild}, // new -> epic-1
		{IssueID: "dep-3", Type: types.DepBlocks},           // dep-3 -> new
		nil, // tolerated
	}
	out := resolveSpooledDeps(in, "new-9")
	if len(out) != 2 {
		t.Fatalf("got %d deps, want 2 (nil skipped)", len(out))
	}
	if out[0].IssueID != "new-9" || out[0].DependsOnID != "epic-1" {
		t.Errorf("edge 0 wrong: %+v", out[0])
	}
	if out[1].IssueID != "dep-3" || out[1].DependsOnID != "new-9" {
		t.Errorf("edge 1 wrong: %+v", out[1])
	}
	// Originals untouched.
	if in[0].IssueID != "" || in[1].DependsOnID != "" {
		t.Error("resolveSpooledDeps mutated its input")
	}
}
