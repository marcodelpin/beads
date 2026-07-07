package main

import (
	"context"
	"fmt"
	"os"

	"github.com/steveyegge/beads/internal/config"
)

func runDoltRemoteRemoveProxied(ctx context.Context, name string) {
	if uowProvider == nil {
		fmt.Fprintf(os.Stderr, "Error: proxied-server UOW provider not initialized\n")
		os.Exit(1)
	}
	uw, err := uowProvider.NewUOW(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening unit of work: %v\n", err)
		os.Exit(1)
	}
	defer uw.Close(ctx)

	if err := uw.DoltRemoteUseCase().DeleteRemote(ctx, name); err != nil {
		if jsonOutput {
			_ = outputJSONError(err, "remote_remove_failed")
		} else {
			fmt.Fprintf(os.Stderr, "Error removing remote: %v\n", err)
		}
		os.Exit(1)
	}

	if err := uw.Commit(ctx, fmt.Sprintf("bd: remove remote %s", name)); err != nil && !isDoltNothingToCommit(err) {
		fmt.Fprintf(os.Stderr, "Error committing remote removal: %v\n", err)
		os.Exit(1)
	}

	if name == "origin" {
		if current := config.GetYamlConfig("sync.remote"); current != "" {
			if err := config.UnsetYamlConfig("sync.remote"); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to clear sync.remote from config.yaml: %v\n", err)
			}
			if isGitRepo() {
				commitBeadsConfig("bd: clear sync.remote")
			}
		}
	}

	if jsonOutput {
		if err := outputJSON(map[string]interface{}{
			"name":    name,
			"removed": true,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
	} else {
		fmt.Printf("Removed remote %q\n", name)
	}
}
