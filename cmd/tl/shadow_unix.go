//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

var shadowLockContended = func() {}

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

func lockShadowResult(path string) (func(), *os.File, error) {
	release, f, _, err := lockShadowResultTimeout(path, 0)
	return release, f, err
}

func lockShadowResultTimeout(path string, timeout time.Duration) (func(), *os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, false, fmt.Errorf("open %s: %w", path, err)
	}
	if timeout <= 0 {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return nil, nil, false, fmt.Errorf("lock %s: %w", path, err)
		}
	} else {
		deadline := time.Now().Add(timeout)
		for {
			err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				break
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
				f.Close()
				return nil, nil, false, fmt.Errorf("lock %s: %w", path, err)
			}
			shadowLockContended()
			if !time.Now().Before(deadline) {
				f.Close()
				return nil, nil, false, nil
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return func() {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
			fmt.Fprintf(os.Stderr, "tl: release shadow lock: %v\n", err)
		}
	}, f, true, nil
}

// detached starts the repeat in a session of its own: it outlives the call and its terminal.
func detached() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
