package main

import "testing"

// bda-hcs5: the version-witness parsers must tolerate semver prerelease and
// build suffixes. A witness like "1.2.1-rc.1" split naively on "." yields 4
// parts, failed the parse, and - because an unparseable witness classified as
// legacy - bd refused the workspace for every command (upstream measured 644
// gate-sweep failures in 6h from exactly this, gastownhall/beads#5675).
//
// Re-pointed after upstream #5650 replaced currentVersionWitness with
// classifyVersionWitness (a three-way era classifier that ALSO stops reading
// "unreadable" as "legacy" - the second half of the same defect family). The
// property this test pins is unchanged: well-formed semver with a suffix is
// recognized in its era, and a GENUINELY unparseable witness is never
// misclassified into an era that relaxes or triggers the legacy guard.
func TestVersionWitnessParsersTolerateSemverSuffixes(t *testing.T) {
	t.Run("classifyVersionWitness", func(t *testing.T) {
		cases := []struct {
			version string
			want    witnessEra
		}{
			{"1.2.1", witnessEraCurrent},
			{"v1.2.1", witnessEraCurrent},
			{"1.2.1-rc.1", witnessEraCurrent},         // prerelease suffix (the latent trigger)
			{"1.2.1+build.5", witnessEraCurrent},      // build metadata
			{"1.2.1-beta+exp.sha", witnessEraCurrent}, // both
			{"0.62.0", witnessEraLegacy},              // pre-1.0
			{"0.62.0-rc.1", witnessEraLegacy},
			{"garbage", witnessEraUnknown}, // unreadable is NOT legacy (#5650)
			// x/mod semver accepts an omitted patch ("v1.2"), so upstream's
			// parse places it in the v1 era - a change from the old 3-part
			// core check, and the safe direction (era from major, not shape).
			{"1.2", witnessEraCurrent},
			{"1.2.x", witnessEraUnknown},
			{"", witnessEraUnknown},
		}
		for _, c := range cases {
			if got := classifyVersionWitness(c.version); got != c.want {
				t.Errorf("classifyVersionWitness(%q) = %v, want %v", c.version, got, c.want)
			}
		}
	})
	t.Run("legacyVersionMinor", func(t *testing.T) {
		cases := []struct {
			version string
			minor   int
			ok      bool
		}{
			{"0.62.0", 62, true},
			{"0.60.1-beta", 60, true}, // suffixed historical witness still recognized
			{"1.2.1", 0, false},
			{"junk", 0, false},
		}
		for _, c := range cases {
			minor, ok := legacyVersionMinor(c.version)
			if minor != c.minor || ok != c.ok {
				t.Errorf("legacyVersionMinor(%q) = (%d,%v), want (%d,%v)", c.version, minor, ok, c.minor, c.ok)
			}
		}
	})
}
