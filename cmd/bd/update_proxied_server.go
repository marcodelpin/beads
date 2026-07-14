package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/hooks"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/fs"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/ui"
)

func runUpdateProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
	if len(args) == 0 {
		return HandleErrorRespectJSON("no issue ID provided")
	}

	in, err := gatherUpdateInput(ctx, cmd)
	if err != nil {
		return err
	}
	if isUpdateInputNoop(in) {
		fmt.Println("No updates specified")
		return nil
	}

	jsonOut, _ := cmd.Flags().GetBool("json")
	var updated []*types.Issue
	var anyUpdated bool
	// claimFailed records a requested-but-lost --claim. In a mixed batch (one
	// claim won, another lost to a different owner) anyUpdated is set by the
	// winner, so the command would otherwise exit 0 and hide the lost claim from
	// exit-code automation. Track it separately and exit non-zero, mirroring the
	// non-proxied path in update.go (beads audit finding #10).
	claimFailed := false

	for _, id := range args {
		issue, ok, claimLost, err := applyUpdateProxiedOne(ctx, id, in)
		if err != nil {
			return err
		}
		if claimLost {
			claimFailed = true
		}
		if !ok {
			continue
		}
		anyUpdated = true
		if jsonOut {
			updated = append(updated, issue)
		} else {
			fmt.Printf("%s Updated issue: %s\n", ui.RenderPass("✓"), formatFeedbackID(issue.ID, issue.Title))
		}
	}

	if jsonOut && len(updated) > 0 {
		_ = outputJSON(updated)
	}
	if !anyUpdated || claimFailed {
		return SilentExit()
	}
	return nil
}

type updateProxiedResult struct {
	before  *types.Issue
	after   *types.Issue
	updated bool
}

// applyUpdateProxiedOne applies one ID's update on the proxied path. The third
// return (claimLost) reports a requested --claim that lost to a different owner
// (already-claimed / not-claimable), so the caller can flip the batch exit code
// even when another ID succeeded — matching the non-proxied path.
//
// uow.RunTxResult redoes the WHOLE read-merge-write in a fresh unit of work
// when Dolt reports a serialization failure — never just the commit. The
// server already rolled the conflicted transaction back, so re-committing the
// same session (the old uow.CommitWithRetries idiom) could only ever produce
// "nothing to commit", which was swallowed: bd printed "✓ Updated" and exited
// 0 for a write that never landed. Redoing the attempt re-reads the winner's
// committed row, so merge operations (metadata edits, note appends) resolve
// against authoritative state instead of erasing it; exhausted retries surface
// as a loud per-issue error and a non-zero exit.
func applyUpdateProxiedOne(ctx context.Context, id string, in *updateInput) (*types.Issue, bool, bool, error) {
	if uowProvider == nil {
		return nil, false, false, HandleError("proxied-server UOW provider not initialized")
	}

	var claimLost bool
	res, err := uow.RunTxResult(ctx, uowProvider, func(ctx context.Context, uw uow.UnitOfWork) (updateProxiedResult, string, error) {
		issueUC := uw.IssueUseCase()
		current, err := issueUC.GetIssue(ctx, id)
		if err != nil || current == nil {
			wispCurrent, wispErr := issueUC.GetWisp(ctx, id)
			if wispErr == nil && wispCurrent != nil {
				current = wispCurrent
			} else if err != nil {
				return updateProxiedResult{}, "", fmt.Errorf("resolving %s: %w", id, err)
			} else {
				return updateProxiedResult{}, "", fmt.Errorf("issue %s not found", id)
			}
		}
		if err := validateIssueUpdatable(id, current); err != nil {
			return updateProxiedResult{}, "", err
		}

		spec := buildUpdateSpecForIssue(current, in)

		updated, err := issueUC.ApplyUpdate(ctx, id, spec, actor)
		if err != nil {
			if errors.Is(err, storage.ErrAlreadyClaimed) || errors.Is(err, storage.ErrNotClaimable) {
				claimLost = in.claim
				return updateProxiedResult{}, "", err
			}
			return updateProxiedResult{}, "", fmt.Errorf("updating %s: %w", id, err)
		}

		return updateProxiedResult{before: current, after: updated, updated: true}, fmt.Sprintf("bd: update %s", id), nil
	})
	if err != nil {
		if claimLost {
			fmt.Fprintf(os.Stderr, "Error claiming %s: %v\n", id, err)
			return nil, false, true, nil
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return nil, false, false, nil
	}

	if res.updated {
		if err := fireProxiedUpdateHooks(ctx, res.before, res.after); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", id, err)
		}
	}
	return res.after, res.updated, false, nil
}

func fireProxiedUpdateHooks(ctx context.Context, before, after *types.Issue) error {
	if after == nil {
		return nil
	}
	runner, err := proxiedHookRunner(ctx)
	if err != nil {
		return fmt.Errorf("hook runner: %w", err)
	}
	if runner == nil {
		return nil
	}
	if err := runner.RunSync(hooks.EventUpdate, after); err != nil {
		return fmt.Errorf("on_update hook: %w", err)
	}
	if before != nil &&
		before.Status != types.StatusClosed &&
		after.Status == types.StatusClosed {
		if err := runner.RunSync(hooks.EventClose, after); err != nil {
			return fmt.Errorf("on_close hook: %w", err)
		}
	}
	return nil
}

func proxiedHookRunner(ctx context.Context) (*hooks.Runner, error) {
	if hookRunner != nil {
		return hookRunner, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getwd: %w", err)
	}
	fsProvider := fs.NewFileSystemProvider(cwd, newBeadsDirTemplates(), newFileSystemAdapters())
	resolution := fsProvider.BeadsDirFSUseCase().ResolveBeadsDir(ctx)
	if resolution.BeadsDir == "" {
		return nil, nil
	}
	return hooks.NewRunner(filepath.Join(resolution.BeadsDir, "hooks")), nil
}

// buildUpdateSpecForIssue translates gathered CLI input into a domain
// UpdateSpec. It never pre-merges row state: merge-shaped edits are passed as
// operation keys and resolved by the repository inside the mutation
// transaction.
func buildUpdateSpecForIssue(current *types.Issue, in *updateInput) domain.UpdateSpec {
	fields := make(map[string]any, len(in.fields))
	for k, v := range in.fields {
		fields[k] = v
	}

	if in.clearDeferStatus && current.Status == types.StatusDeferred {
		fields["status"] = string(types.StatusOpen)
	}
	// Metadata edits and note appends pass through as merge OPERATIONS: the
	// repository resolves them against the row re-read inside the mutation
	// transaction (issueops.ResolveMergeOps via the domain/db Update path).
	// Merging here against `current` — a read from this unit of work's MVCC
	// snapshot — silently erased keys a concurrent writer committed after our
	// snapshot was taken: both processes exited 0, one write vanished.
	if in.hasAppendNotes {
		fields[issueops.OpAppendNotes] = in.appendNotes
	}
	if len(in.mergeMetadataIn) > 0 {
		fields[issueops.OpMergeMetadata] = in.mergeMetadataIn
	}
	if len(in.setMetadata) > 0 {
		fields[issueops.OpSetMetadata] = in.setMetadata
	}
	if len(in.unsetMetadata) > 0 {
		fields[issueops.OpUnsetMetadata] = in.unsetMetadata
	}

	return domain.UpdateSpec{
		Fields:       fields,
		Claim:        in.claim,
		AddLabels:    in.addLabels,
		RemoveLabels: in.removeLabels,
		SetLabels:    in.setLabels,
		Reparent:     in.reparent,
	}
}
