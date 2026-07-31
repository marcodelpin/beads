package httpapi

// The `limit` defaults for the two list operations. Both surfaces — the CLI
// flag and the HTTP query parameter — must read ONE constant, so the promise
// that HTTP and CLI cannot diverge is structural instead of documentary.
// TestSpecDefaultsMatchSharedConstants pins the spec's documented defaults to
// these values.
//
// INTERIM HOME. The shared constants belong in internal/workapi
// (DefaultListLimit / DefaultReadyLimit), landing with the filter-builder
// extraction; this slice branches from main, where that package does not exist
// yet, and declaring it here rather than in cmd/bd keeps the values reachable
// from the spec tests without importing package main. When workapi lands,
// delete these two constants and re-point the spec tests at it — nothing else
// consumes them.
//
// Until then TestDefaultsMatchCLIFlags reads the cobra flag registrations in
// cmd/bd and fails if the CLI's defaults move away from these, so the temporary
// duplication cannot drift in silence. That test also pins `bd ready`'s --sort
// default, which has no constant to share at all.
const (
	// DefaultListLimit is `bd list`'s --limit default (cmd/bd/list.go).
	DefaultListLimit = 50
	// DefaultReadyLimit is `bd ready`'s --limit default (cmd/bd/ready.go).
	DefaultReadyLimit = 100
)
