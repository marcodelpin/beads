package main

import "testing"

// bda-hcs5: the version-witness parsers must tolerate semver prerelease and
// build suffixes. A witness like "1.2.1-rc.1" split naively on "." yields 4
// parts, failed the parse, and - because an unparseable witness classifies as
// legacy - bd refused the workspace for every command (upstream measured 644
// gate-sweep failures in 6h from exactly this, gastownhall/beads#5675). The
// deliberate refusal of a GENUINELY unparseable witness is unchanged: only
// well-formed semver with a suffix gains recognition.
func TestVersionWitnessParsersTolerateSemverSuffixes(t *testing.T) {
	t.Run("currentVersionWitness", func(t *testing.T) {
		cases := []struct {
			version string
			want    bool
		}{
			{"1.2.1", true},
			{"v1.2.1", true},
			{"1.2.1-rc.1", true},         // prerelease suffix (the latent trigger)
			{"1.2.1+build.5", true},      // build metadata
			{"1.2.1-beta+exp.sha", true}, // both
			{"0.62.0", false},            // pre-1.0 is not a current witness
			{"0.62.0-rc.1", false},
			{"garbage", false},
			{"1.2", false},
			{"1.2.x", false},
			{"", false},
		}
		for _, c := range cases {
			if got := currentVersionWitness(c.version); got != c.want {
				t.Errorf("currentVersionWitness(%q) = %v, want %v", c.version, got, c.want)
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
