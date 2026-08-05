package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/issueops"
)

// proxiedQuerier hands back the guarded boolean-query surface for the
// proxied-server provider, through the provider's OWN capability accessor —
// the same two-step proxiedCounter performs, and for the same reason: the
// accessor is where each layer is added.
//
// This file used to hold a second copy of `bd query`: it parsed the expression
// again, evaluated it again, applied the default closed exclusion again, and
// carried its own copy of the limit*3-floor-100 over-fetch. All of that is
// behind the role now, so the proxied route differs from the direct one by
// which accessor it asks.
//
// TWO --offset REFUSALS LEFT WITH IT, in opposite directions. "--offset is not
// supported with OR/predicate queries" is GONE: it existed because an offset
// into a window that had already dropped matches meant nothing, and the window
// is gone, so the role answers that combination correctly. "--offset with a
// display order" is still refused, but by the ROLE and for every caller — the
// order is applied to the rows the query bounded, so paging under a sort would
// sort each page for itself.
func proxiedQuerier() (issueops.Querier, error) {
	if uowProvider == nil {
		return nil, errors.New("proxied-server UOW provider not initialized")
	}
	src, ok := uowProvider.(uow.QuerierSource)
	if !ok {
		return nil, fmt.Errorf("proxied-server provider %T does not offer the boolean-query surface", uowProvider)
	}
	return src.Querier()
}

// runQueryProxiedServer is the proxied route's entry point. It reads the same
// flags the direct route reads and asks the same role, and the only line the
// direct route has that this one does not is the refusal of an --offset it
// cannot serve.
func runQueryProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
	in, err := gatherQueryInput(cmd, args)
	if err != nil {
		return err
	}
	if in.parseOnly {
		return printParsedQuery(in.expression)
	}

	querier, err := openQuerier()
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	return runQuery(ctx, querier, in)
}
