//go:build !windows

package spool

import (
	"os"
	"syscall"
)

// fileLock implements FileLock via flock(2) on Unix systems.
type fileLock struct {
	f    *os.File
	path string
}

func (l *fileLock) Lock() error {
	return syscall.Flock(int(l.f.Fd()), syscall.LOCK_EX)
}

func (l *fileLock) TryLock() error {
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		// EWOULDBLOCK means the lock is held.
		return ErrLockHeld
	}
	return nil
}

func (l *fileLock) Unlock() error {
	err1 := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	err2 := l.f.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

func (l *fileLock) Path() string { return l.path }
