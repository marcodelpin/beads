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

// spoolOutcome tells the caller HOW writeWithSpool completed: applied live
// against the store, or queued to the offline spool for later replay. Callers
// whose post-write logic depends on server-side effects (e.g. create's
// store-generated issue ID) MUST branch on Spooled instead of assuming the
// write landed (GH#4378-review D1: a spooled create has NO ID yet).
type spoolOutcome struct {
	Spooled bool
	OpID    string
}

// writeWithSpool wraps a bd write op with spool-on-failure path:
//   - try the live op first
//   - if returns transient error (IsTransientErr=true) -> write Entry to spool,
//     return {Spooled:true, OpID}, nil
//   - if permanent error -> return as-is (caller decides)
//   - if success -> no spool touch, zero-value outcome
func writeWithSpool(ctx context.Context, op string, payload []byte, directWrite func() error) (spoolOutcome, error) {
	var out spoolOutcome
	err := directWrite()
	if err == nil {
		return out, nil
	}

	if !spool.IsTransientErr(err) {
		return out, err
	}

	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		// No .beads dir -- can't spool, surface original error
		return out, err
	}

	s := spool.NewSpool(filepath.Join(beadsDir, "spool"))
	entry, spoolErr := s.Append(ctx, op, payload, false, "bd-cli")
	if spoolErr != nil {
		fmt.Fprintf(os.Stderr, "dolt write failed AND spool failed: dolt=%v spool=%v\n", err, spoolErr)
		return out, err // original error wins
	}

	fmt.Fprintf(os.Stderr, "queued for replay (op_id=%s, will retry on next bd command)\n", entry.OpID)
	out.Spooled = true
	out.OpID = entry.OpID
	return out, nil
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
		// Guard against the command-teardown race: PersistentPostRunE joins
		// the drain goroutine before closing the store, but if that wait
		// times out this check keeps a late dispatch from touching a closed
		// store. The error text matches IsTransientErr's "store shutting
		// down" signature so the entry stays queued instead of dead-lettering.
		lockStore()
		active := isStoreActive() && getStore() != nil
		unlockStore()
		if !active || store == nil {
			return fmt.Errorf("store shutting down; entry stays queued")
		}
		switch e.Op {
		case "create":
			var p struct {
				Issue        *types.Issue        `json:"issue"`
				Actor        string              `json:"actor"`
				Dependencies []*types.Dependency `json:"dependencies"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return fmt.Errorf("decode create payload: %w", err)
			}
			if p.Issue == nil {
				return fmt.Errorf("create payload: missing issue field")
			}
			if err := store.CreateIssue(ctx, p.Issue, p.Actor); err != nil {
				return err
			}
			// Apply the dependency edges queued with the create, now that the
			// store-generated ID exists (an empty side means "the new issue").
			// Failures are warn-only, mirroring the live path's WarnError:
			// returning an error here would re-run CreateIssue on the next
			// drain cycle and duplicate the issue under a fresh ID.
			for _, dep := range resolveSpooledDeps(p.Dependencies, p.Issue.ID) {
				if err := store.AddDependency(ctx, dep, p.Actor); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: replayed create %s: dependency %s -> %s: %v\n",
						p.Issue.ID, dep.IssueID, dep.DependsOnID, err)
				}
			}
			return nil

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

// resolveSpooledDeps materializes the dependency edges queued alongside a
// spooled create. At append time the new issue's ID does not exist yet, so
// edges reference it with an EMPTY field; replay substitutes the freshly
// generated ID. Copies are returned -- the payload structs stay untouched.
func resolveSpooledDeps(deps []*types.Dependency, newID string) []*types.Dependency {
	out := make([]*types.Dependency, 0, len(deps))
	for _, dep := range deps {
		if dep == nil {
			continue
		}
		d := *dep
		if d.IssueID == "" {
			d.IssueID = newID
		}
		if d.DependsOnID == "" {
			d.DependsOnID = newID
		}
		out = append(out, &d)
	}
	return out
}
