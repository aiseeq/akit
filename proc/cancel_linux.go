//go:build linux

package proc

import (
	"os/exec"
	"syscall"
	"time"
)

// CancelByStop installs StopCmd as cmd's cancellation handler; call it before
// Start. A nonzero Setpgid Pgid is treated as a direct child for safety.
func CancelByStop(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) error {
	return installCancelByStop(cmd, grace, sig, stopCmdForCancel)
}
