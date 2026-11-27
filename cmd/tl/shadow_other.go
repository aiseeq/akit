//go:build !unix

package main

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// takeSlot: without flock no slot is ever free, so the shadow does not start.
func takeSlot(string) (func(), error) { return nil, nil }

func lockShadowResult(path string) (func(), *os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	return func() {}, f, nil
}

func lockShadowResultTimeout(path string, _ time.Duration) (func(), *os.File, bool, error) {
	release, f, err := lockShadowResult(path)
	return release, f, err == nil, err
}

func detached() *syscall.SysProcAttr { return nil }
