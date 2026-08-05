package storage

import (
	"github.com/steveyegge/beads/issueops"
)

// Sweeper returns the inner store's bulk-clearance surface.
//
// IT RECURSES UNWRAPPED, and this is the one WRITE role that does. The
// read roles recurse because nothing completed; this one completes plenty and
// still has no hook to fire, for two reasons that are both about the hook
// vocabulary rather than about the sweep:
//
//   - there is no on_delete hook. internal/hooks publishes on_create,
//     on_update and on_close, and a sweep is none of them. Firing on_update
//     for a row that no longer exists would hand a user's script an id it
//     cannot read back.
//   - a hook script is handed an ISSUE, and a sweep's result carries counts
//     rather than rows — deliberately, because a sweep of ten thousand wisps
//     that materialized all ten thousand to fire a subprocess each would be a
//     new failure mode rather than a feature.
//
// So the accessor EXISTS on this decorator — declared, never inherited, which
// the reflection test in role_accessor_decorator_test.go asserts — and adds no
// layer. If a delete event ever joins the hook vocabulary, this file is where
// it lands.
func (h *HookFiringStore) Sweeper() (issueops.Sweeper, error) {
	return h.inner.Sweeper()
}
