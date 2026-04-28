package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// gcMinOlderThanFloor is the minimum value for --older-than unless --allow-recent is set.
// Defends against accidental same-day or recent-day deletions (fork-only safety net,
// motivated by upstream gastownhall/beads issue #3543: bd gc --older-than 30 deleted
// 82 same-day-closed beads in production with no sandbox repro).
const gcMinOlderThanFloor = 7

var (
	gcDryRun          bool
	gcForce           bool
	gcOlderThan       int
	gcSkipDecay       bool
	gcSkipDolt        bool
	gcAllowRecent     bool
	gcNoBackup        bool
	gcSkipMemoryPrune bool
	// gcSkipMemoryBackup disables the pre-delete JSONL backup written by
	// pruneExpiredMemories during the gc decay sub-phase. Default false
	// (backup ON, mirroring gcNoBackup for the issue-decay path).
	gcSkipMemoryBackup bool
	gcPlan             bool
	// gcPlanSummary emits a human-readable tabular view of GC candidates
	// instead of the JSON form. Mutually exclusive with --plan and --force.
	// Mirrors --plan's read-only contract (zero writes).
	gcPlanSummary bool
	gcOnly        string
)

var gcCmd = &cobra.Command{
	Use:     "gc",
	GroupID: "maint",
	Short:   "Garbage collect: decay old issues, compact Dolt commits, run Dolt GC",
	Long: `Full lifecycle garbage collection for standalone Beads databases.

Runs three phases in sequence:
  1. DECAY   — Delete closed issues older than N days (default 90)
  2. COMPACT — Squash old Dolt commits into fewer commits (bd compact)
  3. GC      — Run Dolt garbage collection to reclaim disk space

Each phase can be skipped individually. Use --dry-run to preview all phases
without making changes.

Examples:
  bd gc                              # Full GC with defaults (90 day decay)
  bd gc --dry-run                    # Preview what would happen
  bd gc --older-than 30              # Decay issues closed 30+ days ago
  bd gc --skip-decay                 # Skip issue deletion, just compact+GC
  bd gc --skip-dolt                  # Skip Dolt GC, just decay+compact
  bd gc --force                      # Skip confirmation prompt

Fork safety net (mitigates upstream gastownhall/beads#3543):
  - Refuses --older-than below 7 unless --allow-recent is also set.
  - Skips any candidate whose closed_at is null/zero (logs a warning).
  - Writes a .gc-backup-<unix>.jsonl inside .beads/ BEFORE any delete,
    unless --no-backup is set. Restore manually with bd import on the JSONL.

Fork extras (memory prune):
  - Decay phase also hard-deletes expired memories with expire-policy=delete,
    same logic as 'bd memories --gc'. Skip with --skip-memory-prune.

Consent flow (recommended):
  bd gc --plan                                       # JSON list of candidates, no changes
  # orchestrator (e.g. Claude AskUserQuestion) curates the IDs/keys
  bd gc --force --only=sys-1234,old-fact-key,...     # delete ONLY the curated subset
  Use 'bd gc --force' alone for the legacy wholesale path (warns when N>5).`,
	Run: func(cmd *cobra.Command, _ []string) {
		if !gcDryRun && !gcPlan && !gcPlanSummary {
			CheckReadonly("gc")
		}
		ctx := rootCtx
		start := time.Now()

		if gcOlderThan < 0 {
			FatalError("--older-than must be non-negative")
		}

		if gcPlan && gcForce {
			FatalError("--plan and --force are mutually exclusive (use --plan first to inspect, then --force --only=... to delete)")
		}
		if gcPlanSummary && (gcPlan || gcForce) {
			FatalError("--plan-summary is read-only and mutually exclusive with --plan and --force (use --plan-summary alone to eyeball candidates, --plan for JSON)")
		}

		// Parse --only allowlist (set of issue IDs and/or memory keys).
		// nil means "no allowlist; legacy behavior (delete every candidate)".
		var onlySet map[string]bool
		if strings.TrimSpace(gcOnly) != "" {
			onlySet = make(map[string]bool)
			for _, s := range strings.Split(gcOnly, ",") {
				s = strings.TrimSpace(s)
				if s != "" {
					onlySet[s] = true
				}
			}
		}

		// --plan mode: collect candidates and emit plan, exit early without modifying anything.
		if gcPlan {
			runGCPlan(gcOlderThan)
			return
		}
		// --plan-summary mode: same data as --plan, but tabular human view.
		if gcPlanSummary {
			runGCPlanSummary(gcOlderThan)
			return
		}

		// Fork safety net: refuse very small --older-than values unless explicitly allowed.
		// Defends against accidental destructive runs and the unresolved upstream issue
		// gastownhall/beads#3543 (same-day-closed beads deleted by --older-than 30).
		if !gcSkipDecay && !gcAllowRecent && gcOlderThan < gcMinOlderThanFloor {
			FatalErrorWithHint(
				fmt.Sprintf("--older-than %d is below the safety floor of %d days", gcOlderThan, gcMinOlderThanFloor),
				"Pass --allow-recent to bypass the floor (only after auditing what will be deleted with --dry-run).")
		}

		// Phase tracking for summary
		type phaseResult struct {
			name    string
			skipped bool
			detail  string
		}
		var results []phaseResult

		// ── Phase 1: DECAY ──
		if gcSkipDecay {
			results = append(results, phaseResult{name: "Decay", skipped: true})
		} else {
			if !jsonOutput {
				fmt.Println("Phase 1/3: Decay (delete old closed issues)")
			}

			cutoffDays := gcOlderThan
			cutoffTime := time.Now().AddDate(0, 0, -cutoffDays)
			statusClosed := types.StatusClosed
			filter := types.IssueFilter{
				Status:       &statusClosed,
				ClosedBefore: &cutoffTime,
			}

			closedIssues, err := store.SearchIssues(ctx, "", filter)
			if err != nil {
				FatalError("searching closed issues: %v", err)
			}

			// Filter out pinned issues AND issues with missing/zero closed_at.
			// The null-safe ClosedAt check is a fork-only defense against upstream #3543:
			// any candidate without a real ClosedAt should never be treated as "old".
			filtered := make([]*types.Issue, 0, len(closedIssues))
			skippedNullClosedAt := 0
			for _, issue := range closedIssues {
				if issue.Pinned {
					continue
				}
				if issue.ClosedAt == nil || issue.ClosedAt.IsZero() {
					skippedNullClosedAt++
					WarnError("skipping %s: closed_at is null/zero (refusing to treat as old)", issue.ID)
					continue
				}
				filtered = append(filtered, issue)
			}
			closedIssues = filtered
			if skippedNullClosedAt > 0 && !jsonOutput {
				fmt.Printf("  Skipped %d issue(s) with null/zero closed_at (fork safety net)\n", skippedNullClosedAt)
			}

			// Apply --only allowlist if set: keep only issues whose ID is in the set.
			if onlySet != nil {
				keep := closedIssues[:0]
				for _, issue := range closedIssues {
					if onlySet[issue.ID] {
						keep = append(keep, issue)
					}
				}
				closedIssues = keep
			}

			if len(closedIssues) == 0 {
				detail := fmt.Sprintf("  No closed issues older than %d days", cutoffDays)
				if !jsonOutput {
					fmt.Println(detail)
				}
				results = append(results, phaseResult{name: "Decay", detail: "0 issues deleted"})
			} else {
				if gcDryRun {
					detail := fmt.Sprintf("  Would delete %d closed issue(s)", len(closedIssues))
					if !jsonOutput {
						fmt.Println(detail)
					}
					results = append(results, phaseResult{name: "Decay", detail: fmt.Sprintf("%d issues (dry-run)", len(closedIssues))})
				} else {
					if !gcForce {
						FatalErrorWithHint(
							fmt.Sprintf("would delete %d closed issue(s) older than %d days", len(closedIssues), cutoffDays),
							"Run 'bd gc --plan' to inspect candidates as JSON, then 'bd gc --force --only=ID1,ID2,...' to delete only the curated subset. Or 'bd gc --force' to delete all (legacy).")
					}
					// Suggest the consent flow when --force is used wholesale without --only.
					if onlySet == nil && len(closedIssues) > 5 && !jsonOutput {
						fmt.Printf("  WARNING: --force without --only will delete all %d issue candidates autonomously.\n",
							len(closedIssues))
						fmt.Println("           Consider 'bd gc --plan' + AskUserQuestion + --only=... for per-item consent.")
					}

					// Pre-delete backup (fork-only safety net for upstream #3543).
					// Writes the full content of every candidate to a JSONL file inside
					// .beads/ before any DeleteIssue call. Skipped only with --no-backup.
					if !gcNoBackup {
						backupPath, err := writeGCBackup(closedIssues)
						if err != nil {
							FatalError("failed to write GC backup: %v (refusing to delete)", err)
						}
						if !jsonOutput {
							fmt.Printf("  Backup: %s (%d issue(s))\n", backupPath, len(closedIssues))
						}
					}

					deleted := 0
					for _, issue := range closedIssues {
						if err := store.DeleteIssue(ctx, issue.ID); err != nil {
							WarnError("failed to delete %s: %v", issue.ID, err)
						} else {
							deleted++
						}
					}
					commandDidWrite.Store(true)
					detail := fmt.Sprintf("  Deleted %d issue(s)", deleted)
					if !jsonOutput {
						fmt.Println(detail)
					}
					results = append(results, phaseResult{name: "Decay", detail: fmt.Sprintf("%d issues deleted", deleted)})

					// Embedded mode: flush Dolt commit after deletes.
					if isEmbeddedMode() && deleted > 0 && store != nil {
						if _, err := store.CommitPending(ctx, actor); err != nil {
							WarnError("failed to commit after decay: %v", err)
						}
					}
				}
			}
			// Memory prune sub-phase: hard-delete expired memories with
			// expire-policy=delete. Same logic as `bd memories --gc`.
			// Skipped with --skip-memory-prune.
			if !gcSkipMemoryPrune {
				if gcDryRun {
					if !jsonOutput {
						fmt.Println("  Memory prune: dry-run (use bd memories --include-expired to inspect)")
					}
				} else {
					deleted, err := pruneExpiredMemories(time.Now(), onlySet, gcSkipMemoryBackup)
					if err != nil {
						WarnError("memory prune failed: %v", err)
					} else if len(deleted) > 0 {
						commandDidWrite.Store(true)
						if !jsonOutput {
							fmt.Printf("  Memory prune: deleted %d expired memor(y/ies) with policy=delete\n", len(deleted))
						}
						if isEmbeddedMode() && store != nil {
							if _, err := store.CommitPending(ctx, actor); err != nil {
								WarnError("failed to commit after memory prune: %v", err)
							}
						}
						results = append(results, phaseResult{
							name:   "Memory prune",
							detail: fmt.Sprintf("%d memor(y/ies) deleted", len(deleted)),
						})
					} else {
						if !jsonOutput {
							fmt.Println("  Memory prune: no expired memories with policy=delete")
						}
					}
				}
			}

			if !jsonOutput {
				fmt.Println()
			}
		}

		// ── Phase 2: COMPACT (report only — actual squashing is bd flatten) ──
		if !jsonOutput {
			fmt.Println("Phase 2/3: Compact (Dolt commit history info)")
		}

		commitCount := 0
		logEntries, logErr := store.Log(ctx, 0)
		if logErr != nil {
			WarnError("could not read Dolt commit log: %v", logErr)
		} else {
			commitCount = len(logEntries)
		}

		if commitCount <= 1 {
			if !jsonOutput {
				fmt.Printf("  Only %d commit(s), nothing to compact\n\n", commitCount)
			}
			results = append(results, phaseResult{name: "Compact", detail: "nothing to compact"})
		} else {
			if gcDryRun {
				if !jsonOutput {
					fmt.Printf("  %d commits in history (use bd flatten to squash)\n\n", commitCount)
				}
				results = append(results, phaseResult{name: "Compact", detail: fmt.Sprintf("%d commits (dry-run)", commitCount)})
			} else {
				if !jsonOutput {
					fmt.Printf("  %d commits in history\n", commitCount)
					fmt.Printf("  Tip: use 'bd flatten' to squash all history to one commit\n\n")
				}
				results = append(results, phaseResult{name: "Compact", detail: fmt.Sprintf("%d commits", commitCount)})
			}
		}

		// ── Phase 3: Dolt GC ──
		if gcSkipDolt {
			results = append(results, phaseResult{name: "Dolt GC", skipped: true})
		} else {
			if !jsonOutput {
				fmt.Println("Phase 3/3: Dolt GC (reclaim disk space)")
			}

			gc, ok := storage.UnwrapStore(store).(storage.GarbageCollector)
			if !ok {
				if !jsonOutput {
					fmt.Println("  Storage backend does not support GC, skipping")
				}
				results = append(results, phaseResult{name: "Dolt GC", detail: "not supported"})
			} else if gcDryRun {
				if !jsonOutput {
					fmt.Println("  Would run DOLT_GC()")
				}
				results = append(results, phaseResult{name: "Dolt GC", detail: "dry-run"})
			} else {
				if err := gc.DoltGC(ctx); err != nil {
					WarnError("dolt gc failed: %v", err)
					results = append(results, phaseResult{name: "Dolt GC", detail: "failed"})
				} else {
					if !jsonOutput {
						fmt.Println("  Done")
					}
					results = append(results, phaseResult{name: "Dolt GC", detail: "complete"})
				}
			}
			if !jsonOutput {
				fmt.Println()
			}
		}

		elapsed := time.Since(start)

		// ── Summary ──
		if jsonOutput {
			summaryMap := make(map[string]interface{})
			summaryMap["dry_run"] = gcDryRun
			summaryMap["elapsed_ms"] = elapsed.Milliseconds()
			phases := make([]map[string]interface{}, 0, len(results))
			for _, r := range results {
				p := map[string]interface{}{
					"name":    r.name,
					"skipped": r.skipped,
				}
				if r.detail != "" {
					p["detail"] = r.detail
				}
				phases = append(phases, p)
			}
			summaryMap["phases"] = phases
			outputJSON(summaryMap)
			return
		}

		mode := "✓ GC complete"
		if gcDryRun {
			mode = "DRY RUN complete"
		}
		fmt.Printf("%s (%v)\n", mode, elapsed.Round(time.Millisecond))
		for _, r := range results {
			if r.skipped {
				fmt.Printf("  %s: skipped\n", r.name)
			} else {
				fmt.Printf("  %s: %s\n", r.name, r.detail)
			}
		}
	},
}

func init() {
	gcCmd.Flags().BoolVar(&gcDryRun, "dry-run", false, "Preview without making changes")
	gcCmd.Flags().BoolVarP(&gcForce, "force", "f", false, "Skip confirmation prompts")
	gcCmd.Flags().IntVar(&gcOlderThan, "older-than", 90, "Delete closed issues older than N days")
	gcCmd.Flags().BoolVar(&gcSkipDecay, "skip-decay", false, "Skip issue deletion phase")
	gcCmd.Flags().BoolVar(&gcSkipDolt, "skip-dolt", false, "Skip Dolt garbage collection phase")
	gcCmd.Flags().BoolVar(&gcAllowRecent, "allow-recent", false, fmt.Sprintf("Bypass the --older-than safety floor of %d days (fork-only)", gcMinOlderThanFloor))
	gcCmd.Flags().BoolVar(&gcNoBackup, "no-backup", false, "Skip pre-delete backup JSONL (fork-only — backup is on by default)")
	gcCmd.Flags().BoolVar(&gcSkipMemoryPrune, "skip-memory-prune", false, "Skip memory prune sub-phase (fork-only — by default decay also hard-deletes expired memories with expire-policy=delete, same as 'bd memories --gc')")
	gcCmd.Flags().BoolVar(&gcSkipMemoryBackup, "skip-memory-backup", false, "Skip pre-delete JSONL backup written to .beads/.gc-memory-backup-<unix>.jsonl during the memory prune sub-phase (fork-only — backup is on by default, mirrors --no-backup for issue decay)")
	gcCmd.Flags().BoolVar(&gcPlan, "plan", false, "Emit a JSON plan of the candidates that WOULD be deleted (closed issues + expired memories) without modifying anything. Mutually exclusive with --force. Use the plan to drive a per-item consent flow, then re-invoke with --force --only=ID1,key2,ID3.")
	gcCmd.Flags().BoolVar(&gcPlanSummary, "plan-summary", false, "Emit a human-readable tabular view of GC candidates instead of JSON (fork-only, read-only, mutex with --plan/--force). Sorted by age desc.")
	gcCmd.Flags().StringVar(&gcOnly, "only", "", "Comma-separated allowlist of issue IDs and/or memory keys. When set, gc deletes ONLY items in this list. Use after `bd gc --plan` to commit a curated subset.")

	rootCmd.AddCommand(gcCmd)
}

// gcPlanIssue is the wire shape of an issue candidate inside the JSON plan
// emitted by `bd gc --plan`. It surfaces just enough to let an orchestrator
// (Claude, a CI gate, an interactive UI) decide per-item whether to keep
// or delete each candidate.
type gcPlanIssue struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	ClosedAt string `json:"closed_at,omitempty"`
	AgeDays  int    `json:"age_days"`
}

// gcPlanOutput is the top-level JSON shape returned by `bd gc --plan`.
type gcPlanOutput struct {
	OlderThanDays int                      `json:"older_than_days"`
	Now           string                   `json:"now"`
	Issues        []gcPlanIssue            `json:"issues"`
	Memories      []expiredMemoryCandidate `json:"memories"`
	IssueCount    int                      `json:"issue_count"`
	MemoryCount   int                      `json:"memory_count"`
	HintNextStep  string                   `json:"hint_next_step,omitempty"`
}

// runGCPlan implements `bd gc --plan`. It reads the same candidate set that
// the decay + prune phases would touch, but performs ZERO writes. Output is
// always JSON (regardless of --json) so downstream tooling (Claude's
// AskUserQuestion, scripts, CI gates) has a stable contract.
//
// Fork-only — implements the user-consent flow requested by the bd owner:
// "non mi va bene che bd cancelli le cose vecchie senza chiedere".
func runGCPlan(olderThanDays int) {
	now := time.Now()
	cutoff := now.AddDate(0, 0, -olderThanDays)
	statusClosed := types.StatusClosed

	closedIssues, err := store.SearchIssues(rootCtx, "", types.IssueFilter{
		Status:       &statusClosed,
		ClosedBefore: &cutoff,
	})
	if err != nil {
		FatalError("plan: searching closed issues: %v", err)
	}

	plan := gcPlanOutput{
		OlderThanDays: olderThanDays,
		Now:           now.UTC().Format(time.RFC3339),
		Issues:        []gcPlanIssue{},
		Memories:      []expiredMemoryCandidate{},
	}
	for _, issue := range closedIssues {
		if issue.Pinned {
			continue
		}
		if issue.ClosedAt == nil || issue.ClosedAt.IsZero() {
			continue
		}
		closedAt := *issue.ClosedAt
		plan.Issues = append(plan.Issues, gcPlanIssue{
			ID:       issue.ID,
			Title:    issue.Title,
			ClosedAt: closedAt.UTC().Format(time.RFC3339),
			AgeDays:  int(now.Sub(closedAt).Hours() / 24),
		})
	}

	memCands, err := listExpiredMemoryCandidates(now)
	if err != nil {
		FatalError("plan: listing expired memories: %v", err)
	}
	if memCands != nil {
		plan.Memories = memCands
	}
	plan.IssueCount = len(plan.Issues)
	plan.MemoryCount = len(plan.Memories)
	if plan.IssueCount+plan.MemoryCount > 0 {
		plan.HintNextStep = "Pass approved IDs/keys via 'bd gc --force --only=<csv>' to delete only the curated subset."
	} else {
		plan.HintNextStep = "Nothing to do (no closed issues older than cutoff, no expired memories with policy=delete)."
	}

	// Always JSON for --plan. The contract is the JSON; bypass jsonOutput flag.
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(plan); err != nil {
		FatalError("plan: encode JSON: %v", err)
	}
}

// runGCPlanSummary implements `bd gc --plan-summary`. Same candidate set as
// runGCPlan, but emits a human-readable tabular view (sorted by age desc)
// instead of JSON. Fork-only — for eyeballing the GC backlog without piping
// to jq. The JSON contract via --plan stays untouched.
func runGCPlanSummary(olderThanDays int) {
	now := time.Now()
	cutoff := now.AddDate(0, 0, -olderThanDays)
	statusClosed := types.StatusClosed

	closedIssues, err := store.SearchIssues(rootCtx, "", types.IssueFilter{
		Status:       &statusClosed,
		ClosedBefore: &cutoff,
	})
	if err != nil {
		FatalError("plan-summary: searching closed issues: %v", err)
	}

	type issueRow struct {
		ID       string
		Title    string
		AgeDays  int
		Priority int
		ClosedAt time.Time
	}
	var rows []issueRow
	for _, issue := range closedIssues {
		if issue.Pinned {
			continue
		}
		if issue.ClosedAt == nil || issue.ClosedAt.IsZero() {
			continue
		}
		closedAt := *issue.ClosedAt
		rows = append(rows, issueRow{
			ID:       issue.ID,
			Title:    issue.Title,
			AgeDays:  int(now.Sub(closedAt).Hours() / 24),
			Priority: int(issue.Priority),
			ClosedAt: closedAt,
		})
	}
	// Sort by age desc (oldest first).
	sort.Slice(rows, func(i, j int) bool { return rows[i].AgeDays > rows[j].AgeDays })

	memCands, err := listExpiredMemoryCandidates(now)
	if err != nil {
		FatalError("plan-summary: listing expired memories: %v", err)
	}

	fmt.Printf("GC candidates (older-than %dd, %d issues + %d memories):\n\n",
		olderThanDays, len(rows), len(memCands))

	if len(rows) > 0 {
		fmt.Println("ISSUES:")
		// Compute column widths from data.
		idW, ageW := 8, 4
		for _, r := range rows {
			if len(r.ID) > idW {
				idW = len(r.ID)
			}
			ageStr := fmt.Sprintf("%dd", r.AgeDays)
			if len(ageStr) > ageW {
				ageW = len(ageStr)
			}
		}
		const titleMax = 60
		for _, r := range rows {
			title := r.Title
			if len(title) > titleMax {
				title = title[:titleMax-1] + "…"
			}
			fmt.Printf("  %-*s  %*dd  P%d  %s\n",
				idW, r.ID, ageW-1, r.AgeDays, r.Priority, title)
		}
	} else {
		fmt.Println("ISSUES:\n  (none older than cutoff)")
	}

	fmt.Println()
	if len(memCands) > 0 {
		fmt.Println("MEMORIES:")
		keyW := 12
		for _, m := range memCands {
			if len(m.Key) > keyW {
				keyW = len(m.Key)
			}
		}
		const contentMax = 60
		for _, m := range memCands {
			content := strings.ReplaceAll(m.Content, "\n", " ")
			if len(content) > contentMax {
				content = content[:contentMax-1] + "…"
			}
			validUntil := m.ValidUntil
			if len(validUntil) > 10 {
				validUntil = validUntil[:10]
			}
			fmt.Printf("  %-*s  expired %s  %s\n", keyW, m.Key, validUntil, content)
		}
	} else {
		fmt.Println("MEMORIES:\n  (none expired with policy=delete)")
	}

	fmt.Println()
	if len(rows)+len(memCands) > 0 {
		fmt.Println("Run 'bd gc --plan' for the JSON form (orchestration-friendly),")
		fmt.Println("or 'bd gc --force --only=ID1,key2,...' to delete the curated subset.")
	} else {
		fmt.Println("Nothing to do.")
	}
}

// writeGCBackup serializes the given issues to a JSONL file inside .beads/
// before any DeleteIssue call. Returns the absolute path of the backup on success.
// Fork-only safeguard motivated by upstream gastownhall/beads#3543.
func writeGCBackup(issues []*types.Issue) (string, error) {
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		// Fall back to current working directory if .beads/ can't be located.
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("locate beads dir or cwd: %w", err)
		}
		beadsDir = cwd
	}
	name := fmt.Sprintf(".gc-backup-%d.jsonl", time.Now().Unix())
	path := filepath.Join(beadsDir, name)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // contains no secrets, only issue content
	if err != nil {
		return "", fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, issue := range issues {
		if err := enc.Encode(issue); err != nil {
			return "", fmt.Errorf("encode %s: %w", issue.ID, err)
		}
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("fsync %s: %w", path, err)
	}
	return path, nil
}
