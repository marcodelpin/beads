package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/audit"
	"github.com/steveyegge/beads/internal/hooks"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/ui"
	"github.com/steveyegge/beads/internal/workapi"
	"github.com/steveyegge/beads/issueops"
)

// reopenProxiedTarget is one id that resolved, carrying the status it sat at
// before the reopens ran.
type reopenProxiedTarget struct {
	id string
	// status is the prior status, and it feeds the audit sidecar's old_value
	// and nothing else. The role's result is a post-state snapshot and carries
	// no prior status, and a constant "closed" is exactly the value this commit
	// would make wrong, now that a configured done status reopens here.
	status types.Status
}

// runReopenProxiedServer reopens each id through issueops.Lifecycle — the same
// role, reached through the same kind of accessor, that the direct route calls.
// Nothing here decides what a reopen means: the plane resolve, the done-category
// rule, the reopened event, the version-control entry and the whole-attempt
// retry all live behind the call, so the two routes can no longer answer the
// same command differently.
//
// TWO BEHAVIORS CHANGE, both of them toward the direct route.
//
// A CONFIGURED DONE STATUS NOW REOPENS. This route used to compare the current
// status against literal StatusClosed and report "already <status>" for
// anything else, so an issue parked on a custom done status could not be
// reopened on a team server while the same command worked locally. The role
// speaks in terms of the configured done CATEGORY
// (issueops/issueops.go:417-420), and the contract pins that at all three
// backends.
//
// ONE CALL PER ID, so one transaction and one history entry per id, where this
// route used to run every id in one unit of work under a hand-composed
// "bd: reopen a, b" message. The role hands out no transaction handle to hold
// open, and a batch role for a two-flag command is not warranted (Q2,
// recommended and non-blocking), so the per-id entry the direct route has
// always written is what both routes now write.
func runReopenProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
	if len(args) == 0 {
		return HandleErrorRespectJSON("no issue ID provided")
	}
	reason, _ := cmd.Flags().GetString("reason")
	jsonOut, _ := cmd.Flags().GetBool("json")

	targets, hasError, err := reopenProxiedResolve(ctx, args)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	if len(targets) == 0 {
		if hasError {
			return SilentExit()
		}
		return nil
	}

	lifecycle, err := proxiedIssueLifecycle()
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	reopenedIssues := []*types.Issue{}
	for _, target := range targets {
		result, err := lifecycle.Reopen(ctx, issueops.ReopenRequest{
			Actor:   actor,
			IssueID: target.id,
			Reason:  reason,
			// The label the direct route spells, so one reopen reads the same
			// in `bd dolt log` whichever route served it.
			Provenance: "bd: reopen " + target.id,
		})
		if err != nil {
			reportIssueLookupFailure("reopening", target.id, err)
			hasError = true
			continue
		}
		if !result.Changed {
			// Read off the result rather than off the pre-read: the status the
			// reopen left in place is the one the operation saw inside its own
			// transaction, and it is what lets this route reach the
			// already-open line without a short-circuit of its own.
			fmt.Fprintln(os.Stderr, reopenNoOpMessage(target.id, reopenStatusOf(result.Issue, nil)))
			continue
		}

		audit.LogFieldChange(target.id, "status", string(target.status), string(types.StatusOpen), actor, reason)
		if err := fireProxiedReopenHooks(ctx, result.Issue); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", target.id, err)
		}
		if jsonOut {
			if issue := result.Issue; issue != nil {
				// `bd reopen` has never printed dependency records, on either
				// route: the direct route drops them from the operation's own
				// snapshot for exactly this reason.
				issue.Dependencies = nil
				reopenedIssues = append(reopenedIssues, issue)
			}
			continue
		}
		suffix := ""
		if reason != "" {
			suffix = ": " + reason
		}
		fmt.Printf("%s Reopened %s%s\n", ui.RenderAccent("↻"), target.id, suffix)
	}

	if jsonOut && len(reopenedIssues) > 0 {
		_ = outputJSON(reopenedIssues)
	}

	if hasError {
		return SilentExit()
	}
	return nil
}

// reopenProxiedResolve resolves every id in ONE read-only unit of work and
// reports the ones that did not resolve, returning the survivors with the
// status each sat at.
//
// It is the shape `bd close`'s preflight landed with, and it exists for the two
// things the role's result cannot supply: the audit sidecar's old_value, and
// the difference between an absent id and a backend that failed to answer —
// which this route has always drawn, and which reportIssueLookupFailure spells
// the same way here as it does for `bd show`.
//
// It decides NOTHING about the reopen. A resolved id goes to the role whatever
// status it is at, including a status this route used to refuse; the read is a
// transaction earlier than the mutation, and every guard that matters is inside
// the role's own.
func reopenProxiedResolve(ctx context.Context, ids []string) ([]reopenProxiedTarget, bool, error) {
	var targets []reopenProxiedTarget
	failed := false
	_, err := uow.RunTxRead(ctx, uowProvider, func(ctx context.Context, uw uow.UnitOfWork) (struct{}, error) {
		source := workapi.NewUOWDetailSource(uw)
		for _, id := range ids {
			issue, _, err := workapi.GetIssueOrWisp(ctx, source, id)
			if err != nil {
				reportIssueLookupFailure("resolving", id, err)
				failed = true
				continue
			}
			targets = append(targets, reopenProxiedTarget{id: id, status: issue.Status})
		}
		return struct{}{}, nil
	})
	return targets, failed, err
}

func fireProxiedReopenHooks(ctx context.Context, after *types.Issue) error {
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
	return nil
}
