package testutil

import "testing"

// SkipIfShort skips a subprocess-spawning test in -short mode, with the
// caller naming exactly what gets spawned. It is the named wrapper the
// testing.Short() policy (scripts/check-testing-short.sh) prescribes for
// integration boundaries: raw testing.Short() stays reserved for true
// runtime/stress/large-fixture skips, while the subprocess-lane skips
// (bda-9l1: the cmd/bd suite cannot finish on a Windows dev host without a
// fast lane, so tests that build the bd binary or spawn real git/child
// processes are excluded from -short) route through this ONE site. The
// -short CI lanes rely on these skips, so the wrapper must keep exactly the
// semantics of the inline guard it replaces: skip in -short, run otherwise.
func SkipIfShort(t *testing.T, reason string) {
	t.Helper()
	if testing.Short() {
		t.Skip(reason)
	}
}
