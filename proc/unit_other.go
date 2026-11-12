//go:build !linux

package proc

import (
	"context"
	"errors"
	"syscall"
)

// SignalOwnUnit is unsupported outside Linux and sends no signal.
func SignalOwnUnit(syscall.Signal) (int, error) { return 0, errors.ErrUnsupported }

// SignalOwnUnitContext is unsupported outside Linux and sends no signal.
func SignalOwnUnitContext(context.Context, syscall.Signal) (int, error) {
	return 0, errors.ErrUnsupported
}
