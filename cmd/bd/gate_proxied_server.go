package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/audit"
	"github.com/steveyegge/beads/internal/metrics"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
)

// proxiedGateClose captures a resolved gate's before/after state so on_update
// and on_close hooks can fire once, after the transaction commits.
type proxiedGateClose struct {
	before *types.Issue
	after  *types.Issue
}

// gateCheckApply is the outcome of the write transaction, built fresh on each
// RunTx attempt so a serialization retry never doubles it.
type gateCheckApply struct {
	closed    []proxiedGateClose
	closeErrs map[string]error
	awaitErrs map[string]error
}

// runGateCheckProxiedServer is the proxied-server dual of the embedded gate
// check flow. It evaluates gates outside the transaction (gate evaluation shells
// out to `gh` and must not run under a retryable tx), then applies await-id
// updates and gate closes inside a single uow.RunTx, and fires hooks and renders
// output after the commit.
func runGateCheckProxiedServer(cmd *cobra.Command, ctx context.Context) error {
	CheckReadonly("gate check")

	evt := metrics.NewCommandEvent("gate-check")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	gateTypeFilter, _ := cmd.Flags().GetString("type")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	escalateFlag, _ := cmd.Flags().GetBool("escalate")
	limit, _ := cmd.Flags().GetInt("limit")

	if uowProvider == nil {
		return HandleErrorRespectJSON("proxied-server UOW provider not initialized")
	}

	gateType := types.IssueType("gate")
	filter := types.IssueFilter{
		IssueType:     &gateType,
		ExcludeStatus: []types.Status{types.StatusClosed},
		Limit:         limit,
	}

	// Read + evaluate under a read-only unit of work. A gh:run gate that names a
	// workflow hint records its discovered run id here for a deferred write; the
	// callback never writes so evaluation stays side-effect free (dry runs pass
	// nil and skip discovery persistence entirely).
	discovered := map[string]string{}
	var persistAwaitID func(gateID, runID string) error
	if !dryRun {
		persistAwaitID = func(gateID, runID string) error {
			discovered[gateID] = runID
			return nil
		}
	}

	readUW, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return err
	}
	page, err := readUW.IssueUseCase().SearchIssues(ctx, "", filter)
	if err != nil {
		readUW.Close(ctx)
		return HandleErrorRespectJSON("%v", err)
	}
	filteredGates := filterCheckableGates(page.Items, gateTypeFilter)
	if len(filteredGates) == 0 {
		readUW.Close(ctx)
		printNoOpenGates(gateTypeFilter)
		return nil
	}
	results := evaluateGates(ctx, filteredGates, time.Now(), readUW.IssueUseCase(), persistAwaitID)
	readUW.Close(ctx)

	// Dry run performs no writes; render straight from the evaluation.
	if dryRun {
		resolved, escalated, errCount := applyGateCheckResults(results, true, escalateFlag, nil)
		return printGateCheckSummary(len(results), resolved, escalated, errCount, dryRun)
	}

	applied, err := uow.RunTxResult(ctx, uowProvider, func(ctx context.Context, uw uow.UnitOfWork) (gateCheckApply, string, error) {
		out := gateCheckApply{
			closeErrs: map[string]error{},
			awaitErrs: map[string]error{},
		}

		for gateID, runID := range discovered {
			if err := uw.IssueUseCase().UpdateIssue(ctx, gateID, map[string]any{"await_id": runID}, actor); err != nil {
				out.awaitErrs[gateID] = fmt.Errorf("failed to update gate with discovered run ID: %w", err)
			}
		}

		for _, r := range results {
			if r.err != nil || !r.resolved {
				continue
			}
			if _, awaitFailed := out.awaitErrs[r.gate.ID]; awaitFailed {
				continue
			}
			before, _ := uw.IssueUseCase().GetIssue(ctx, r.gate.ID)
			res, closeErr := uw.IssueUseCase().CloseIssue(ctx, r.gate.ID, domain.CloseIssueParams{Reason: r.reason}, actor)
			if closeErr != nil {
				out.closeErrs[r.gate.ID] = closeErr
				continue
			}
			audit.LogFieldChange(r.gate.ID, "status", string(r.gate.Status), "closed", actor, r.reason)
			out.closed = append(out.closed, proxiedGateClose{before: before, after: res.Issue})
		}

		return out, "bd: gate check", nil
	})
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	// Fire hooks and mark the command as a writer only after the commit lands.
	for _, c := range applied.closed {
		if err := fireProxiedCloseHooks(ctx, c.before, c.after); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", c.after.ID, err)
		}
	}
	if len(applied.closed) > 0 || len(discovered) > len(applied.awaitErrs) {
		commandDidWrite.Store(true)
	}

	// Overlay transaction-time await-id write failures onto evaluation so they
	// render as check errors, matching the embedded flow where a persistence
	// failure supersedes the gate's resolution.
	for i := range results {
		if awaitErr, failed := applied.awaitErrs[results[i].gate.ID]; failed {
			results[i].resolved = false
			results[i].escalated = false
			results[i].err = awaitErr
		}
	}

	resolved, escalated, errCount := applyGateCheckResults(results, false, escalateFlag,
		func(gate *types.Issue, reason string) error {
			return applied.closeErrs[gate.ID]
		})
	return printGateCheckSummary(len(results), resolved, escalated, errCount, dryRun)
}

func runGateListProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
	allFlag, _ := cmd.Flags().GetBool("all")
	limit, _ := cmd.Flags().GetInt("limit")

	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return err
	}
	defer uw.Close(ctx)

	if len(args) == 1 {
		target, isWisp, err := proxiedGetIssueOrWisp(ctx, uw, args[0])
		if err != nil || target == nil {
			return HandleErrorRespectJSON("issue not found: %s", args[0])
		}
		var metas []*types.IssueWithDependencyMetadata
		if isWisp {
			metas, err = uw.DependencyUseCase().ListWispWithIssueMetadata(ctx, target.ID, domain.DepListFilter{Direction: domain.DepDirectionOut})
		} else {
			metas, err = uw.DependencyUseCase().ListWithIssueMetadata(ctx, target.ID, domain.DepListFilter{Direction: domain.DepDirectionOut})
		}
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		deps := make([]*types.Issue, 0, len(metas))
		for _, m := range metas {
			if m != nil {
				deps = append(deps, &m.Issue)
			}
		}
		gates := filterIssueGates(deps, allFlag, limit)
		if jsonOutput {
			return outputJSON(gates)
		}
		displayGates(gates, allFlag)
		return nil
	}

	gateType := types.IssueType("gate")
	filter := types.IssueFilter{
		IssueType: &gateType,
		Limit:     limit,
	}
	if !allFlag {
		filter.ExcludeStatus = []types.Status{types.StatusClosed}
	}
	page, err := uw.IssueUseCase().SearchIssues(ctx, "", filter)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	if jsonOutput {
		return outputJSON(page.Items)
	}
	displayGates(page.Items, allFlag)
	return nil
}
