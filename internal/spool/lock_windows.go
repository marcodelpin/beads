//go:build windows

package spool

import (
	"os"

	"golang.org/x/sys/windows"
)

// fileLock implements FileLock via LockFileEx on Windows.
type fileLock struct {
	f    *os.File
	path string
}

func (l *fileLock) Lock() error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(
		windows.Handle(l.f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		1, 0, // lock 1 byte (entire file region semantics)
		ol,
	)
}

func (l *fileLock) TryLock() error {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(
		windows.Handle(l.f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1, 0,
		ol,
	)
	if err != nil {
		return ErrLockHeld
	}
	return nil
}

func (l *fileLock) Unlock() error {
	ol := new(windows.Overlapped)
	err1 := windows.UnlockFileEx(
		windows.Handle(l.f.Fd()),
		0,
		1, 0,
		ol,
	)
	err2 := l.f.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

func (l *fileLock) Path() string { return l.path }
