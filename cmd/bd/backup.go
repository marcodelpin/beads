package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/config"
)

var backupForce bool

var backupCmd = &cobra.Command{
	Use:     "backup",
	Short:   "Export JSONL backup of all tables to .beads/backup/",
	Long:    "Exports all database tables to JSONL files in .beads/backup/ for off-machine recovery.\nEvents are exported incrementally using a high-water mark.",
	GroupID: "sync",
	RunE: func(cmd *cobra.Command, args []string) error {
		state, err := runBackupExport(rootCtx, backupForce)
		if err != nil {
			return err
		}

		if jsonOutput {
			data, err := json.MarshalIndent(state, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(data))
			return nil
		}

		fmt.Printf("Backup complete: %d issues, %d events, %d comments, %d deps, %d labels, %d config\n",
			state.Counts.Issues, state.Counts.Events, state.Counts.Comments,
			state.Counts.Dependencies, state.Counts.Labels, state.Counts.Config)

		// Optional git push
		if config.GetBool("backup.git-push") {
			if err := gitBackup(rootCtx); err != nil {
				return err
			}
			fmt.Println("Backup committed and pushed to git.")
		}

		return nil
	},
}

var backupStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show last backup status",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := backupDir()
		if err != nil {
			return err
		}

		state, err := loadBackupState(dir)
		if err != nil {
			return err
		}

		if jsonOutput {
			data, err := json.MarshalIndent(state, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(data))
			return nil
		}

		if state.LastDoltCommit == "" {
			fmt.Println("No backup has been performed yet.")
			fmt.Println("Run 'bd backup' to create one, or set backup.enabled: true in config.yaml")
			return nil
		}

		fmt.Printf("Last backup: %s (%s ago)\n",
			state.Timestamp.Format(time.RFC3339),
			time.Since(state.Timestamp).Round(time.Second))
		fmt.Printf("Dolt commit: %s\n", state.LastDoltCommit)
		fmt.Printf("Event high-water mark: %d\n", state.LastEventID)
		fmt.Printf("Counts: %d issues, %d events, %d comments, %d deps, %d labels, %d config\n",
			state.Counts.Issues, state.Counts.Events, state.Counts.Comments,
			state.Counts.Dependencies, state.Counts.Labels, state.Counts.Config)

		// Show config
		enabled := config.GetBool("backup.enabled")
		interval := config.GetDuration("backup.interval")
		gitPush := config.GetBool("backup.git-push")
		fmt.Printf("\nConfig: enabled=%v interval=%s git-push=%v\n", enabled, interval, gitPush)

		return nil
	},
}

func init() {
	backupCmd.Flags().BoolVar(&backupForce, "force", false, "Export even if nothing changed")
	backupCmd.AddCommand(backupStatusCmd)
	rootCmd.AddCommand(backupCmd)
}
