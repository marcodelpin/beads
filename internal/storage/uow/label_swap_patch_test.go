package uow

import (
	"context"
	"sort"
	"testing"

	"github.com/steveyegge/beads/internal/labelns"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// bda-fjjb: a single update patch carrying BOTH the removal of the old
// exclusive-namespace label and the addition of its replacement must swap
// atomically on the unit-of-work backend. Before the fix the domain's
// ApplyUpdate applied AddLabels before RemoveLabels, so the add's exclusivity
// guard saw the old label still in place and refused the patch - which is what
// forced `bd label add --replace` into a non-atomic two-transaction swap whose
// failure window strips the old label and applies nothing.
func TestUpdateSinglePatchSwapsExclusiveLabelAtomically(t *testing.T) {
	ctx := context.Background()
	operations, provider := newRealIssueOperationsWithProvider(t, ctx)
	if err := RunTx(ctx, provider, func(ctx context.Context, uw UnitOfWork) (string, error) {
		return "configure exclusive prefixes", uw.ConfigUseCase().SetConfig(ctx, labelns.ConfigKey, "tier:")
	}); err != nil {
		t.Fatalf("SetConfig(%s): %v", labelns.ConfigKey, err)
	}

	if _, err := operations.Create(ctx, issueops.CreateRequest{Actor: "tester", Issue: &issueops.Issue{
		ID: "bd-swap", Title: "swap target", IssueType: types.TypeTask, Priority: 2, Labels: []string{"tier:opus", "lane:test"},
	}}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	result, err := operations.Update(ctx, issueops.UpdateRequest{
		Actor:   "tester",
		IssueID: "bd-swap",
		Patch: issueops.IssuePatch{Labels: issueops.LabelPatch{
			Add:    []string{"tier:fable"},
			Remove: []string{"tier:opus"},
		}},
	})
	if err != nil {
		t.Fatalf("single-patch swap must succeed atomically, got: %v", err)
	}
	got := append([]string(nil), result.Issue.Labels...)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "lane:test" || got[1] != "tier:fable" {
		t.Errorf("labels after swap = %v, want [lane:test tier:fable]", got)
	}

	// Removal wins when the SAME label sits in both edits (the documented
	// LabelPatch contract) - the ordering fix must not invert that.
	second, err := operations.Update(ctx, issueops.UpdateRequest{
		Actor:   "tester",
		IssueID: "bd-swap",
		Patch: issueops.IssuePatch{Labels: issueops.LabelPatch{
			Add:    []string{"tier:fable"},
			Remove: []string{"tier:fable"},
		}},
	})
	if err != nil {
		t.Fatalf("same-label add+remove: %v", err)
	}
	// Read the hydrated result, not a bare row read: the first half already
	// proved the result carries labels, so this assertion cannot pass
	// vacuously on an unhydrated snapshot.
	if len(second.Issue.Labels) != 1 || second.Issue.Labels[0] != "lane:test" {
		t.Errorf("removal must win for a label in both Add and Remove; labels = %v", second.Issue.Labels)
	}
	_ = provider
}
