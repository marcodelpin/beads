package main

import (
	"testing"

	"github.com/steveyegge/beads/internal/doltserver"
)

// TestExternalStatusPort pins the bda-guxr contract: `bd dolt status` must
// dial the same port the data commands dial. A remote host with no resolved
// port gets the shared-server default (never 0 - host:0 is not dialable and
// reads as a false outage); a local host keeps 0 (auto-start allocates); an
// explicitly resolved port always wins.
func TestExternalStatusPort(t *testing.T) {
	cases := []struct {
		name     string
		resolved int
		host     string
		want     int
	}{
		{"remote_unresolved_gets_shared_default", 0, "forgejo-mdp.mdp", doltserver.DefaultSharedServerPort},
		{"local_unresolved_stays_zero", 0, "localhost", 0},
		{"loopback_unresolved_stays_zero", 0, "127.0.0.1", 0},
		{"remote_resolved_wins", 3309, "forgejo-mdp.mdp", 3309},
		{"local_resolved_wins", 3307, "localhost", 3307},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := externalStatusPort(tc.resolved, tc.host); got != tc.want {
				t.Errorf("externalStatusPort(%d, %q) = %d, want %d", tc.resolved, tc.host, got, tc.want)
			}
		})
	}
}
