// FileLock provides cross-platform exclusive file locking for the spool
// directory. On Unix this wraps flock(2); on Windows it wraps LockFileEx.
// The lock coordinates concurrent bd CLI processes draining the same spool.
package spool

import (
	"fmt"
	"os"
)

// FileLock is an exclusive advisory lock on a file. Callers must call
// Unlock (typically via defer) exactly once after a successful Lock or
// TryLock. The lock is released when the file is closed.
type FileLock interface {
	// Lock acquires an exclusive blocking lock. It blocks until the lock
	// is available or an error occurs.
	Lock() error

	// TryLock attempts a non-blocking exclusive lock. Returns
	// ErrLockHeld if another process holds the lock.
	TryLock() error

	// Unlock releases the lock and closes the underlying file.
	Unlock() error

	// Path returns the lock file path for diagnostics.
	Path() string
}

// ErrLockHeld is returned by TryLock when another process holds the lock.
var ErrLockHeld = fmt.Errorf("spool: lock held by another process")

// OpenLock opens (or creates) the file at path and returns a FileLock
// handle. The file is NOT locked on return -- call Lock or TryLock.
func OpenLock(path string) (FileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 - internal lock path
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	return &fileLock{f: f, path: path}, nil
}
