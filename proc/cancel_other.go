//go:build !linux

package proc

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// CancelByStop installs an unsupported cancellation handler; call it before Start.
func CancelByStop(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) error {
	if cmd == nil || cmd.Process != nil {
		return errors.New("CancelByStop requires a command before Start")
	}
	cmd.Cancel = func() error { return errors.ErrUnsupported }
	return nil
}
