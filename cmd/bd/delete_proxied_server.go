package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/ui"
)

type deleteInput struct {
	ids        []string
	cascade    bool
	force      bool
	dryRun     bool
	jsonOutput bool
	quiet      bool
}

func gatherDeleteInput(cmd *cobra.Command, args []string) (*deleteInput, error) {
	in := &deleteInput{}
	in.ids = append(in.ids, args...)

	if fromFile, _ := cmd.Flags().GetString("from-file"); fromFile != "" {
		ids, err := readIssueIDsFromFile(fromFile)
		if err != nil {
			return nil, fmt.Errorf("reading file: %w", err)
		}
		in.ids = append(in.ids, ids...)
	}
	in.ids = uniqueStrings(in.ids)

	in.cascade, _ = cmd.Flags().GetBool("cascade")
	in.force, _ = cmd.Flags().GetBool("force")
	in.dryRun, _ = cmd.Flags().GetBool("dry-run")
	in.jsonOutput = jsonOutput
	in.quiet = isQuiet()
	return in, nil
}

func runDeleteProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
	in, err := gatherDeleteInput(cmd, args)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	if len(in.ids) == 0 {
		_ = cmd.Usage()
		return HandleError("no issue IDs provided")
	}

	if uowProvider == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}

	if in.dryRun || !in.force {
		return runDeleteProxiedPreviewTx(ctx, in)
	}

	res, err := uow.RunTxResult(ctx, uowProvider, func(ctx context.Context, uw uow.UnitOfWork) (domain.DeleteIssuesResult, string, error) {
		issueUC := uw.IssueUseCase()

		preview, err := issueUC.PreviewDelete(ctx, in.ids)
		if err != nil {
			return domain.DeleteIssuesResult{}, "", fmt.Errorf("preview: %w", err)
		}
		if len(preview.NotFound) > 0 {
			return domain.DeleteIssuesResult{}, "", fmt.Errorf("issues not found: %s", strings.Join(preview.NotFound, ", "))
		}

		res, err := issueUC.DeleteIssues(ctx, domain.DeleteIssuesParams{
			IDs:                  in.ids,
			Cascade:              in.cascade,
			Force:                true, // this path only runs under --force
			EnforceCascadePolicy: true,
			UpdateTextReferences: true,
		}, actor)
		if err != nil {
			return domain.DeleteIssuesResult{}, "", fmt.Errorf("delete: %w", err)
		}
		if res.DeletedCount == 0 {
			return domain.DeleteIssuesResult{}, "", fmt.Errorf("issues not found: %s", strings.Join(in.ids, ", "))
		}

		return res, fmt.Sprintf("bd: delete %d issue(s)", res.DeletedCount), nil
	})
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	renderDeleteProxiedResult(in, res)
	return nil
}

type deletePreviewResult struct {
	preview domain.DeletePreview
	res     domain.DeleteIssuesResult
	// blocked carries the embedded-parity refusal (external dependents, no
	// --cascade / --force) so the preview can render it the way classic does —
	// preview first, then the refusal explaining how to proceed.
	blocked *domain.DeleteBlockedError
}

func runDeleteProxiedPreviewTx(ctx context.Context, in *deleteInput) error {
	result, err := uow.RunTxRead(ctx, uowProvider, func(ctx context.Context, uw uow.UnitOfWork) (deletePreviewResult, error) {
		issueUC := uw.IssueUseCase()

		preview, err := issueUC.PreviewDelete(ctx, in.ids)
		if err != nil {
			return deletePreviewResult{}, fmt.Errorf("preview: %w", err)
		}
		if len(preview.NotFound) > 0 {
			return deletePreviewResult{}, fmt.Errorf("issues not found: %s", strings.Join(preview.NotFound, ", "))
		}

		res, err := issueUC.DeleteIssues(ctx, domain.DeleteIssuesParams{
			IDs:                  in.ids,
			Cascade:              in.cascade,
			Force:                in.force,
			EnforceCascadePolicy: true,
			DryRun:               true,
		}, actor)
		if err != nil {
			var blocked *domain.DeleteBlockedError
			if errors.As(err, &blocked) {
				return deletePreviewResult{preview: preview, res: res, blocked: blocked}, nil
			}
			return deletePreviewResult{}, fmt.Errorf("preview counts: %w", err)
		}

		return deletePreviewResult{preview: preview, res: res}, nil
	})
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	if err := outputDeleteProxiedPreview(in, result); err != nil {
		return err
	}
	if result.blocked != nil {
		// Classic parity: render the preview, then fail with the refusal
		// (which itself says how to proceed). In JSON mode the preview payload
		// above already carries the refusal in its "error" key — emitting
		// jsonStdoutError too would put two JSON docs on stdout.
		if in.jsonOutput {
			return &exitError{Code: 1}
		}
		return HandleErrorRespectJSON("%v", result.blocked)
	}
	return nil
}

// outputDeleteProxiedPreview is the proxied-server preview output boundary.
// JSON takes precedence over quiet, but neither mode may serialize issue payloads.
func outputDeleteProxiedPreview(in *deleteInput, result deletePreviewResult) error {
	if in.jsonOutput {
		payload := map[string]any{
			"would_delete":         result.res.DeletedCount,
			"dependencies_removed": result.res.DependenciesCount,
			"labels_removed":       result.res.LabelsCount,
			"events_removed":       result.res.EventsCount,
			"ids":                  in.ids,
			"not_found":            result.preview.NotFound,
			"connected":            sortedKeys(result.preview.ConnectedIssues),
			"dry_run":              in.dryRun,
			"cascade":              in.cascade,
			"would_orphan":         len(result.res.OrphanedIssues),
		}
		if result.blocked != nil {
			payload["error"] = result.blocked.Error()
		}
		return outputJSON(payload)
	}
	if in.quiet {
		return nil
	}
	renderDeletePreview(in, result.preview, result.res, result.blocked)
	return nil
}

func renderDeletePreview(in *deleteInput, preview domain.DeletePreview, res domain.DeleteIssuesResult, blocked *domain.DeleteBlockedError) {
	fmt.Printf("\n%s\n", ui.RenderFail("⚠️  DELETE PREVIEW"))
	fmt.Printf("\nIssues to delete (%d):\n", len(in.ids))
	for _, id := range in.ids {
		title := ""
		if iss, ok := preview.Issues[id]; ok && iss != nil {
			title = iss.Title
		}
		fmt.Printf("  %s: %s\n", id, title)
	}
	if in.cascade {
		fmt.Printf("\n%s Cascade mode enabled - will also delete all dependent issues\n", ui.RenderWarn("⚠"))
	}
	if blocked != nil {
		fmt.Printf("\n%s\n", ui.RenderFail(blocked.Error()))
		return
	}
	fmt.Printf("\nWould remove:\n")
	fmt.Printf("  %d issue(s) total\n", res.DeletedCount)
	fmt.Printf("  %d dependency link(s)\n", res.DependenciesCount)
	fmt.Printf("  %d label(s)\n", res.LabelsCount)
	fmt.Printf("  %d event(s)\n", res.EventsCount)
	if len(res.OrphanedIssues) > 0 {
		fmt.Printf("  Would orphan %d issue(s): %s\n",
			len(res.OrphanedIssues), strings.Join(res.OrphanedIssues, ", "))
	}

	if len(preview.ConnectedIssues) > 0 {
		fmt.Printf("\nConnected issues (text references may be rewritten):\n")
		for _, id := range sortedKeys(preview.ConnectedIssues) {
			iss := preview.ConnectedIssues[id]
			title := ""
			if iss != nil {
				title = iss.Title
			}
			fmt.Printf("  %s: %s\n", id, title)
		}
	}

	if in.dryRun {
		fmt.Printf("\n(Dry-run mode - no changes made)\n")
		return
	}
	fmt.Printf("\n%s\n", ui.RenderWarn("This operation cannot be undone!"))
	proceed := "bd delete " + strings.Join(in.ids, " ") + " --force"
	if in.cascade {
		proceed = "bd delete " + strings.Join(in.ids, " ") + " --cascade --force"
	}
	fmt.Printf("To proceed, run: %s\n", ui.RenderWarn(proceed))
}

func renderDeleteProxiedResult(in *deleteInput, res domain.DeleteIssuesResult) {
	if in.jsonOutput {
		_ = outputJSON(map[string]any{
			"deleted":              in.ids,
			"deleted_count":        res.DeletedCount,
			"dependencies_removed": res.DependenciesCount,
			"labels_removed":       res.LabelsCount,
			"events_removed":       res.EventsCount,
			"references_updated":   res.ReferencesUpdated,
			"orphaned_issues":      res.OrphanedIssues,
		})
		return
	}
	fmt.Printf("%s Deleted %d issue(s)\n", ui.RenderPass("✓"), res.DeletedCount)
	fmt.Printf("  Removed %d dependency link(s)\n", res.DependenciesCount)
	fmt.Printf("  Removed %d label(s)\n", res.LabelsCount)
	fmt.Printf("  Removed %d event(s)\n", res.EventsCount)
	fmt.Printf("  Updated text references in %d issue(s)\n", res.ReferencesUpdated)
	if len(res.OrphanedIssues) > 0 {
		fmt.Printf("  %s Orphaned %d issue(s): %s\n",
			ui.RenderWarn("⚠"), len(res.OrphanedIssues), strings.Join(res.OrphanedIssues, ", "))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
