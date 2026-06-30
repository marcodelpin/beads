package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads"
	"github.com/steveyegge/beads/internal/spool"
)

// spoolCmd is the root of the "bd spool" subcommand tree.
var spoolCmd = &cobra.Command{
	Use:     "spool",
	GroupID: "maint",
	Short:   "Manage the offline write spool",
	Long: `Manage the offline write-spool (.beads/spool/).

The spool buffers bd write commands (create/update/note/close) when Dolt is
temporarily unreachable. Entries are replayed automatically at the start of
the next bd command (MaybeDrain), or you can trigger a drain manually.

Subcommands:
  status   Show queue depth, oldest entry, and disk usage
  drain    Force-drain the spool now (replay all pending entries)
  clear    Wipe the spool (requires --confirm)`,
}

// spoolStatusCmd prints spool diagnostics without modifying state.
var spoolStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show spool queue depth, oldest entry timestamp, and disk usage",
	RunE: func(cmd *cobra.Command, args []string) error {
		sp, err := openSpool()
		if err != nil {
			return err
		}

		count, err := sp.QueueLineCount()
		if err != nil {
			return fmt.Errorf("count queue: %w", err)
		}
		bytes, err := sp.QueueDiskBytes()
		if err != nil {
			return fmt.Errorf("stat queue: %w", err)
		}
		deadCount, err := sp.DeadLetterCount()
		if err != nil {
			return fmt.Errorf("count dead-letter: %w", err)
		}
		inflightAge, err := sp.InflightOldestAge()
		if err != nil {
			return fmt.Errorf("inflight age: %w", err)
		}

		cursor, err := sp.LoadCursor()
		if err != nil {
			return fmt.Errorf("load cursor: %w", err)
		}

		if jsonOutput {
			fmt.Printf(`{"queue_entries":%d,"queue_bytes":%d,"dead_letter_entries":%d,"inflight_oldest_age_sec":%.0f,"last_drain_ts":%q}`+"\n",
				count, bytes, deadCount, inflightAge, cursor.LastDrainTS)
			return nil
		}

		fmt.Printf("Spool directory: %s\n", sp.Dir)
		fmt.Printf("  Pending entries:  %d\n", count)
		fmt.Printf("  Queue size:       %d bytes\n", bytes)
		fmt.Printf("  Dead-letter:      %d entries\n", deadCount)
		if inflightAge > 0 {
			fmt.Printf("  Inflight age:     %.0f seconds (drain may be in progress)\n", inflightAge)
		}
		if cursor.LastDrainTS != "" {
			fmt.Printf("  Last drain:       %s\n", cursor.LastDrainTS)
		} else {
			fmt.Printf("  Last drain:       (never)\n")
		}
		return nil
	},
}

// spoolDrainCmd replays all pending spool entries synchronously.
var spoolDrainCmd = &cobra.Command{
	Use:   "drain",
	Short: "Force-drain the spool now (replay all pending entries into Dolt)",
	RunE: func(cmd *cobra.Command, args []string) error {
		sp, err := openSpool()
		if err != nil {
			return err
		}

		ctx := rootCtx
		dispatch := spoolDispatch(ctx)

		result, err := spool.Drain(ctx, sp, dispatch)
		if err != nil {
			return fmt.Errorf("drain failed: %w", err)
		}

		if jsonOutput {
			fmt.Printf(`{"drained":%d,"dead":%d}`+"\n", result.Drained, result.Dead)
			return nil
		}
		fmt.Printf("Drain complete: %d replayed, %d dead-lettered\n", result.Drained, result.Dead)
		return nil
	},
}

var spoolClearConfirm bool

// spoolClearCmd wipes the spool queue and inflight files.
var spoolClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Wipe the spool (requires --confirm)",
	Long: `Clear all pending spool entries. This permanently discards any queued writes
that have not yet been replayed into Dolt. Use with caution.

You must pass --confirm to proceed.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !spoolClearConfirm {
			return fmt.Errorf("refusing to clear spool without --confirm (this permanently discards pending writes)")
		}

		sp, err := openSpool()
		if err != nil {
			return err
		}

		// Remove queue.jsonl, inflight.jsonl, cursor.json.
		// Leave acked/ and dead-letter.jsonl (audit trail).
		var removed []string
		for _, path := range []string{sp.QueueFile(), sp.InflightFile(), sp.CursorFile()} {
			if rerr := os.Remove(path); rerr == nil {
				removed = append(removed, filepath.Base(path))
			} else if !os.IsNotExist(rerr) {
				return fmt.Errorf("remove %s: %w", filepath.Base(path), rerr)
			}
		}

		if jsonOutput {
			fmt.Printf(`{"removed":%d}`+"\n", len(removed))
			return nil
		}
		if len(removed) == 0 {
			fmt.Println("Spool already empty.")
		} else {
			fmt.Printf("Cleared: %v\n", removed)
		}
		return nil
	},
}

// openSpool resolves the spool directory from the current .beads dir and
// returns a *spool.Spool. Returns an error if no .beads directory is found.
func openSpool() (*spool.Spool, error) {
	var beadsDir string

	// Prefer the already-resolved dbPath (set by PersistentPreRun).
	if dbPath != "" {
		beadsDir = filepath.Dir(dbPath)
	} else {
		beadsDir = beads.FindBeadsDir()
	}
	if beadsDir == "" {
		return nil, fmt.Errorf("no .beads directory found; run 'bd init' first")
	}
	return spool.NewSpool(filepath.Join(beadsDir, "spool")), nil
}

func init() {
	spoolClearCmd.Flags().BoolVar(&spoolClearConfirm, "confirm", false, "Required: confirms you want to permanently discard pending spool entries")

	spoolCmd.AddCommand(spoolStatusCmd)
	spoolCmd.AddCommand(spoolDrainCmd)
	spoolCmd.AddCommand(spoolClearCmd)

	rootCmd.AddCommand(spoolCmd)
}
