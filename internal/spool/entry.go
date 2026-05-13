// Package spool provides the offline write-spool for bd. When Dolt is
// unreachable or slow, write commands (create, update, note, close) enqueue
// operations into a per-repo JSONL queue (.beads/spool/queue.jsonl) instead
// of silently losing the user's intent.
//
// Spool layout (all files under the spoolDir):
//
//	queue.jsonl           append-only WAL, one Entry per line (producers append)
//	inflight.jsonl        entries currently being drained (crash recovery)
//	acked/YYYY-MM-DD.jsonl successfully drained, 7-day retention
//	dead-letter.jsonl     non-retryable entries, manual review
//	cursor.json           {last_drain_ts, last_acked_offset, queue_size}
package spool

import (
	"encoding/json"
	"fmt"
)

// AllowedOps lists the bd write operations the spool accepts.
// Anything else is rejected at JSON-decode time.
var AllowedOps = map[string]bool{
	"create": true,
	"update": true,
	"note":   true,
	"close":  true,
}

// Entry is the persisted spool record. The JSON field names are the wire
// contract between producers and the drainer — do NOT rename without a
// migration. Schema version field v lets mixed-version fleets degrade
// gracefully.
type Entry struct {
	OpID          string          `json:"op_id"`            // 32-hex random, dedup key
	TS            string          `json:"ts"`               // RFC3339 UTC at enqueue
	Op            string          `json:"op"`               // create | update | note | close
	Payload       json.RawMessage `json:"payload"`          // op-specific args
	Attempts      int             `json:"attempts"`         // replay attempts so far
	FirstFailedAt string          `json:"first_failed_at,omitempty"` // RFC3339; set on first failure
	SchemaVersion int             `json:"v"`                // schema version, currently 1
	ContentHash   string          `json:"content_hash"`     // blake3 hex of canonical payload
	Origin        string          `json:"origin,omitempty"` // e.g. "bd-cli"
	LastError     string          `json:"last_error,omitempty"`
}

// Cursor tracks drain progress + spool size for metrics.
type Cursor struct {
	LastDrainTS     string `json:"last_drain_ts"`
	LastAckedOffset int64  `json:"last_acked_offset"`
	QueueSize       int64  `json:"queue_size"`
	DeadLetterCount int64  `json:"dead_letter_count"`
}

// ValidateEntry checks that an Entry has required fields and a valid op.
func ValidateEntry(e Entry) error {
	if e.OpID == "" {
		return fmt.Errorf("spool: missing op_id")
	}
	if e.SchemaVersion < 1 {
		return fmt.Errorf("spool: invalid schema version %d (need >= 1)", e.SchemaVersion)
	}
	if !AllowedOps[e.Op] {
		return fmt.Errorf("spool: unknown op %q (allowed: create, update, note, close)", e.Op)
	}
	if len(e.Payload) == 0 {
		return fmt.Errorf("spool: empty payload for op %q", e.Op)
	}
	return nil
}
