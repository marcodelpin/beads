package doltserver

import (
	"fmt"
	"os"
	"sync"

	"github.com/steveyegge/beads/internal/configfile"
)

// RemoteFallbackPort reports the port to dial for a Dolt server that lives on
// another machine when no source in the precedence chain resolved one.
//
// Port 0 can never work for a remote host: auto-start is local-only (see
// canAutoStart in the storage layer), so nothing will ever allocate an
// ephemeral port for it, and dialing host:0 is an immediate connection
// refused. DefaultSharedServerPort is the documented deployment for a remote
// beads server, so a fresh clone — metadata.json carries dolt_server_host but
// the gitignored .beads/dolt-server.port does not exist yet — works out of the
// box (bda-i69).
//
// This function is the single source of that decision. Both the storage layer
// (applyConfigDefaults, which opens the connection) and DefaultConfig (which
// the read-only diagnostics `bd dolt status`, `bd dolt show` and `bd dolt
// test` resolve their port through) call it, so status can never report a port
// the connection path would not have used. Before this was shared, `bd dolt
// push` defaulted to 3308 and succeeded while `bd dolt status` reported
// Port 0 / connection refused and called the same healthy remote unreachable
// (kb-fwrz).
//
// Reports ok=false when the fallback must not apply:
//   - a local host: auto-start allocates the port instead;
//   - a configured unix socket: the TCP port is unused;
//   - test mode: a test must never silently reach a production shared server.
func RemoteFallbackPort(host string) (int, bool) {
	if configfile.IsLocalHostString(host) {
		return 0, false
	}
	if os.Getenv("BEADS_DOLT_SERVER_SOCKET") != "" {
		return 0, false
	}
	if os.Getenv("BEADS_TEST_MODE") == "1" {
		return 0, false
	}
	return DefaultSharedServerPort, true
}

var remoteFallbackNoticeOnce sync.Once

// AnnounceRemoteFallbackPort prints the operator-facing notice that bd filled
// in the port for a remote host, at most once per process — the fallback is
// resolved on nearly every command in such a repo, and repeating the line per
// resolution would bury the output it accompanies.
//
// The notice names the fix (write .beads/dolt-server.port) because the
// fallback is a guess: it is right for the documented shared-server
// deployment and wrong for any remote server on another port.
func AnnounceRemoteFallbackPort(host string, port int) {
	remoteFallbackNoticeOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "Info: no Dolt port configured for remote server %s — defaulting to %d (echo <port> > .beads/dolt-server.port to override)\n",
			host, port)
	})
}
