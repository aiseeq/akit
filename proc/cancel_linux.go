//go:build linux

package proc

import (
	"os/exec"
	"syscall"
	"time"
)

// CancelByStop installs StopCmd as cmd's cancellation handler; call it before
// Start. A nonzero Setpgid Pgid is treated as a direct child for safety. If
// pidfd fallback was required, Cancel returns ErrPidfdUnavailable; the caller
// can use ctx.Err() to identify the cancellation cause.
func CancelByStop(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) error {
	return installCancelByStop(cmd, grace, sig, stopCmdForCancel)
}

// CancelByStopWithObserver installs the same cancellation handler as
// CancelByStop and reports its degradation flag and error when cancellation runs.
// Wait may return the process exit error instead of the Cancel error, so this
// lets callers still report a degraded direct-child fallback.
func CancelByStopWithObserver(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal, observer func(bool, error)) error {
	return installCancelByStopWithObserver(cmd, grace, sig, stopCmdForCancel, observer)
}
