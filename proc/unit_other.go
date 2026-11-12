//go:build !linux

package proc

import (
	"errors"
	"syscall"
)

// SignalOwnUnit is unsupported outside Linux and sends no signal.
func SignalOwnUnit(syscall.Signal) (int, error) { return 0, errors.ErrUnsupported }
