package schema

import "testing"

// TestLatestVersionIncludesMigration0069 pins the slot the opt-in label
// vocabulary claims. It supersedes 0068's own version of this test the same
// way 0068's superseded 0067's: the pin moves to whichever migration is the
// current top. Same shape and same purpose: a hardcoded literal, so the NEXT
// migration to claim a slot fails here and has to say why its number is what
// it is, rather than silently inheriting whatever LatestVersion() drifted to.
//
// This registry originally landed on 0067 and was renumbered to 0068 when
// #6304 took 0067 for versioned beads on main (2bb1e20de), then to 0069 when
// #6650 took 0068 for attribution_status (da6263374). The first collision was
// caught by schema's own duplicate-version panic, not by a review, which is
// exactly why the pin is a literal.
func TestLatestVersionIncludesMigration0069(t *testing.T) {
	const want = 69
	if got := LatestVersion(); got != want {
		t.Fatalf("LatestVersion() = %d, want %d (label_definitions migration slot claimed by the opt-in label vocabulary)", got, want)
	}
}
