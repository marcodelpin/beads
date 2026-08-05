package storage

import (
	"github.com/steveyegge/beads/issueops"
)

// VersionReconciler returns the inner store's version-marker surface.
//
// It recurses, like every other accessor on this decorator, rather than being
// absent: the accessor set is uniform across the chain, so a caller never has
// to know which decorators a store is wearing. What it does NOT do is wrap the
// result, and here that is the same statement hook_workspace_config.go makes
// about a role that WRITES.
//
// This decorator's vocabulary is on_create / on_update / on_close, and every
// one of those hands a hook script an ISSUE. Recording which binary opened the
// workspace has no issue to name and no bead to describe, so there is nothing
// this layer could fire that a hook script could read. There is a second reason
// here that the settings role does not have: this one runs from PersistentPreRun
// on every startup, so a hook fired here would run a user's script before every
// command — including the ones that go on to fail on their own arguments.
func (h *HookFiringStore) VersionReconciler() (issueops.VersionReconciler, error) {
	return h.inner.VersionReconciler()
}
