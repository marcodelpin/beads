package db

import (
	"os"
	"testing"

	"github.com/steveyegge/beads/internal/testutil"
)

// TestMain terminates the shared Dolt test container this package starts via
// testutil.RequireDoltContainer. The testcontainers ryuk reaper is
// deliberately disabled on docker-in-LXC hosts (AppArmor policy-admin gap,
// see startDoltContainer in testutil/testdoltserver.go), so nothing reaps a
// container the test process does not terminate itself: every `go test` of a
// package lacking this hook leaked one Up container until docker create
// starved and container-backed suites went SKIP-green (bda-z6um: 65
// accumulated). Terminate-only, after m.Run: the container still starts
// lazily on first RequireDoltContainer, and terminating a never-started
// singleton is a safe no-op.
func TestMain(m *testing.M) {
	code := m.Run()
	testutil.TerminateDoltContainer()
	os.Exit(code)
}
