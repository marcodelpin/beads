// Package main — bd group: gate/release a GROUP of issues with one deferred gate.
//
// Native port of the ~/.claude/scripts/bd-group-gate.py bridge (bda-1y1). A single
// "GATE" issue (labels meta-gate, focus:<name>, gated-group) is DEFERRED — deferred
// hides it from `bd ready` while it STILL acts as a non-closed blocker. Each member is
// wired blocked-by the GATE and tagged focus:<name>-parked, so everything ready-based
// (bd ready, /go, /bdloop, /dddrain, /sssug) surfaces only the non-gated work. RELEASE
// the whole group in one toggle: close the GATE → every dependent auto-unblocks.
//
// `bd group` is a distinct namespace from the upstream `bd gate` (async coordination
// gates for formula steps) — different concept, no collision.
package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/ui"
	"github.com/steveyegge/beads/internal/utils"
)

var groupCmd = &cobra.Command{
	Use:     "group",
	GroupID: "issues",
	Short:   "Gate or release a group of issues with one deferred gate",
	Long: `Gate (park) or release a GROUP of issues in one toggle via a single deferred GATE issue.

A GATE issue (labels: meta-gate, focus:<name>, gated-group) is DEFERRED so it is hidden
from 'bd ready' while still acting as an active (non-closed) blocker. Each member is wired
blocked-by the GATE and tagged focus:<name>-parked, so 'bd ready' (and /go, /bdloop,
/dddrain, /sssug) surfaces only the non-gated work. Release the whole group in one toggle:
closing the GATE auto-unblocks every dependent.

This is distinct from 'bd gate' (async coordination gates for formula steps).

Examples:
  bd group create  --focus dvr   --members bd-a,bd-b,bd-c   # park 3 issues behind a dvr gate
  bd group create  --focus dvr   --members @ids.txt          # members from a file
  echo "bd-a bd-b" | bd group create --focus dvr --members - # members from stdin
  bd group status  --focus dvr                               # show the gate + parked members
  bd group release --focus dvr                               # close the gate, unblock the group`,
}

func groupGateLabels(focus string) []string { return []string{"meta-gate", "focus:" + focus} }
func groupParkedLabel(focus string) string  { return "focus:" + focus + "-parked" }

// findGroupGate returns the id of the active (non-closed) GATE for focus, or "".
// Labels match is exact-equality AND-join (focus:test never matches focus:test2).
func findGroupGate(focus string) (string, error) {
	issues, err := store.SearchIssues(rootCtx, "", types.IssueFilter{Labels: groupGateLabels(focus)})
	if err != nil {
		return "", err
	}
	for _, i := range issues {
		if i.Status != types.StatusClosed {
			return i.ID, nil
		}
	}
	return "", nil
}

// groupParkedMembers returns ids of non-closed issues tagged focus:<name>-parked, sorted.
func groupParkedMembers(focus string) ([]string, error) {
	issues, err := store.SearchIssues(rootCtx, "", types.IssueFilter{Labels: []string{groupParkedLabel(focus)}})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		if i.Status != types.StatusClosed {
			out = append(out, i.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// gateDependentIDs returns the set of issue ids currently blocked-by gateID
// (i.e. already wired to the gate). One query — no per-member round-trips.
func gateDependentIDs(gateID string) (map[string]bool, error) {
	deps, err := store.GetDependents(rootCtx, gateID)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(deps))
	for _, d := range deps {
		set[d.ID] = true
	}
	return set, nil
}

// parseGroupMembers expands --members: "-" = stdin, "@path" = file, else comma/space list.
func parseGroupMembers(spec string) ([]string, error) {
	switch {
	case spec == "-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("reading stdin: %w", err)
		}
		spec = string(b)
	case strings.HasPrefix(spec, "@"):
		b, err := os.ReadFile(spec[1:])
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", spec[1:], err)
		}
		spec = string(b)
	}
	toks := strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		if t != "" {
			out = append(out, t)
		}
	}
	return out, nil
}

// resolveGroupMembers resolves partial IDs (de-duped), warning + skipping unresolvable ones.
func resolveGroupMembers(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, id := range raw {
		full, err := utils.ResolvePartialID(rootCtx, store, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s skip %s: %v\n", ui.RenderWarn("!"), id, err)
			continue
		}
		if seen[full] {
			continue
		}
		seen[full] = true
		out = append(out, full)
	}
	return out
}

var groupCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create/extend a group gate: park members behind one deferred gate",
	RunE: func(cmd *cobra.Command, args []string) error {
		CheckReadonly("group create")
		focus := strings.TrimSpace(flagString(cmd, "focus"))
		if focus == "" {
			return HandleErrorRespectJSON("--focus is required")
		}
		raw, err := parseGroupMembers(flagString(cmd, "members"))
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		if len(raw) == 0 {
			return HandleErrorRespectJSON("no members supplied (--members)")
		}
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		gateTitle := strings.TrimSpace(flagString(cmd, "gate-title"))
		ctx := rootCtx

		members := resolveGroupMembers(raw)
		if len(members) == 0 {
			return HandleErrorRespectJSON("no members resolved (all ids unresolvable)")
		}
		gate, err := findGroupGate(focus)
		if err != nil {
			return HandleErrorRespectJSON("looking up gate: %v", err)
		}

		// Phase 1: find-or-create the GATE, then ALWAYS assert it is deferred (self-heals a
		// half-created gate left open by an earlier crash between create and defer — HIGH-2).
		freshGate := gate == ""
		if dryRun {
			if freshGate {
				fmt.Printf("DRY: create+defer gate (labels=meta-gate,focus:%s,gated-group,effort:low)\n", focus)
				gate = "<NEW-GATE>"
			} else {
				fmt.Printf("DRY: reuse + assert-deferred gate %s\n", gate)
			}
			for _, m := range members {
				if m == gate {
					continue
				}
				fmt.Printf("DRY: dep add %s blocked-by %s ; label add %s %s\n", m, gate, m, groupParkedLabel(focus))
			}
			fmt.Printf("DRY: gate=%s focus=%s members=%d\n", gate, focus, len(members))
			return nil
		}
		if freshGate {
			title := gateTitle
			if title == "" {
				title = fmt.Sprintf("GATE: %s-focus — non-%s work parked", focus, focus)
			}
			desc := fmt.Sprintf("META-GATE hub (bd group, focus:%s). Members are blocked-by this "+
				"issue so `bd ready` surfaces only the non-gated work. Release in one toggle: "+
				"`bd group release --focus %s` (closes this gate; dependents auto-unblock).", focus, focus)
			gateIssue := buildCreateIssue(createIssueParams{
				Title:       title,
				Description: desc,
				Priority:    4,
				IssueType:   types.TypeTask,
				Labels:      []string{"meta-gate", "focus:" + focus, "gated-group", "effort:low"},
				CreatedBy:   getActorWithGit(),
				Owner:       getOwner(),
			})
			if err := store.CreateIssue(ctx, gateIssue, actor); err != nil {
				return HandleErrorRespectJSON("creating gate: %v", err)
			}
			gate = gateIssue.ID
		}
		// Assert deferred (idempotent — covers fresh create AND a reused-but-undeferred gate).
		if err := store.UpdateIssue(ctx, gate, map[string]interface{}{
			"status": string(types.StatusDeferred),
			"notes":  "meta-gate hub focus:" + focus + " — not real work (deferred: hidden from ready, still blocks)",
		}, actor); err != nil {
			return HandleErrorRespectJSON("deferring gate %s: %v", gate, err)
		}
		commandDidWrite.Store(true)
		if !jsonOutput {
			if freshGate {
				fmt.Printf("%s created + deferred gate %s (focus:%s)\n", ui.RenderPass("✓"), gate, focus)
			} else {
				fmt.Printf("reusing gate %s (focus:%s)\n", gate, focus)
			}
		}

		// Phase 2: wire members (dep + parked label) atomically. One read of the gate's
		// existing dependents tells us which members are already wired (idempotent re-run).
		existing, err := gateDependentIDs(gate)
		if err != nil {
			return HandleErrorRespectJSON("reading gate dependents: %v", err)
		}
		parked := groupParkedLabel(focus)
		wiredNew, already := 0, 0
		commitMsg := fmt.Sprintf("bd: group create focus:%s — park %d issue(s) behind %s", focus, len(members), gate)
		err = transactHonoringAutoCommit(ctx, store, commitMsg, func(tx storage.Transaction) error {
			for _, m := range members {
				if m == gate {
					continue // never park the gate behind itself
				}
				if existing[m] {
					already++
				} else {
					// tx.AddDependency cycle-checks by default and rolls back the whole tx on a cycle.
					dep := &types.Dependency{IssueID: m, DependsOnID: gate, Type: types.DepBlocks}
					if err := tx.AddDependency(ctx, dep, actor); err != nil {
						return fmt.Errorf("dep add %s blocked-by %s: %w", m, gate, err)
					}
					wiredNew++
				}
				if err := tx.AddLabel(ctx, m, parked, actor); err != nil { // INSERT IGNORE — idempotent
					return fmt.Errorf("label add %s %s: %w", m, parked, err)
				}
			}
			return nil
		})
		if err != nil {
			return HandleErrorRespectJSON("group create: %v", err)
		}
		commandDidWrite.Store(true)
		warnIfCyclesExist(store)

		if jsonOutput {
			outputJSON(map[string]interface{}{
				"gate": gate, "focus": focus, "members": len(members),
				"wired_new": wiredNew, "already": already,
			})
			return nil
		}
		fmt.Printf("%s gate=%s focus=%s members=%d wired_new=%d already=%d\n",
			ui.RenderPass("✓"), gate, focus, len(members), wiredNew, already)
		return nil
	},
}

var groupReleaseCmd = &cobra.Command{
	Use:   "release",
	Short: "Release a group: close the gate, unblock + un-park all members",
	RunE: func(cmd *cobra.Command, args []string) error {
		CheckReadonly("group release")
		focus := strings.TrimSpace(flagString(cmd, "focus"))
		if focus == "" {
			return HandleErrorRespectJSON("--focus is required")
		}
		keepLabels, _ := cmd.Flags().GetBool("keep-labels")
		keepDeps, _ := cmd.Flags().GetBool("keep-deps")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		ctx := rootCtx

		gate, err := findGroupGate(focus)
		if err != nil {
			return HandleErrorRespectJSON("looking up gate: %v", err)
		}
		if gate == "" {
			return HandleErrorRespectJSON("no gate found for focus:%s", focus)
		}

		// Reconcile BOTH sources of truth so divergence (a hand-removed label, a partially
		// applied create) cannot leave dangling edges: members wired blocked-by the gate
		// (authoritative for deps) ∪ members carrying the parked label (authoritative for labels).
		depSet, err := gateDependentIDs(gate)
		if err != nil {
			return HandleErrorRespectJSON("reading gate dependents: %v", err)
		}
		labelMembers, err := groupParkedMembers(focus)
		if err != nil {
			return HandleErrorRespectJSON("listing parked members: %v", err)
		}
		parked := groupParkedLabel(focus)
		union := map[string]bool{}
		for m := range depSet {
			union[m] = true
		}
		for _, m := range labelMembers {
			union[m] = true
		}
		members := make([]string, 0, len(union))
		for m := range union {
			members = append(members, m)
		}
		sort.Strings(members)

		if dryRun {
			for _, m := range members {
				fmt.Printf("DRY: dep remove %s %s ; label remove %s %s\n", m, gate, m, parked)
			}
			fmt.Printf("DRY: close %s → %d members unblocked\n", gate, len(members))
			return nil
		}

		commitMsg := fmt.Sprintf("bd: group release focus:%s — unblock %d issue(s) + close %s", focus, len(members), gate)
		err = transactHonoringAutoCommit(ctx, store, commitMsg, func(tx storage.Transaction) error {
			for _, m := range members {
				if !keepDeps && depSet[m] {
					if err := tx.RemoveDependency(ctx, m, gate, actor); err != nil {
						return fmt.Errorf("dep remove %s -> %s: %w", m, gate, err)
					}
				}
				if !keepLabels && slices.Contains(labelMembers, m) {
					if err := tx.RemoveLabel(ctx, m, parked, actor); err != nil {
						return fmt.Errorf("label remove %s %s: %w", m, parked, err)
					}
				}
			}
			if err := tx.UpdateIssue(ctx, gate, map[string]interface{}{
				"status": string(types.StatusClosed),
				"notes":  "released group focus:" + focus,
			}, actor); err != nil {
				return fmt.Errorf("close gate %s: %w", gate, err)
			}
			return nil
		})
		if err != nil {
			return HandleErrorRespectJSON("group release: %v", err)
		}
		commandDidWrite.Store(true)

		if jsonOutput {
			outputJSON(map[string]interface{}{"gate": gate, "focus": focus, "released": len(members)})
			return nil
		}
		fmt.Printf("%s closed gate %s → %d members unblocked (focus:%s)\n",
			ui.RenderPass("✓"), gate, len(members), focus)
		return nil
	},
}

var groupStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the gate and parked members for a focus",
	RunE: func(cmd *cobra.Command, args []string) error {
		focus := strings.TrimSpace(flagString(cmd, "focus"))
		if focus == "" {
			return HandleErrorRespectJSON("--focus is required")
		}
		gate, err := findGroupGate(focus)
		if err != nil {
			return HandleErrorRespectJSON("looking up gate: %v", err)
		}
		members, err := groupParkedMembers(focus)
		if err != nil {
			return HandleErrorRespectJSON("listing parked members: %v", err)
		}
		if jsonOutput {
			outputJSON(map[string]interface{}{"gate": gate, "focus": focus, "parked": members})
			return nil
		}
		if gate == "" {
			fmt.Printf("no gate for focus:%s\n", focus)
			return nil
		}
		fmt.Printf("%s GATE %s (focus:%s) — %d parked member(s):\n", ui.RenderAccent("●"), gate, focus, len(members))
		for _, m := range members {
			fmt.Printf("  - %s\n", m)
		}
		return nil
	},
}

// flagString reads a string flag (empty string if unset/error).
func flagString(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}

func init() {
	for _, c := range []*cobra.Command{groupCreateCmd, groupReleaseCmd, groupStatusCmd} {
		c.Flags().String("focus", "", "Focus name identifying the group/gate (required)")
	}
	groupCreateCmd.Flags().String("members", "", "Members: comma/space list, '-' for stdin, or '@file' (required)")
	groupCreateCmd.Flags().String("gate-title", "", "Custom gate issue title (optional)")
	groupCreateCmd.Flags().Bool("dry-run", false, "Print intended actions without writing")
	groupReleaseCmd.Flags().Bool("keep-labels", false, "Keep the focus:<name>-parked labels on members")
	groupReleaseCmd.Flags().Bool("keep-deps", false, "Keep the blocked-by dependencies on members")
	groupReleaseCmd.Flags().Bool("dry-run", false, "Print intended actions without writing")

	groupCmd.AddCommand(groupCreateCmd)
	groupCmd.AddCommand(groupReleaseCmd)
	groupCmd.AddCommand(groupStatusCmd)
	rootCmd.AddCommand(groupCmd)
}
