package schema

import "testing"

// TestLatestVersionIncludesMigration0070 pins the slot the opt-in label
// vocabulary claims on this fork. The pin moves to whichever migration is the
// current top, the way upstream moves it (0067 -> 0068 -> 0069). Same shape and
// same purpose: a hardcoded literal, so the NEXT migration to claim a slot fails
// here and has to say why its number is what it is, rather than silently
// inheriting whatever LatestVersion() drifted to.
//
// This registry originally landed on 0067, was renumbered to 0068 when #6304
// took 0067 for versioned beads (2bb1e20de), and to 0070 when upstream took
// 0068 for attribution_status (#6650, da6263374) and 0069 for the change_at
// DATETIME(6) widening (73702cc73). No database on the shared server had
// applied the old 0068 (read-only census 2026-09-28: 183 databases, all at
// cursor 65), so the rename changes no recorded migration hash.
func TestLatestVersionIncludesMigration0070(t *testing.T) {
	const want = 70
	if got := LatestVersion(); got != want {
		t.Fatalf("LatestVersion() = %d, want %d (label_definitions migration slot claimed by the opt-in label vocabulary)", got, want)
	}
}
