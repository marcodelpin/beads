package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// errCloseSpooled marks a close outcome that was QUEUED to the offline write
// spool (GH#4379, internal/spool) instead of applied: the batch hit a
// transient server-unreachable error, so each id was appended for replay on
// the next bd command. The reporting loop matches it with errors.Is and
// prints an honest queued outcome, skipping the success side effects.
//
// Fork-only file: the offline write spool is a fork feature; keeping its
// batch-route seam in its own file keeps future upstream merges of
// close_direct.go down to the two hook lines in closeDirectRun.
var errCloseSpooled = errors.New("close queued for offline replay")

// spoolCloseOutcomes queues every id of a transiently-failed batch for
// offline replay, writing each item's outcome directly. The payload shape is
// the one spoolDispatch has always replayed for "close" (id/reason/actor/
// session), so replay applies the same per-id CloseIssue the pre-batch CLI
// spooled. Returns false only when spooling is impossible (no .beads dir);
// the caller then reports the original error for the whole batch. A per-item
// append failure mirrors writeWithSpool: the original error wins for THAT id
// and the rest of the batch still spools.
func spoolCloseOutcomes(ctx context.Context, batch closeDirectBatch, session string, cause error, outcomes []*issueops.CloseOutcome) bool {
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return false
	}
	s := spoolSingleton(filepath.Join(beadsDir, "spool"))
	for _, item := range batch.items {
		payload := spoolPayload(map[string]interface{}{
			"id":      item.id,
			"reason":  item.reason,
			"actor":   actor,
			"session": session,
		})
		entry, err := s.Append(ctx, "close", payload, false, "bd-cli")
		if err != nil {
			fmt.Fprintf(os.Stderr, "dolt write failed AND spool failed: dolt=%v spool=%v\n", cause, err)
			refused := issueops.CloseOutcome{IssueID: item.id, Err: cause}
			outcomes[item.arg] = &refused
			continue
		}
		fmt.Fprintf(os.Stderr, "queued for replay (op_id=%s, will retry on next bd command)\n", entry.OpID)
		queued := issueops.CloseOutcome{IssueID: item.id, Err: errCloseSpooled}
		outcomes[item.arg] = &queued
	}
	return true
}
