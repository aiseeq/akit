//go:build linux

package proc

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// CancelByStop installs a verified cancellation handler on cmd; call it before Start.
// Own process groups and sessions use StopChildGroup; other commands use StopChild.
func CancelByStop(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) error {
	if cmd == nil || cmd.Process != nil {
		return errors.New("CancelByStop requires a command before Start")
	}
	if grace < 0 || grace > 24*time.Hour {
		return errors.New("grace must be between 0 and 24h")
	}
	if sig <= 0 || sig > 64 {
		return errors.New("invalid signal")
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return errors.ErrUnsupported
		}
		if ownGroupConfig(cmd.SysProcAttr) {
			_, err := StopChildGroup(cmd, grace, sig)
			return err
		}
		_, err := StopChild(cmd.Process.Pid, grace, func() error {
			return SignalChild(cmd.Process.Pid, sig)
		})
		return err
	}
	return nil
}
