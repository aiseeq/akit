//go:build !linux

package proc

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// StopChild is unsupported outside Linux because a bare PID cannot be verified
// as this process's direct child. It returns errors.ErrUnsupported without
// sending a signal.
func StopChild(pid int, grace time.Duration, request func() error) (StopResult, error) {
	return StopResult{}, errors.ErrUnsupported
}

// StopChildGroup kills only cmd's process on systems without Linux process
// group inspection. It requires the caller's own started Cmd and does not stop
// descendants.
func StopChildGroup(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) (StopResult, error) {
	if grace < 0 || grace > 24*time.Hour {
		return StopResult{}, errors.New("grace must be between 0 and 24h")
	}
	if sig <= 0 || sig > 64 {
		return StopResult{}, fmt.Errorf("invalid signal %d", sig)
	}
	if cmd == nil || cmd.Process == nil {
		return StopResult{}, errors.New("missing started command")
	}
	if err := cmd.Process.Kill(); err != nil {
		return StopResult{}, err
	}
	return StopResult{Killed: 1}, nil
}

// SignalChild is unsupported outside Linux because a bare PID cannot be
// verified as this process's direct child. It returns errors.ErrUnsupported
// without sending a signal.
func SignalChild(pid int, sig syscall.Signal) error {
	return errors.ErrUnsupported
}

// StopFromShell is available only on Linux.
func StopFromShell(int, time.Duration, syscall.Signal) (StopResult, error) {
	return StopResult{}, errors.New("StopFromShell is available only on Linux")
}
