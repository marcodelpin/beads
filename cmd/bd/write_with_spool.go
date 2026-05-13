package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/beads"
	"github.com/steveyegge/beads/internal/spool"
)

// writeWithSpool wraps a bd write op with spool-on-failure path:
//   - try the live op first
//   - if returns transient error (IsTransientErr=true) → write Entry to spool, return nil
//   - if permanent error → return as-is (caller decides)
//   - if success → no spool touch
func writeWithSpool(ctx context.Context, op string, payload []byte, directWrite func() error) error {
	err := directWrite()
	if err == nil {
		return nil
	}

	if !spool.IsTransientErr(err) {
		return err
	}

	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		// No .beads dir — can't spool, surface original error
		return err
	}

	s := spool.NewSpool(filepath.Join(beadsDir, "spool"))
	entry, spoolErr := s.Append(ctx, op, payload, false, "bd-cli")
	if spoolErr != nil {
		fmt.Fprintf(os.Stderr, "dolt write failed AND spool failed: dolt=%v spool=%v\n", err, spoolErr)
		return err // original error wins
	}

	fmt.Fprintf(os.Stderr, "queued for replay (op_id=%s, will retry on next bd command)\n", entry.OpID)
	return nil
}

// spoolPayload is a helper to JSON-marshal a payload for the spool.
// Panics on marshal failure (should never happen for valid data).
func spoolPayload(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// This should never happen — all inputs are valid Go structs/maps.
		panic(fmt.Sprintf("spool payload marshal: %v", err))
	}
	return b
}
