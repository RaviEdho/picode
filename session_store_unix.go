//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

type platformSessionLock struct {
	file *os.File
}

func acquireSessionLock(path string) (SessionLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open session lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("%w: %v", ErrSessionLocked, err)
	}
	return &platformSessionLock{file: file}, nil
}

func (l *platformSessionLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
