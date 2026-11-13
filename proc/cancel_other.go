//go:build !linux

package proc

import (
	"os/exec"
	"syscall"
	"time"
)

// CancelByStop installs StopCmd as cmd's cancellation handler; call it before
// Start. The caller can use ctx.Err() to identify the cancellation cause. On
// Windows, which delivers only Kill, cancellation kills the process at once.
func CancelByStop(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) error {
	return installCancelByStop(cmd, grace, sig, StopCmd)
}

// CancelByStopWithObserver installs the same cancellation handler as
// CancelByStop and reports its result to observer when cancellation runs.
func CancelByStopWithObserver(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal, observer func(bool, error)) error {
	return installCancelByStopWithObserver(cmd, grace, sig, StopCmd, observer)
}
