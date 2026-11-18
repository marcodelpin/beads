package schema

import "testing"

// TestLatestVersionIncludesMigration0075 pins the slot the opt-in label
// vocabulary claims on this fork. The pin moves to whichever migration is the
// current top, the way upstream moves it (0067 -> 0068 -> 0069 -> ... -> 0074). Same shape and
// same purpose: a hardcoded literal, so the NEXT migration to claim a slot fails
// here and has to say why its number is what it is, rather than silently
// inheriting whatever LatestVersion() drifted to.
//
// This registry originally landed on 0067, was renumbered to 0068 when #6304
// took 0067 for versioned beads (2bb1e20de), and to 0070 when upstream took
// 0068 for attribution_status (#6650, da6263374) and 0069 for the change_at
// DATETIME(6) widening (73702cc73). No database on the shared server had
// applied the old 0068 (read-only census 2026-09-28: 183 databases, all at
// cursor 65), so the rename changes no recorded migration hash. It moved to
// 0073 when upstream took 0070 (participation_generation, #6943), 0071
// (removed_restriction, #6661) and 0072 (epoch_minted_addresses, #6664); this
// pin supersedes upstream's TestLatestVersionIncludesMigration0072. The fleet
// cut that would apply it had not happened at that rename (bda-tfok), so the
// renumber again changes no recorded migration hash. It moved to 0075 when
// upstream took 0073 (arrival_seq) and 0074 (id tombstones); this pin
// supersedes upstream's TestLatestVersionIncludesMigration0074 and keeps its
// ignored-plane check (ignored 0030, wisp_id_tombstones), which no fork
// migration moves. The cut still had not happened, so no recorded hash moves.
func TestLatestVersionIncludesMigration0075(t *testing.T) {
	const want = 75
	if got := LatestVersion(); got != want {
		t.Fatalf("LatestVersion() = %d, want %d (label_definitions migration slot claimed by the opt-in label vocabulary)", got, want)
	}
	const wantIgnored = 30
	if got := LatestIgnoredVersion(); got != wantIgnored {
		t.Fatalf("LatestIgnoredVersion() = %d, want %d (wisp_id_tombstones on fresh clones)", got, wantIgnored)
	}
}
