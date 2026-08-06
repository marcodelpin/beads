package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/ui"
	"github.com/steveyegge/beads/issueops"
)

type deleteInput struct {
	ids        []string
	force      bool
	dryRun     bool
	cascade    bool
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

	in.force, _ = cmd.Flags().GetBool("force")
	in.dryRun, _ = cmd.Flags().GetBool("dry-run")
	// --cascade IS SUPPORTED HERE NOW. This route used to refuse the flag
	// outright ("delete always cascades") and hardcode Cascade: true at both of
	// its call sites, so on a team server there was no way to delete an issue
	// without taking its dependents and asking for the safer behavior was an
	// error. Both routes now mean the same three things by the same two flags.
	in.cascade, _ = cmd.Flags().GetBool("cascade")
	in.jsonOutput = jsonOutput
	in.quiet = isQuiet()
	return in, nil
}

// proxiedDeleter hands back the named-row erasure surface for the proxied
// route, through the provider's OWN capability accessor rather than a
// constructor — the accessor is where a decorator would add its layer.
func proxiedDeleter() (issueops.Deleter, error) {
	if uowProvider == nil {
		return nil, fmt.Errorf("proxied-server UOW provider not initialized")
	}
	source, ok := uowProvider.(uow.DeleterSource)
	if !ok {
		return nil, fmt.Errorf("proxied-server provider %T does not offer the Deleter accessor", uowProvider)
	}
	return source.Deleter()
}

// runDeleteProxiedServer is `bd delete` against a team server.
//
// WHAT IT NO LONGER DOES is most of what it used to. It held its own copy of
// the whole flow — a preview unit of work, a committing unit of work, its own
// not-found check, and Cascade hardcoded true at both call sites — and the two
// routes disagreed about what `--force` MEANS on a destructive command. The
// selection, the guard and the deletion are issueops.Deleter now, the same
// library surface the direct route calls; what is left here is this route's
// output, which is not the direct route's and is pinned separately.
func runDeleteProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
	in, err := gatherDeleteInput(cmd, args)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	if len(in.ids) == 0 {
		_ = cmd.Usage()
		return HandleError("no issue IDs provided")
	}

	deleter, err := proxiedDeleter()
	if err != nil {
		return HandleError("%v", err)
	}

	// --force is the confirmation as well as the orphan mode, exactly as on the
	// direct route, so an unconfirmed run asks the role what it WOULD do.
	request := issueops.DeleteRequest{
		Actor:   actor,
		IDs:     in.ids,
		Cascade: in.cascade,
		Force:   in.force,
		DryRun:  in.dryRun || !in.force,
	}
	result, err := deleter.Delete(ctx, request)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	if request.DryRun {
		// The role answered WHAT WOULD HAPPEN; this read answers WHICH ROWS,
		// which is presentation this route has always printed and which no
		// result carries. It runs only after the role accepted the request, so
		// a refusal costs it nothing.
		preview, err := runDeleteProxiedPreviewTx(ctx, in)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		return outputDeleteProxiedPreview(in, deletePreviewResult{preview: preview, res: result})
	}

	commandDidWrite.Store(true)
	renderDeleteProxiedResult(in, result)
	return nil
}

type deletePreviewResult struct {
	preview domain.DeletePreview
	res     issueops.DeleteResult
}

// runDeleteProxiedPreviewTx reads the titles and the neighborhood one preview
// prints. It is a READ unit of work and decides nothing.
func runDeleteProxiedPreviewTx(ctx context.Context, in *deleteInput) (domain.DeletePreview, error) {
	return uow.RunTxRead(ctx, uowProvider, func(ctx context.Context, uw uow.UnitOfWork) (domain.DeletePreview, error) {
		preview, err := uw.IssueUseCase().PreviewDelete(ctx, in.ids)
		if err != nil {
			return domain.DeletePreview{}, fmt.Errorf("preview: %w", err)
		}
		return preview, nil
	})
}

// outputDeleteProxiedPreview is the proxied-server preview output boundary.
// JSON takes precedence over quiet, but neither mode may serialize issue payloads.
func outputDeleteProxiedPreview(in *deleteInput, result deletePreviewResult) error {
	if in.jsonOutput {
		return outputJSON(map[string]any{
			"would_delete":         result.res.Deleted,
			"dependencies_removed": result.res.Dependencies,
			"labels_removed":       result.res.Labels,
			"events_removed":       result.res.Events,
			"ids":                  in.ids,
			"not_found":            result.preview.NotFound,
			"connected":            sortedKeys(result.preview.ConnectedIssues),
			"dry_run":              in.dryRun,
		})
	}
	if in.quiet {
		return nil
	}
	renderDeletePreview(in, result.preview, result.res)
	return nil
}

func renderDeletePreview(in *deleteInput, preview domain.DeletePreview, res issueops.DeleteResult) {
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
		fmt.Printf("\nCascade mode enabled — dependent issues will be removed.\n")
	}
	fmt.Printf("\nWould remove:\n")
	fmt.Printf("  %d issue(s) total\n", res.Deleted)
	fmt.Printf("  %d dependency link(s)\n", res.Dependencies)
	fmt.Printf("  %d label(s)\n", res.Labels)
	fmt.Printf("  %d event(s)\n", res.Events)
	if len(res.Orphaned) > 0 {
		fmt.Printf("  %s Would orphan %d issue(s): %s\n",
			ui.RenderWarn("⚠"), len(res.Orphaned), strings.Join(res.Orphaned, ", "))
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
	if in.cascade {
		fmt.Printf("To proceed with cascade deletion, run: %s\n",
			ui.RenderWarn("bd delete "+strings.Join(in.ids, " ")+" --cascade --force"))
		return
	}
	fmt.Printf("To proceed, run: %s\n",
		ui.RenderWarn("bd delete "+strings.Join(in.ids, " ")+" --force"))
}

func renderDeleteProxiedResult(in *deleteInput, res issueops.DeleteResult) {
	if in.jsonOutput {
		_ = outputJSON(map[string]any{
			"deleted":              in.ids,
			"deleted_count":        res.Deleted,
			"dependencies_removed": res.Dependencies,
			"labels_removed":       res.Labels,
			"events_removed":       res.Events,
			"references_updated":   res.ReferencesUpdated,
			// New on this route, and new because the behavior is: `--force`
			// used to cascade here, so there was never anything to orphan.
			"orphaned_issues": res.Orphaned,
		})
		return
	}
	fmt.Printf("%s Deleted %d issue(s)\n", ui.RenderPass("✓"), res.Deleted)
	fmt.Printf("  Removed %d dependency link(s)\n", res.Dependencies)
	fmt.Printf("  Removed %d label(s)\n", res.Labels)
	fmt.Printf("  Removed %d event(s)\n", res.Events)
	fmt.Printf("  Updated text references in %d issue(s)\n", res.ReferencesUpdated)
	if len(res.Orphaned) > 0 {
		fmt.Printf("  %s Orphaned %d issue(s): %s\n",
			ui.RenderWarn("⚠"), len(res.Orphaned), strings.Join(res.Orphaned, ", "))
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
