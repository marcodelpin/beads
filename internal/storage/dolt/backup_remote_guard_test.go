package dolt

import (
	"context"
	"path/filepath"
	"testing"
)

// TestBackupDatabase_RemoteServerSkips is the regression test for sys-c8066:
// against a REMOTE dolt sql-server (does not share the client filesystem),
// the Dolt-native backup (CALL DOLT_BACKUP) must NOT run, because the
// procedure executes server-side and a client-local file:// path is resolved
// relative to the server's cwd, creating a garbage tree (e.g.
// /var/lib/dolt/S:/Commesse/<windows-path>/.beads/backup) that grows unbounded.
//
// The guard returns before touching os.Stat or s.db, so a remoteServer store
// must return nil even when the dir does not exist. A non-remote store with a
// missing dir must surface the os.Stat error — proving the guard (not some
// other early return) is what makes the remote case a no-op.
func TestBackupDatabase_RemoteServerSkips(t *testing.T) {
	missingDir := filepath.Join(t.TempDir(), "does-not-exist")

	// Remote server: guard fires → no-op success, never reaches os.Stat / s.db.
	remote := &DoltStore{remoteServer: true}
	if err := remote.BackupDatabase(context.Background(), missingDir); err != nil {
		t.Fatalf("remote sql-server BackupDatabase must be a no-op (nil), got: %v", err)
	}

	// Local/embedded server: guard does NOT fire → os.Stat on a missing dir errors.
	// (We never reach s.db because os.Stat fails first, so nil db is safe here.)
	local := &DoltStore{remoteServer: false}
	if err := local.BackupDatabase(context.Background(), missingDir); err == nil {
		t.Fatalf("non-remote BackupDatabase must surface the missing-dir error; got nil — guard may be firing unconditionally")
	}
}
