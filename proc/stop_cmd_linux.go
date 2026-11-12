//go:build linux

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

var stopCmdGroup = StopChildGroup
var stopCmdSingle = StopChild

// SignalCmd signals cmd's direct child. Wait remains the caller's responsibility;
// before Wait the child PID cannot be reused, and after Wait this returns
// os.ErrProcessDone without signaling. If pidfd_open is unavailable, it uses
// os.Process.Signal and SignalCmdWithResult reports the degraded path.
func SignalCmd(cmd *exec.Cmd, sig syscall.Signal) error {
	_, err := SignalCmdWithResult(cmd, sig)
	return err
}

// SignalCmdWithResult is SignalCmd with an explicit indication that a direct
// os.Process fallback was used because pidfd_open was unavailable.
func SignalCmdWithResult(cmd *exec.Cmd, sig syscall.Signal) (SignalResult, error) {
	return signalCmdWithResultState(cmd, sig, true)
}

func signalCmdWithResultState(cmd *exec.Cmd, sig syscall.Signal, checkProcessState bool) (SignalResult, error) {
	process, err := validateCmdSignalState(cmd, sig, checkProcessState)
	if err != nil {
		return SignalResult{}, err
	}
	err = SignalChild(process.Pid, sig)
	if !errors.Is(err, ErrPidfdUnavailable) {
		return SignalResult{}, err
	}
	if directErr := directProcessSignal(process, sig); directErr != nil {
		if errors.Is(directErr, os.ErrProcessDone) {
			return SignalResult{Degraded: true}, os.ErrProcessDone
		}
		return SignalResult{Degraded: true}, directErr
	}
	return SignalResult{Degraded: true}, nil
}

// StopCmd stops cmd's direct child, or its verified process group when cmd was
// started as its own group/session. If pidfd_open is unavailable it falls back
// to the direct child only and marks StopResult.Degraded; descendants are then
// left alone. A nonzero Setpgid Pgid is treated as a direct child for safety.
func StopCmd(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) (StopResult, error) {
	return stopCmdLinux(cmd, grace, sig, true)
}

func stopCmdForCancel(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) (StopResult, error) {
	return stopCmdLinux(cmd, grace, sig, false)
}

func stopCmdLinux(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal, checkProcessState bool) (StopResult, error) {
	process, err := validateCmdStopState(cmd, grace, sig, checkProcessState)
	if err != nil {
		return StopResult{}, err
	}
	probe := ensureSignalChildPidfd
	if ownGroupConfig(cmd.SysProcAttr) {
		probe = ensurePidfdAvailable
	}
	if err := probe(process.Pid); errors.Is(err, ErrPidfdUnavailable) {
		return stopCmdDirectState(cmd, grace, sig, true, checkProcessState)
	}
	if ownGroupConfig(cmd.SysProcAttr) {
		result, err := stopCmdGroup(cmd, grace, sig)
		if errors.Is(err, ErrPidfdUnavailable) {
			return stopCmdDirectState(cmd, grace, sig, true, checkProcessState)
		}
		return result, err
	}
	degraded := false
	result, err := stopCmdSingle(process.Pid, grace, func() error {
		signalResult, signalErr := signalCmdWithResultState(cmd, sig, checkProcessState)
		degraded = signalResult.Degraded
		return signalErr
	})
	if errors.Is(err, ErrPidfdUnavailable) {
		return stopCmdDirectState(cmd, grace, sig, true, checkProcessState)
	}
	result.Degraded = degraded
	return result, err
}
