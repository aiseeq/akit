//go:build !linux

package proc

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// SignalCmd signals only cmd's direct process. cmd.Process must come from
// cmd.Start; the caller owns Wait, and after Wait this returns os.ErrProcessDone
// without signaling.
func SignalCmd(cmd *exec.Cmd, sig syscall.Signal) error {
	process, err := validateCmdSignal(cmd, sig)
	if err != nil {
		return err
	}
	return directProcessSignal(process, sig)
}

// SignalCmdWithResult is SignalCmd with an explicit degradation result. The
// non-Linux direct-child implementation is the native path and is not degraded.
func SignalCmdWithResult(cmd *exec.Cmd, sig syscall.Signal) (SignalResult, error) {
	return SignalResult{}, SignalCmd(cmd, sig)
}

// StopCmd stops only cmd's direct process from cmd.Start. It waits the full grace period
// before Kill because it cannot inspect descendants or safely poll by PID. Where
// the platform rejects sig as unsupported (Windows accepts only Kill), it kills
// at once and sets StopResult.SignalUnsupported.
func StopCmd(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) (StopResult, error) {
	return stopCmdDirect(cmd, grace, sig, false)
}

func stopCmdForCancel(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) (StopResult, error) {
	return stopCmdDirectState(cmd, grace, sig, false, false)
}

// StopChild is unsupported outside Linux because a bare PID cannot be verified
// as this process's direct child. It returns errors.ErrUnsupported without
// sending a signal.
func StopChild(pid int, grace time.Duration, request func() error) (StopResult, error) {
	return StopResult{}, errors.ErrUnsupported
}

// StopChildGroup kills only cmd's process on systems without Linux process
// inspection. It requires the caller's own started Cmd and does not stop
// descendants.
func StopChildGroup(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) (StopResult, error) {
	return stopChildGroupDirect(cmd, grace, sig)
}

// StopDescendants is unsupported outside Linux and sends no signal.
func StopDescendants(time.Duration, syscall.Signal) (StopResult, error) {
	return StopResult{}, errors.ErrUnsupported
}

// StopSession is unsupported outside Linux and sends no signal.
func StopSession(SessionRecord, time.Duration, syscall.Signal) (StopResult, error) {
	return StopResult{}, errors.ErrUnsupported
}

// SignalDescendant is unsupported outside Linux and sends no signal.
func SignalDescendant(int, syscall.Signal) error { return errors.ErrUnsupported }

// BecomeChildSubreaper is unsupported outside Linux.
func BecomeChildSubreaper() error { return errors.ErrUnsupported }

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
