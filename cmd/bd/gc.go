package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	gcDryRun           bool
	gcForce            bool
	gcOlderThan        int
	gcSkipDecay        bool
	gcSkipDolt         bool
	gcAllowRecent      bool
	gcNoBackup         bool
	gcSkipMemoryPrune  bool
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
    same logic as 'bd memories --gc'. Skip with --skip-memory-prune.`,
	Run: func(cmd *cobra.Command, _ []string) {
		if !gcDryRun {
			CheckReadonly("gc")
		}
		ctx := rootCtx
		start := time.Now()

		if gcOlderThan < 0 {
			FatalError("--older-than must be non-negative")
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
							"Use --force to confirm or --dry-run to preview.")
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
					deleted, err := pruneExpiredMemories(time.Now())
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

	rootCmd.AddCommand(gcCmd)
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
