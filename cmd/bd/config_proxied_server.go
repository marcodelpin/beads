package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/issueops"
)

// proxiedWorkspaceConfig hands back the guarded workspace-settings surface for
// the proxied-server provider, through the provider's OWN capability accessor —
// the same two-step proxiedCounter performs, and for the same reason: the
// accessor is where each layer is added.
//
// This file used to hold a second copy of the four single-key `bd config`
// verbs: each one opened its own unit of work, called the config use case and
// then printed the same lines the direct route prints a hundred lines away.
// One of those copies had drifted in a way no reader could see. The proxied
// `set` validated `status.custom` and stored it, but nothing on this route
// re-synchronized the custom_statuses and custom_types tables that reads
// consult FIRST — so a proxied `bd config set types.custom` reported success
// and never took effect. That duplication is gone: the route now differs by
// which accessor answers.
func proxiedWorkspaceConfig() (issueops.WorkspaceConfig, error) {
	if uowProvider == nil {
		return nil, errors.New("proxied-server UOW provider not initialized")
	}
	src, ok := uowProvider.(uow.WorkspaceConfigSource)
	if !ok {
		return nil, fmt.Errorf("proxied-server provider %T does not offer the workspace-settings surface", uowProvider)
	}
	return src.WorkspaceConfig()
}

// runConfigSetManyProxiedServer writes a whole batch of settings in ONE unit of
// work, which is the fifth verb and the one that stays off the role.
//
// The batch is the point of `bd config set-many` — one Dolt commit for N keys
// rather than N, which is what makes it usable in CI — and
// TestProxiedServerConfigSetMany pins it. issueops.WorkspaceConfig writes one
// setting per call and commits each, as its contract says, so routing this
// through it would turn a three-key batch into three commits. A batched write
// is a different capability and would get a role of its own, exactly as
// `bd create --file` stays off issueops.Lifecycle.Create.
//
// It does NOT re-implement the role's refusals: cmd/bd/config.go applies them
// to every pair, before any of them is written.
func runConfigSetManyProxiedServer(ctx context.Context, keys, values []string) error {
	if len(keys) == 0 {
		return nil
	}
	if uowProvider == nil {
		return HandleErrorRespectJSON("proxied-server UOW provider not initialized")
	}

	return uow.RunTx(ctx, uowProvider, func(ctx context.Context, uw uow.UnitOfWork) (string, error) {
		cfgUC := uw.ConfigUseCase()
		for i, k := range keys {
			if err := cfgUC.SetConfig(ctx, k, values[i]); err != nil {
				return "", fmt.Errorf("setting config %s: %w", k, err)
			}
		}
		return fmt.Sprintf("bd: config set-many (%d keys)", len(keys)), nil
	})
}
