//go:build windows

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
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("encode session lock path: %w", err)
	}
	// A zero share mode gives this process exclusive access until Close.
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errno, ok := err.(syscall.Errno); ok && (errno == 32 || errno == 33) {
			return nil, fmt.Errorf("%w: %v", ErrSessionLocked, err)
		}
		return nil, fmt.Errorf("open session lock: %w", err)
	}
	return &platformSessionLock{file: os.NewFile(uintptr(handle), path)}, nil
}

func (l *platformSessionLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
