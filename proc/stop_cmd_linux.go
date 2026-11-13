//go:build linux

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

var stopCmdGroup = StopChildGroup
var stopCmdSingle = StopChild
var cmdProcRoot = "/proc"

// SignalCmd signals cmd's direct child. cmd.Process must be a direct child
// created by cmd.Start. Wait remains the caller's responsibility; before Wait
// the child PID cannot be reused, and after Wait this returns
// os.ErrProcessDone without signaling. If pidfd_open is unavailable, it uses
// os.Process.Signal and SignalCmdWithResult reports the degraded path. It does
// not read cmd.ProcessState, so it may be called from cmd.Cancel while Wait runs.
func SignalCmd(cmd *exec.Cmd, sig syscall.Signal) error {
	_, err := SignalCmdWithResult(cmd, sig)
	return err
}

// SignalCmdWithResult is SignalCmd with an explicit indication that a direct
// os.Process fallback was used because pidfd_open was unavailable.
func SignalCmdWithResult(cmd *exec.Cmd, sig syscall.Signal) (SignalResult, error) {
	process, err := validateCmdSignal(cmd, sig)
	if err != nil {
		return SignalResult{}, err
	}
	if err := validateLinuxCmdChild(process.Pid); err != nil {
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
// Like SignalCmd it may be called from cmd.Cancel while Wait runs.
func StopCmd(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) (StopResult, error) {
	process, err := validateCmdStop(cmd, grace, sig)
	if err != nil {
		return StopResult{}, err
	}
	if err := validateLinuxCmdChild(process.Pid); err != nil {
		return StopResult{}, err
	}
	probe := ensureSignalChildPidfd
	if ownGroupConfig(cmd.SysProcAttr) {
		probe = ensurePidfdAvailable
	}
	if err := probe(process.Pid); errors.Is(err, ErrPidfdUnavailable) {
		return stopCmdDirect(cmd, grace, sig, true)
	}
	if ownGroupConfig(cmd.SysProcAttr) {
		result, err := stopCmdGroup(cmd, grace, sig)
		if errors.Is(err, ErrPidfdUnavailable) {
			return stopCmdDirect(cmd, grace, sig, true)
		}
		return result, err
	}
	degraded := false
	result, err := stopCmdSingle(process.Pid, grace, func() error {
		signalResult, signalErr := SignalCmdWithResult(cmd, sig)
		degraded = signalResult.Degraded
		return signalErr
	})
	if errors.Is(err, ErrPidfdUnavailable) {
		return stopCmdDirect(cmd, grace, sig, true)
	}
	result.Degraded = degraded
	return result, err
}

func validateLinuxCmdChild(pid int) error {
	if err := validateCmdPID(pid); err != nil {
		return err
	}
	stat, err := ReadStat(cmdProcRoot, pid)
	if err != nil {
		return fmt.Errorf("verify command child PID %d: %w", pid, err)
	}
	if stat.Ppid != os.Getpid() {
		return fmt.Errorf("command PID %d is not a direct child", pid)
	}
	return nil
}
