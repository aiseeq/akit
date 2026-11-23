//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// takeSlot holds the lock file at path until release or the end of the process; a nil release — another process
// holds it.
func takeSlot(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("shadow slot: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, fmt.Errorf("shadow slot %s: %w", path, err)
	}
	return func() { f.Close() }, nil
}

// detached starts the repeat in a session of its own: it outlives the call and its terminal.
func detached() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
