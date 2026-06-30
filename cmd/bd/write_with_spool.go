package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/steveyegge/beads"
	"github.com/steveyegge/beads/internal/spool"
	"github.com/steveyegge/beads/internal/types"
)

// writeWithSpool wraps a bd write op with spool-on-failure path:
//   - try the live op first
//   - if returns transient error (IsTransientErr=true) -> write Entry to spool, return nil
//   - if permanent error -> return as-is (caller decides)
//   - if success -> no spool touch
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
		// No .beads dir -- can't spool, surface original error
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
		// This should never happen -- all inputs are valid Go structs/maps.
		panic(fmt.Sprintf("spool payload marshal: %v", err))
	}
	return b
}

// spoolSingletonMu guards the per-directory spool cache.
var (
	spoolSingletonMu    sync.Mutex
	spoolSingletonCache = make(map[string]*spool.Spool)
)

// spoolSingleton returns a cached *spool.Spool for the given directory.
// Using one instance per directory avoids re-opening the same files.
func spoolSingleton(dir string) *spool.Spool {
	spoolSingletonMu.Lock()
	defer spoolSingletonMu.Unlock()
	if s, ok := spoolSingletonCache[dir]; ok {
		return s
	}
	s := spool.NewSpool(dir)
	spoolSingletonCache[dir] = s
	return s
}

// spoolDispatch returns a spool.DispatchFunc that replays a queued entry
// against the global store. It handles the four allowed ops: create, update,
// note, and close.
//
// For "note" entries the payload is the same shape as "update" (id + updates
// map + actor), so a single decoder handles both.
func spoolDispatch(ctx context.Context) spool.DispatchFunc {
	return func(e spool.Entry) error {
		if store == nil {
			return fmt.Errorf("store not initialized")
		}
		switch e.Op {
		case "create":
			var p struct {
				Issue *types.Issue `json:"issue"`
				Actor string       `json:"actor"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return fmt.Errorf("decode create payload: %w", err)
			}
			if p.Issue == nil {
				return fmt.Errorf("create payload: missing issue field")
			}
			return store.CreateIssue(ctx, p.Issue, p.Actor)

		case "update", "note":
			var p struct {
				ID      string                 `json:"id"`
				Updates map[string]interface{} `json:"updates"`
				Actor   string                 `json:"actor"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return fmt.Errorf("decode %s payload: %w", e.Op, err)
			}
			return store.UpdateIssue(ctx, p.ID, p.Updates, p.Actor)

		case "close":
			var p struct {
				ID      string `json:"id"`
				Reason  string `json:"reason"`
				Actor   string `json:"actor"`
				Session string `json:"session"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return fmt.Errorf("decode close payload: %w", err)
			}
			return store.CloseIssue(ctx, p.ID, p.Reason, p.Actor, p.Session)

		default:
			return fmt.Errorf("unknown op %q in spool entry %s", e.Op, e.OpID)
		}
	}
}
