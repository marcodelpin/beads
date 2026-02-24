//go:build cgo

package fix

import (
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/beads/internal/testutil"
)

// TestMain starts an isolated Dolt server so fix tests don't hit the
// production server on port 3307.
func TestMain(m *testing.M) {
	srv, cleanup := testutil.StartTestDoltServer("fix-test-dolt-*")
	if srv != nil {
		os.Setenv("BEADS_DOLT_PORT", fmt.Sprintf("%d", srv.Port))
	}

	code := m.Run()

	os.Unsetenv("BEADS_DOLT_PORT")
	cleanup()
	os.Exit(code)
}
