package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/metrics"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/versioncontrolops"
	"github.com/steveyegge/beads/internal/ui"
)

// `bd conflicts` is the operator surface for a merge that halted (federation
// ask #3). Without it, resolving means dropping into the raw dolt CLI inside
// .beads/dolt/<db> — `dolt conflicts cat issues`, `dolt conflicts resolve
// --ours issues`, `dolt add -A && dolt commit` — whose flag surface differs
// from git's just enough to bite. Here the same work is issue-oriented: which
// issues are conflicted, what each side says field by field, and a resolution
// that can name a single issue instead of a whole table.
//
// It reads the LIVE working set. beads' own pull path auto-resolves the safe
// conflict classes and aborts anything else (restoring the working set), so
// what lands here is what a CLI-level pull — the federation bridge's — left
// behind, plus anything a `bd vc merge` left unresolved.

const conflictsResolveHint = "Resolve with: bd conflicts resolve <issue-id> --ours|--theirs"

var conflictsCmd = &cobra.Command{
	Use:     "conflicts",
	GroupID: "sync",
	Short:   "Inspect and resolve live merge conflicts",
	Long: `Inspect and resolve the merge conflicts sitting in the working set.

Conflicts appear when a pull or merge brought in changes that collide with
local ones and could not be settled automatically. These commands present them
per issue and per field, and resolve them without the raw dolt CLI.

Examples:
  bd conflicts list                          # which tables and issues are conflicted
  bd conflicts show                          # every conflicted row, field by field
  bd conflicts show bd-1234                  # one issue
  bd conflicts resolve bd-1234 --ours        # keep our side of one issue
  bd conflicts resolve --all --theirs        # take their side of everything`,
}

var (
	conflictsShowAllFields bool
	conflictsShowTable     string
	conflictsResolveOurs   bool
	conflictsResolveTheirs bool
	conflictsResolveStrat  string
	conflictsResolveAll    bool
	conflictsResolveTable  string
	conflictsNoCommit      bool
)

var conflictsListCmd = &cobra.Command{
	Use:           "list",
	Short:         "List tables and issues with live merge conflicts",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		defer conflictsMetrics("conflicts-list")()
		ctx := rootCtx
		if err := requireConflictSupport(); err != nil {
			return err
		}
		tables, err := conflictedTables(ctx)
		if err != nil {
			return HandleErrorRespectJSON("failed to read conflicts: %v", err)
		}
		type tableOut struct {
			Table string   `json:"table"`
			Count int      `json:"count"`
			Keys  []string `json:"keys,omitempty"`
		}
		out := make([]tableOut, 0, len(tables))
		total := 0
		for _, t := range tables {
			rows, err := conflictRows(ctx, t.Field)
			if err != nil {
				return HandleErrorRespectJSON("failed to read conflicts for %s: %v", t.Field, err)
			}
			keys := make([]string, 0, len(rows))
			for _, r := range rows {
				if r.Key != "" {
					keys = append(keys, r.Key)
				}
			}
			// dolt_conflicts' own count is authoritative for tables whose
			// rows we could not key (keyless tables list no keys).
			count := len(rows)
			if count == 0 {
				count = t.Count
			}
			total += count
			out = append(out, tableOut{Table: t.Field, Count: count, Keys: keys})
		}
		if jsonOutput {
			return outputJSON(map[string]interface{}{
				"conflicts": total,
				"tables":    out,
			})
		}
		if total == 0 {
			fmt.Println("No merge conflicts.")
			return nil
		}
		fmt.Printf("\n%s %d live merge conflict(s):\n\n", ui.RenderAccent("!!"), total)
		for _, t := range out {
			fmt.Printf("  %s (%d)\n", ui.RenderAccent(t.Table), t.Count)
			for _, k := range t.Keys {
				fmt.Printf("    %s\n", k)
			}
		}
		fmt.Printf("\nInspect with: bd conflicts show [<issue-id>]\n%s\n\n", conflictsResolveHint)
		return nil
	},
}

var conflictsShowCmd = &cobra.Command{
	Use:   "show [<issue-id>]",
	Short: "Show conflicted rows field by field (base/ours/theirs)",
	Long: `Show each conflicted row with its fields side by side.

Only fields where our side and their side disagree are shown; --all-fields
shows every column. Without an issue ID, every conflicted row of every
conflicted table is shown.`,
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		defer conflictsMetrics("conflicts-show")()
		ctx := rootCtx
		if err := requireConflictSupport(); err != nil {
			return err
		}
		wantKey := ""
		if len(args) == 1 {
			wantKey = args[0]
		}

		var tables []string
		if conflictsShowTable != "" {
			tables = []string{conflictsShowTable}
		} else {
			ts, err := conflictedTables(ctx)
			if err != nil {
				return HandleErrorRespectJSON("failed to read conflicts: %v", err)
			}
			for _, t := range ts {
				tables = append(tables, t.Field)
			}
		}

		var matched []storage.ConflictRow
		for _, table := range tables {
			rows, err := conflictRows(ctx, table)
			if err != nil {
				return HandleErrorRespectJSON("failed to read conflicts for %s: %v", table, err)
			}
			for _, r := range rows {
				if wantKey != "" && r.Key != wantKey {
					continue
				}
				matched = append(matched, r)
			}
		}

		if jsonOutput {
			return outputJSON(map[string]interface{}{
				"conflicts": len(matched),
				"rows":      filterShownFields(matched, conflictsShowAllFields),
			})
		}
		if len(matched) == 0 {
			if wantKey != "" {
				fmt.Printf("No live merge conflict for %s.\n", wantKey)
			} else {
				fmt.Println("No merge conflicts.")
			}
			return nil
		}
		for _, r := range matched {
			printConflictRow(r, conflictsShowAllFields)
		}
		fmt.Printf("%s\n\n", conflictsResolveHint)
		return nil
	},
}

var conflictsResolveCmd = &cobra.Command{
	Use:   "resolve [<issue-id>...]",
	Short: "Resolve merge conflicts with --ours or --theirs",
	Long: `Resolve live merge conflicts, then conclude the merge with a commit.

Named issue IDs are resolved row by row, leaving every other conflicted row
alone. --all resolves whole tables at once (dolt's own table-level
resolution). The merge is committed only once NO conflicts remain, so a
partial resolution leaves the merge open for the next pass.

Row-by-row resolution requires the row to exist on both sides: when one side
deleted it, resolve that table wholesale or edit the row directly.

Examples:
  bd conflicts resolve bd-1234 --ours              # keep our side of one issue
  bd conflicts resolve bd-1234 bd-5678 --theirs    # take their side of two
  bd conflicts resolve --all --ours                # every conflicted table
  bd conflicts resolve --all --table config --theirs`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		defer conflictsMetrics("conflicts-resolve")()
		ctx := rootCtx
		if err := requireConflictSupport(); err != nil {
			return err
		}
		strategy, err := resolveStrategy()
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		if conflictsResolveAll == (len(args) > 0) {
			return HandleErrorRespectJSON("name the issue IDs to resolve, or pass --all (not both)")
		}

		// Pre-resolution HEAD scopes the is_blocked recompute the merged-in
		// writes bypassed, exactly as bd vc merge does (bd-578h9.11).
		preHead, _ := store.GetCurrentCommit(ctx)

		resolved := 0
		if conflictsResolveAll {
			tables := []string{}
			if conflictsResolveTable != "" {
				tables = append(tables, conflictsResolveTable)
			} else {
				ts, err := conflictedTables(ctx)
				if err != nil {
					return HandleErrorRespectJSON("failed to read conflicts: %v", err)
				}
				for _, t := range ts {
					tables = append(tables, t.Field)
				}
			}
			if len(tables) == 0 {
				fmt.Println("No merge conflicts.")
				return nil
			}
			for _, table := range tables {
				rows, err := conflictRows(ctx, table)
				if err != nil {
					return HandleErrorRespectJSON("failed to read conflicts for %s: %v", table, err)
				}
				if err := store.ResolveConflicts(ctx, table, strategy); err != nil {
					return HandleErrorRespectJSON("failed to resolve conflicts in %s: %v", table, err)
				}
				resolved += len(rows)
			}
		} else {
			table := conflictsResolveTable
			if table == "" {
				table = "issues"
			}
			inspector, ok := conflictInspector()
			if !ok {
				return HandleErrorRespectJSON("this backend does not support per-issue conflict resolution; use --all")
			}
			if !versioncontrolops.SupportsRowResolve(table) {
				return HandleErrorRespectJSON("per-issue resolution is not supported for table %s; use --all --table %s", table, table)
			}
			n, err := inspector.ResolveConflictRows(ctx, table, args, strategy)
			if err != nil {
				// A partial resolution is real state: report what landed.
				if n > 0 {
					return HandleErrorRespectJSON("resolved %d of %d conflict(s) before failing: %v", n, len(args), err)
				}
				return HandleErrorRespectJSON("failed to resolve conflicts: %v", err)
			}
			resolved = n
		}

		remaining, err := totalConflicts(ctx)
		if err != nil {
			return HandleErrorRespectJSON("resolved %d conflict(s) but failed to re-check conflicts: %v", resolved, err)
		}

		committed := false
		if resolved > 0 && remaining == 0 && !conflictsNoCommit {
			// CommitMergeResolution, not Commit: server-mode Commit excludes
			// the config table (GH#2455), so a resolved config conflict would
			// be dropped and the merge would re-wedge on the next pull
			// (GH#2474).
			msg := fmt.Sprintf("Resolve %d merge conflict(s) using %s strategy", resolved, strategy)
			if err := store.CommitMergeResolution(ctx, msg); err != nil {
				return HandleErrorRespectJSON("conflicts resolved but commit failed: %v", err)
			}
			committed = true
			// Same unwrap rule as conflictInspector: RecomputeBlockedAfterMerge
			// lives on the concrete Dolt store, not on DoltStorage.
			if rs, ok := storage.UnwrapStore(store).(interface {
				RecomputeBlockedAfterMerge(ctx context.Context, fromCommit string) error
			}); ok {
				if err := rs.RecomputeBlockedAfterMerge(ctx, preHead); err != nil {
					return HandleErrorRespectJSON("conflicts resolved but is_blocked recompute failed: %v", err)
				}
			}
		}

		if jsonOutput {
			return outputJSON(map[string]interface{}{
				"resolved":  resolved,
				"strategy":  strategy,
				"remaining": remaining,
				"committed": committed,
			})
		}
		fmt.Printf("Resolved %d conflict(s) using '%s'.\n", resolved, strategy)
		switch {
		case remaining > 0:
			fmt.Printf("%d conflict(s) remain; the merge is not committed yet.\nRun: bd conflicts list\n", remaining)
		case committed:
			fmt.Println("Merge committed. Push when ready: bd sync")
		case resolved == 0:
			fmt.Println("Nothing was conflicted; no commit made.")
		default:
			fmt.Println("All conflicts resolved; commit withheld. Conclude the merge with: bd dolt commit")
		}
		return nil
	},
}

// conflictsMetrics starts a command metrics event and returns its closer, so
// each subcommand can arm it with one deferred call.
func conflictsMetrics(name string) func() {
	evt := metrics.NewCommandEvent(name)
	return func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}
}

// requireConflictSupport refuses the modes where conflict inspection has no
// meaning: a proxied server owns its own working set, and a non-Dolt backend
// has no merge state at all.
func requireConflictSupport() error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("bd conflicts is not supported in proxied-server mode")
	}
	if store == nil {
		return HandleErrorRespectJSON("no database open")
	}
	return nil
}

// conflictedTables returns the tables dolt reports as conflicted.
func conflictedTables(ctx context.Context) ([]storage.Conflict, error) {
	conflicts, err := store.GetConflicts(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Field < conflicts[j].Field })
	return conflicts, nil
}

// conflictInspector returns the store's conflict surface. store is ALWAYS
// wrapped (telemetry, and the hook-firing decorator whenever hooks are on),
// and a decorator that embeds the DoltStorage interface promotes only that
// interface's methods — so asserting ConflictInspector on the wrapper is
// always false and every conflict would read as "no conflicts". UnwrapStore
// is the codebase's answer for optional interfaces (hook_decorator.go:67).
func conflictInspector() (storage.ConflictInspector, bool) {
	inspector, ok := storage.UnwrapStore(store).(storage.ConflictInspector)
	return inspector, ok
}

// conflictRows returns the per-field conflicted rows of one table, or an empty
// slice when the backend has no ConflictInspector.
func conflictRows(ctx context.Context, table string) ([]storage.ConflictRow, error) {
	inspector, ok := conflictInspector()
	if !ok {
		return nil, nil
	}
	return inspector.GetConflictRows(ctx, table)
}

// totalConflicts counts every live conflicted row, the gate on committing the
// merge: dolt refuses a commit while any conflict is live.
func totalConflicts(ctx context.Context) (int, error) {
	tables, err := conflictedTables(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, t := range tables {
		total += t.Count
	}
	return total, nil
}

// resolveStrategy reads the strategy from --ours/--theirs/--strategy.
func resolveStrategy() (string, error) {
	picked := ""
	switch {
	case conflictsResolveOurs && conflictsResolveTheirs:
		return "", fmt.Errorf("pass --ours or --theirs, not both")
	case conflictsResolveOurs:
		picked = versioncontrolops.ConflictStrategyOurs
	case conflictsResolveTheirs:
		picked = versioncontrolops.ConflictStrategyTheirs
	}
	if conflictsResolveStrat != "" {
		if picked != "" && picked != conflictsResolveStrat {
			return "", fmt.Errorf("--strategy %s contradicts --%s", conflictsResolveStrat, picked)
		}
		picked = conflictsResolveStrat
	}
	if picked == "" {
		return "", fmt.Errorf("a resolution strategy is required: --ours or --theirs")
	}
	if err := versioncontrolops.ValidateConflictStrategy(picked); err != nil {
		return "", err
	}
	return picked, nil
}

// filterShownFields drops the agreeing fields from JSON output unless the
// caller asked for all of them, so a conflict reads as its handful of
// diverged fields rather than the whole row.
func filterShownFields(rows []storage.ConflictRow, allFields bool) []storage.ConflictRow {
	if allFields {
		return rows
	}
	out := make([]storage.ConflictRow, 0, len(rows))
	for _, r := range rows {
		trimmed := r
		trimmed.Fields = differingFields(r.Fields)
		out = append(out, trimmed)
	}
	return out
}

func differingFields(fields []storage.ConflictFieldValue) []storage.ConflictFieldValue {
	out := make([]storage.ConflictFieldValue, 0, len(fields))
	for _, f := range fields {
		if f.Differs() {
			out = append(out, f)
		}
	}
	return out
}

// printConflictRow renders one conflicted row for humans.
func printConflictRow(r storage.ConflictRow, allFields bool) {
	key := r.Key
	if key == "" {
		key = "(unkeyed row)"
	}
	fmt.Printf("\n%s %s  [%s]\n", ui.RenderAccent(r.Table), key, conflictKind(r))
	fields := r.Fields
	if !allFields {
		fields = differingFields(fields)
	}
	if len(fields) == 0 {
		fmt.Println("  (no differing fields; the conflict is structural)")
		return
	}
	for _, f := range fields {
		fmt.Printf("  %s\n", f.Name)
		fmt.Printf("    base:   %s\n", conflictValue(f.Base))
		fmt.Printf("    ours:   %s\n", conflictValue(f.Ours))
		fmt.Printf("    theirs: %s\n", conflictValue(f.Theirs))
	}
	fmt.Println()
}

// conflictKind names the conflict class in the vocabulary an operator needs to
// choose a strategy, since ours/theirs mean different things per class.
func conflictKind(r storage.ConflictRow) string {
	switch {
	case !r.OurExists && !r.TheirExists:
		return "both sides deleted"
	case !r.OurExists:
		return "we deleted / they modified"
	case !r.TheirExists:
		return "we modified / they deleted"
	case !r.BaseExists:
		return "both sides added"
	default:
		return "both sides modified"
	}
}

// conflictValue renders a field value, distinguishing SQL NULL from empty.
func conflictValue(v *string) string {
	if v == nil {
		return "(null)"
	}
	s := strings.ReplaceAll(*v, "\n", "\\n")
	if s == "" {
		return `""`
	}
	return s
}

func init() {
	conflictsShowCmd.Flags().BoolVar(&conflictsShowAllFields, "all-fields", false, "Show every column, not just the fields that diverged")
	conflictsShowCmd.Flags().StringVar(&conflictsShowTable, "table", "", "Restrict to one conflicted table (default: all)")

	conflictsResolveCmd.Flags().BoolVar(&conflictsResolveOurs, "ours", false, "Keep our side")
	conflictsResolveCmd.Flags().BoolVar(&conflictsResolveTheirs, "theirs", false, "Take their side")
	conflictsResolveCmd.Flags().StringVar(&conflictsResolveStrat, "strategy", "", "Resolution strategy: ours|theirs")
	conflictsResolveCmd.Flags().BoolVar(&conflictsResolveAll, "all", false, "Resolve whole tables instead of named issues")
	conflictsResolveCmd.Flags().StringVar(&conflictsResolveTable, "table", "", "Table to resolve (default: issues)")
	conflictsResolveCmd.Flags().BoolVar(&conflictsNoCommit, "no-commit", false, "Resolve without committing the merge")

	conflictsCmd.AddCommand(conflictsListCmd)
	conflictsCmd.AddCommand(conflictsShowCmd)
	conflictsCmd.AddCommand(conflictsResolveCmd)
	rootCmd.AddCommand(conflictsCmd)
}
