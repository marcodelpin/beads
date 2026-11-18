package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/internal/spool"
	"github.com/steveyegge/beads/internal/storage"
)

// spoolBackendFake is the global store as writeWithSpool and spoolDispatch see
// it: only IsRemoteBackendStore is answered. The embedded DoltStorage is nil,
// so any other call panics and the test says so if the seam starts reading
// something else.
type spoolBackendFake struct {
	storage.DoltStorage
	remote bool
}

func (f *spoolBackendFake) IsRemoteBackendStore() bool { return f.remote }

// TestWriteWithSpoolRefusesRemoteBackend pins bda-tfok.4.2 F4: on the HTTP
// backend (a remote bd serve) the offline write-spool must not queue. Replay
// goes through the legacy store methods (spoolDispatch), which the HTTP client
// answers as unsupported, so a queued entry could only ever dead-letter: the
// command would report "queued for replay" for a write that never lands. The
// transient error is returned instead, as on any non-spoolable write. The
// local control proves the same transient failure is still queued there.
func TestWriteWithSpoolRefusesRemoteBackend(t *testing.T) {
	for _, tc := range []struct {
		name       string
		remote     bool
		wantSpool  bool
		wantQueued int64
	}{
		{name: "remote backend fails loud", remote: true, wantSpool: false, wantQueued: 0},
		{name: "local store still queues", remote: false, wantSpool: true, wantQueued: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beadsDir := filepath.Join(t.TempDir(), ".beads")
			if err := os.MkdirAll(beadsDir, 0o755); err != nil {
				t.Fatal(err)
			}
			// FindBeadsDir only accepts a dir carrying project files.
			if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("BEADS_DIR", beadsDir)

			saved := store
			store = &spoolBackendFake{remote: tc.remote}
			t.Cleanup(func() { store = saved })

			transient := errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")
			if !spool.IsTransientErr(transient) {
				t.Fatalf("fixture error must classify transient: %v", transient)
			}
			out, err := writeWithSpool(context.Background(), "update",
				spoolPayload(map[string]interface{}{"id": "x-1", "updates": map[string]interface{}{"title": "t"}, "actor": "a"}),
				func() error { return transient })

			lines, qerr := spool.NewSpool(filepath.Join(beadsDir, "spool")).QueueLineCount()
			if qerr != nil {
				t.Fatalf("QueueLineCount: %v", qerr)
			}
			if out.Spooled != tc.wantSpool {
				t.Errorf("Spooled = %v, want %v", out.Spooled, tc.wantSpool)
			}
			if lines != tc.wantQueued {
				t.Errorf("queue lines = %d, want %d", lines, tc.wantQueued)
			}
			if tc.wantSpool {
				if err != nil {
					t.Errorf("err = %v, want nil (queued)", err)
				}
			} else if !errors.Is(err, transient) {
				t.Errorf("err = %v, want the transient error surfaced", err)
			}
		})
	}
}
