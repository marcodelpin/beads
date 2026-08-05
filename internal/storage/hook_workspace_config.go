package storage

import (
	"github.com/steveyegge/beads/issueops"
)

// WorkspaceConfig returns the inner store's workspace-settings surface.
//
// It recurses, like every other accessor on this decorator, rather than being
// absent: the accessor set is uniform across the chain, so a caller never has
// to know which decorators a store is wearing. What it does NOT do is wrap the
// result, and here that is a statement about a WRITE role rather than the
// read-role statement hook_counter.go makes.
//
// This decorator's vocabulary is on_create / on_update / on_close, and every
// one of those hands the hook script an ISSUE. A settings write has no issue to
// name — it changes the workspace, not a bead — so there is nothing this layer
// could fire that a hook script could read. The legacy config path
// (HookFiringStore inherits SetConfig and DeleteConfig unchanged) fires nothing
// either, so wrapping here would not restore a hook that stopped firing; it
// would invent one.
func (h *HookFiringStore) WorkspaceConfig() (issueops.WorkspaceConfig, error) {
	return h.inner.WorkspaceConfig()
}
