//go:build !cgo

package embeddeddolt

import (
	"context"
	"errors"
)

// EmbeddedDoltStore is a stub for builds without CGO.
type EmbeddedDoltStore struct {
	dataDir  string
	database string
	branch   string
}

// Option configures optional behavior for Open (stub: no-op).
type Option func(*struct{})

// WithLock is a no-op in non-CGO builds.
func WithLock(_ Unlocker) Option {
	return func(*struct{}) {}
}

var errNoCGO = errors.New("embeddeddolt: requires CGO (build with CGO_ENABLED=1)")

// Open returns an error when CGO is not enabled.
func Open(_ context.Context, _, _, _ string, _ ...Option) (*EmbeddedDoltStore, error) {
	return nil, errNoCGO
}
