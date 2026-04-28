package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/ui"
)

// cohortCmd is a fork-only graph-traversal command (bda-9pc).
// Shows all issues related to <id> in 4 sections:
//   - ANCESTORS: parent-chain via DepParentChild (walk upward)
//   - DESCENDANTS: child-tree via DepParentChild (BFS downward)
//   - SIBLINGS: issues sharing 1+ labels with target (excluding ancestors+descendants+self)
//   - REFERENCED BY: issues mentioning target ID in description or notes
var cohortCmd = &cobra.Command{
	Use:     "cohort <id>",
	GroupID: "views",
	Short:   "Show all issues related to <id> via graph traversal (fork-only)",
	Long: `Graph traversal of issues related to <id>. Useful for planning + scope
understanding before starting work. Shows 4 sections:

  ANCESTORS   parent chain (DepParentChild walked upward)
  DESCENDANTS child tree (BFS downward via DepParentChild)
  SIBLINGS    issues sharing 1+ labels with <id>
  REFERENCED BY issues mentioning <id> in description or notes (text search)

Examples:
  bd cohort bda-vsn          # see what bda-vsn relates to
  bd cohort bda-116 --json   # structured output
  bd cohort bda-mxa --depth 3  # cap descendant traversal at 3 levels

Fork-only — bda-9pc.`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		depth, _ := cmd.Flags().GetInt("depth")
		if depth < 1 {
			FatalError("--depth must be at least 1")
		}
		ctx := rootCtx
		targetID := args[0]

		// Resolve target.
		target, err := store.GetIssue(ctx, targetID)
		if err != nil || target == nil {
			FatalError("issue %q not found: %v", targetID, err)
		}

		ancestors := walkAncestors(ctx, target.ID, depth)
		descendants := walkDescendants(ctx, target.ID, depth)
		excluded := make(map[string]struct{})
		excluded[target.ID] = struct{}{}
		for _, a := range ancestors {
			excluded[a.ID] = struct{}{}
		}
		for _, d := range descendants {
			excluded[d.ID] = struct{}{}
		}
		siblings := findSiblings(ctx, target, excluded)
		xrefs := findCrossRefs(ctx, target.ID, excluded)

		if jsonOutput {
			outputJSON(map[string]any{
				"target":        target,
				"ancestors":     ancestors,
				"descendants":   descendants,
				"siblings":      siblings,
				"referenced_by": xrefs,
			})
			return
		}
		displayCohort(target, ancestors, descendants, siblings, xrefs)
	},
}

// walkAncestors walks the parent chain upward via DepParentChild.
// Each issue's "parent" is found by looking at GetDependenciesWithMetadata
// and selecting the one with type=DepParentChild (a child depends-on its
// parent in the parent-child relation).
func walkAncestors(ctx context.Context, startID string, maxDepth int) []*types.Issue {
	var out []*types.Issue
	seen := make(map[string]struct{})
	current := startID
	for level := 0; level < maxDepth; level++ {
		deps, err := store.GetDependenciesWithMetadata(ctx, current)
		if err != nil {
			return out
		}
		var parent *types.Issue
		for _, d := range deps {
			if d.DependencyType == types.DepParentChild {
				p := d.Issue
				parent = &p
				break
			}
		}
		if parent == nil {
			return out
		}
		if _, ok := seen[parent.ID]; ok {
			return out // cycle guard
		}
		seen[parent.ID] = struct{}{}
		out = append(out, parent)
		current = parent.ID
	}
	return out
}

// walkDescendants does BFS on parent-child dependents.
func walkDescendants(ctx context.Context, startID string, maxDepth int) []*types.Issue {
	var out []*types.Issue
	seen := make(map[string]struct{})
	frontier := []string{startID}
	for level := 0; level < maxDepth && len(frontier) > 0; level++ {
		var next []string
		for _, id := range frontier {
			deps, err := store.GetDependentsWithMetadata(ctx, id)
			if err != nil {
				continue
			}
			for _, d := range deps {
				if d.DependencyType != types.DepParentChild {
					continue
				}
				if _, ok := seen[d.Issue.ID]; ok {
					continue
				}
				seen[d.Issue.ID] = struct{}{}
				ch := d.Issue
				out = append(out, &ch)
				next = append(next, d.Issue.ID)
			}
		}
		frontier = next
	}
	return out
}

// findSiblings returns issues that share at least one label with target,
// excluding any in the excluded set.
func findSiblings(ctx context.Context, target *types.Issue, excluded map[string]struct{}) []*types.Issue {
	if len(target.Labels) == 0 {
		return nil
	}
	const big = 5000
	results, err := store.SearchIssues(ctx, "", types.IssueFilter{
		LabelsAny: target.Labels,
		Limit:     big,
	})
	if err != nil {
		return nil
	}
	var out []*types.Issue
	for _, i := range results {
		if _, skip := excluded[i.ID]; skip {
			continue
		}
		out = append(out, i)
	}
	return out
}

// findCrossRefs scans description + notes for the target ID.
// Uses the storage layer's text-contains filters (DescriptionContains,
// NotesContains) — bd's existing query DSL.
func findCrossRefs(ctx context.Context, targetID string, excluded map[string]struct{}) []*types.Issue {
	const big = 5000
	// Two passes: description-contains and notes-contains. We dedupe afterward.
	descHits, _ := store.SearchIssues(ctx, "", types.IssueFilter{
		DescriptionContains: targetID,
		Limit:               big,
	})
	notesHits, _ := store.SearchIssues(ctx, "", types.IssueFilter{
		NotesContains: targetID,
		Limit:         big,
	})
	seen := make(map[string]struct{})
	var out []*types.Issue
	for _, i := range descHits {
		if _, skip := excluded[i.ID]; skip {
			continue
		}
		if _, dup := seen[i.ID]; dup {
			continue
		}
		seen[i.ID] = struct{}{}
		out = append(out, i)
	}
	for _, i := range notesHits {
		if _, skip := excluded[i.ID]; skip {
			continue
		}
		if _, dup := seen[i.ID]; dup {
			continue
		}
		seen[i.ID] = struct{}{}
		out = append(out, i)
	}
	return out
}

func displayCohort(
	target *types.Issue,
	ancestors, descendants, siblings, xrefs []*types.Issue,
) {
	const titleMax = 60
	trunc := func(s string) string {
		if len(s) > titleMax {
			return s[:titleMax-1] + "…"
		}
		return s
	}
	fmt.Printf("\nCohort of %s — %s\n", ui.RenderID(target.ID), trunc(target.Title))

	fmt.Printf("\n↑ ANCESTORS (%d)\n", len(ancestors))
	if len(ancestors) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, i := range ancestors {
			fmt.Printf("  %s  P%d  %s  [%s]\n", ui.RenderID(i.ID), i.Priority, trunc(i.Title), i.Status)
		}
	}

	fmt.Printf("\n↓ DESCENDANTS (%d)\n", len(descendants))
	if len(descendants) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, i := range descendants {
			fmt.Printf("  %s  P%d  %s  [%s]\n", ui.RenderID(i.ID), i.Priority, trunc(i.Title), i.Status)
		}
	}

	fmt.Printf("\n↔ SIBLINGS (%d, sharing labels)\n", len(siblings))
	if len(siblings) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, i := range siblings {
			shared := intersectLabels(target.Labels, i.Labels)
			fmt.Printf("  %s  P%d  %s  [shared: %v]\n", ui.RenderID(i.ID), i.Priority, trunc(i.Title), shared)
		}
	}

	fmt.Printf("\n@ REFERENCED BY (%d, mentions in description/notes)\n", len(xrefs))
	if len(xrefs) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, i := range xrefs {
			fmt.Printf("  %s  P%d  %s\n", ui.RenderID(i.ID), i.Priority, trunc(i.Title))
		}
	}
	fmt.Println()
}

// intersectLabels returns labels present in both a and b (preserving order from a).
func intersectLabels(a, b []string) []string {
	bset := make(map[string]struct{}, len(b))
	for _, l := range b {
		bset[l] = struct{}{}
	}
	var out []string
	for _, l := range a {
		if _, ok := bset[l]; ok {
			out = append(out, l)
		}
	}
	return out
}

func init() {
	cohortCmd.Flags().IntP("depth", "d", 5, "Max ancestor/descendant traversal depth")
	rootCmd.AddCommand(cohortCmd)
}
