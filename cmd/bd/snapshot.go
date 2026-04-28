package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/ui"
)

// snapshotCmd is a fork-only "quick dashboard" command (bda-c7h).
// Emits a single-screen view: in_progress + recent closed + recent created
// + stats footer. Faster orientation than running 3 separate `bd list` calls.
var snapshotCmd = &cobra.Command{
	Use:     "snapshot",
	GroupID: "views",
	Short:   "Quick dashboard: in_progress + recent closed + recent created (fork-only)",
	Long: `Quick dashboard for fast session orientation. Shows three sections:
  - IN PROGRESS: issues currently claimed (capped, sorted by start time desc)
  - RECENT CLOSED: closed within --window-hours (default 24h)
  - RECENT CREATED: created within --window-hours (default 24h)
  - STATS: total counts by status + priority breakdown for open

Examples:
  bd snapshot                     # default: 24h window, 10 per section
  bd snapshot --window-hours 168  # last week
  bd snapshot --cap 25            # show up to 25 per section
  bd snapshot --json              # structured output for orchestration

Fork-only — bda-c7h.`,
	Run: func(cmd *cobra.Command, args []string) {
		windowHours, _ := cmd.Flags().GetInt("window-hours")
		capN, _ := cmd.Flags().GetInt("cap")
		if windowHours < 1 {
			FatalError("--window-hours must be at least 1")
		}
		if capN < 1 {
			FatalError("--cap must be at least 1")
		}
		ctx := rootCtx
		now := time.Now()
		cutoff := now.Add(-time.Duration(windowHours) * time.Hour)

		// Section 1: in_progress
		statusIP := types.StatusInProgress
		ipIssues, err := store.SearchIssues(ctx, "", types.IssueFilter{
			Status: &statusIP,
			Limit:  capN,
		})
		if err != nil {
			FatalError("snapshot: in_progress query: %v", err)
		}

		// Section 2: recently closed
		statusClosed := types.StatusClosed
		closedIssues, err := store.SearchIssues(ctx, "", types.IssueFilter{
			Status:      &statusClosed,
			ClosedAfter: &cutoff,
			Limit:       capN,
		})
		if err != nil {
			FatalError("snapshot: closed query: %v", err)
		}

		// Section 3: recently created (any status)
		createdIssues, err := store.SearchIssues(ctx, "", types.IssueFilter{
			CreatedAfter: &cutoff,
			Limit:        capN,
		})
		if err != nil {
			FatalError("snapshot: created query: %v", err)
		}

		// Stats: counts by status; for open also break down by priority.
		_ = ctx // ctx captured by computeSnapshotStats via rootCtx
		stats := computeSnapshotStats()

		if jsonOutput {
			outputJSON(map[string]any{
				"window_hours":    windowHours,
				"now":             now.UTC().Format(time.RFC3339),
				"in_progress":     ipIssues,
				"recent_closed":   closedIssues,
				"recent_created":  createdIssues,
				"stats":           stats,
				"in_progress_cap": capN,
			})
			return
		}
		displaySnapshot(ipIssues, closedIssues, createdIssues, stats, windowHours, now)
	},
}

// snapshotStats holds aggregate counts for the dashboard footer.
type snapshotStats struct {
	OpenTotal  int            `json:"open_total"`
	OpenByPrio map[string]int `json:"open_by_priority"` // P0..P4
	InProgress int            `json:"in_progress"`
	Blocked    int            `json:"blocked"`
}

func computeSnapshotStats() snapshotStats {
	// Use SearchIssues with high Limit to count buckets.
	// (bd has no dedicated count-by-status API in the fork-shipped version;
	// the existing 'bd count --by-status' pulls all issues internally too.)
	const big = 100000
	out := snapshotStats{OpenByPrio: make(map[string]int)}

	statusOpen := types.StatusOpen
	openIssues, _ := store.SearchIssues(rootCtx, "", types.IssueFilter{
		Status: &statusOpen,
		Limit:  big,
	})
	out.OpenTotal = len(openIssues)
	for _, i := range openIssues {
		key := fmt.Sprintf("P%d", i.Priority)
		out.OpenByPrio[key]++
	}

	statusIP := types.StatusInProgress
	ip, _ := store.SearchIssues(rootCtx, "", types.IssueFilter{
		Status: &statusIP,
		Limit:  big,
	})
	out.InProgress = len(ip)

	statusBlocked := types.StatusBlocked
	bl, _ := store.SearchIssues(rootCtx, "", types.IssueFilter{
		Status: &statusBlocked,
		Limit:  big,
	})
	out.Blocked = len(bl)

	return out
}

// formatRelTime returns a short human relative time like "5h ago", "30m ago",
// "2d ago". Empty string if t is zero/nil.
func formatRelTime(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 48*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func displaySnapshot(
	ipIssues, closedIssues, createdIssues []*types.Issue,
	stats snapshotStats,
	windowHours int,
	now time.Time,
) {
	const titleMax = 60

	truncTitle := func(s string) string {
		if len(s) > titleMax {
			return s[:titleMax-1] + "…"
		}
		return s
	}

	// Section: IN PROGRESS
	fmt.Printf("\n%s IN PROGRESS (%d)\n", "◐", len(ipIssues))
	if len(ipIssues) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, i := range ipIssues {
			started := ""
			if i.StartedAt != nil && !i.StartedAt.IsZero() {
				started = " (started " + formatRelTime(now, *i.StartedAt) + ")"
			}
			fmt.Printf("  %s  P%d  %s%s\n",
				ui.RenderID(i.ID), i.Priority, truncTitle(i.Title), started)
		}
	}

	// Section: RECENT CLOSED
	fmt.Printf("\n%s RECENT CLOSED (last %dh, %d)\n", ui.RenderPass("✓"), windowHours, len(closedIssues))
	if len(closedIssues) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, i := range closedIssues {
			closedRel := ""
			if i.ClosedAt != nil && !i.ClosedAt.IsZero() {
				closedRel = " " + formatRelTime(now, *i.ClosedAt)
			}
			fmt.Printf("  %s  closed%s  %s\n",
				ui.RenderID(i.ID), closedRel, truncTitle(i.Title))
		}
	}

	// Section: RECENT CREATED
	fmt.Printf("\n%s RECENT CREATED (last %dh, %d)\n", "○", windowHours, len(createdIssues))
	if len(createdIssues) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, i := range createdIssues {
			createdRel := ""
			if !i.CreatedAt.IsZero() {
				createdRel = " " + formatRelTime(now, i.CreatedAt)
			}
			fmt.Printf("  %s  P%d  %s%s\n",
				ui.RenderID(i.ID), i.Priority, truncTitle(i.Title), createdRel)
		}
	}

	// Footer: STATS
	fmt.Printf("\n%s STATS  open=%d", "∑", stats.OpenTotal)
	if stats.OpenTotal > 0 {
		fmt.Printf(" (")
		first := true
		for _, p := range []string{"P0", "P1", "P2", "P3", "P4"} {
			if n, ok := stats.OpenByPrio[p]; ok && n > 0 {
				if !first {
					fmt.Printf(", ")
				}
				fmt.Printf("%s=%d", p, n)
				first = false
			}
		}
		fmt.Printf(")")
	}
	fmt.Printf("  in_progress=%d  blocked=%d\n\n", stats.InProgress, stats.Blocked)
}

func init() {
	snapshotCmd.Flags().IntP("window-hours", "w", 24, "Time window in hours for recent closed/created sections")
	snapshotCmd.Flags().IntP("cap", "n", 10, "Maximum issues per section")
	rootCmd.AddCommand(snapshotCmd)
}
