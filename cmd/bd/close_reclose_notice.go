package main

import (
	"fmt"
	"os"

	"github.com/steveyegge/beads/internal/types"
)

// noteAlreadyClosed reports a no-op re-close truthfully (GH#4816, bda-4myc).
// The storage layer's idempotent close (GH#4025) keeps the first close_reason
// and changes nothing, so the CLI says so on stderr - in human and --json mode
// alike - instead of printing a fresh "Closed" success line. Every close route
// calls it when CloseResult.Changed is false: the plain batch route in close.go
// and the --if-revision route in close_if_revision.go.
func noteAlreadyClosed(id string, closedIssue *types.Issue) {
	kept := ""
	if closedIssue != nil {
		kept = closedIssue.CloseReason
	}
	fmt.Fprintf(os.Stderr, "%s already closed (close_reason kept: %q)\n", id, kept)
}
