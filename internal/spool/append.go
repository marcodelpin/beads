package spool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Append is the producer entry point. It enqueues a single Entry into
// queue.jsonl, returning the persisted Entry on success.
//
// Fills missing fields: OpID (random 32-hex), TS (now UTC), ContentHash
// (blake3 of payload), Attempts (0), SchemaVersion (1).
//
// Disk cap: before opening queue.jsonl we Stat it. If already at or above
// MaxQueueBytes, return ErrSpoolFull WITHOUT writing.
//
// Allowed ops: create, update, note, close. Others rejected.
func (s *Spool) Append(ctx context.Context, op string, payload []byte, sync bool, origin string) (Entry, error) {
	_ = ctx // reserved for future lock acquisition timeout

	if !AllowedOps[op] {
		return Entry{}, fmt.Errorf("spool: unknown op %q (allowed: create, update, note, close)", op)
	}

	if err := s.EnsureDir(); err != nil {
		return Entry{}, fmt.Errorf("ensure dir: %w", err)
	}

	// Disk cap gate -- STAT before write, refuse loud if at limit.
	if size, err := s.QueueDiskBytes(); err != nil {
		return Entry{}, fmt.Errorf("stat queue for cap check: %w", err)
	} else if size >= MaxQueueBytes {
		return Entry{}, ErrSpoolFull
	}

	hash, err := CanonicalHash(payload)
	if err != nil {
		return Entry{}, fmt.Errorf("canonical hash: %w", err)
	}

	id, err := newID()
	if err != nil {
		return Entry{}, fmt.Errorf("new id: %w", err)
	}

	e := Entry{
		OpID:          id,
		TS:            time.Now().UTC().Format(time.RFC3339),
		Op:            op,
		Payload:       json.RawMessage(payload),
		Attempts:      0,
		SchemaVersion: 1,
		ContentHash:   hash,
		Origin:        origin,
	}

	if err := appendJSONL(s.queueFile, e); err != nil {
		return Entry{}, fmt.Errorf("append queue: %w", err)
	}
	return e, nil
}

// newID returns a 32-hex random string (16 crypto-random bytes).
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// IsTransientErr classifies an error from a Dolt/storage call. True means
// the producer should fall back to spool. False means surface the error
// directly (permanent failure -- spooling would just dead-letter).
//
// Transient: context deadline/canceled, net timeouts, Dolt i/o timeout,
// connection refused, EOF, 5xx HTTP.
// Permanent: SQL constraint violations, schema errors, 4xx HTTP.
func IsTransientErr(err error) bool {
	if err == nil {
		return false
	}
	// Context errors are transient (drainer retries under its own budget).
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	// HTTPStatusErr: 5xx transient, 4xx permanent.
	var hse *HTTPStatusErr
	if errors.As(err, &hse) { //nolint:errorlint // duck-type intentionally
		return hse.Status >= 500
	}
	// net.Error duck-typed check for Timeout()/Temporary().
	type timeoutI interface{ Timeout() bool }
	type tempI interface{ Temporary() bool }
	if t, ok := err.(timeoutI); ok && t.Timeout() {
		return true
	}
	if t, ok := err.(tempI); ok && t.Temporary() {
		return true
	}
	// Dolt-specific transient strings (i/o timeout, connection refused, EOF).
	msg := err.Error()
	for _, pat := range []string{
		"i/o timeout",
		"connection refused",
		"connection reset",
		"eof",
		"broken pipe",
		"driver: bad connection",
	} {
		if strings.Contains(strings.ToLower(msg), pat) {
			return true
		}
	}
	// SQL constraint violations are permanent (dead-letter).
	// Case-insensitive match: MySQL/Dolt errors capitalize "Duplicate entry"
	// while SQLite uses "UNIQUE constraint" -- normalize to compare reliably.
	msgLower := strings.ToLower(msg)
	for _, pat := range []string{
		"duplicate entry",
		"foreign key",
		"constraint",
		"unique index",
	} {
		if strings.Contains(msgLower, pat) {
			return false
		}
	}
	// Default: an unclassified error is PERMANENT -- surface it, do not spool.
	// Only the KNOWN-transient signatures above are queued for replay. Spooling an
	// unclassified error (e.g. a validation/logic failure like an invalid --type)
	// would swallow it and report a false success. A genuinely new transient
	// signature must be ADDED to the lists above, not caught by a blanket default.
	return false
}

// HTTPStatusErr wraps a non-2xx HTTP response so callers can fold status
// code into IsTransientErr classification.
type HTTPStatusErr struct {
	Status int
	Body   string
}

func (e *HTTPStatusErr) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}
