package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

var directProcessSignal = func(process *os.Process, sig syscall.Signal) error {
	return process.Signal(sig)
}

var directProcessKill = func(process *os.Process) error { return process.Kill() }

// probeCmdProcess asks os.Process, without delivering anything, whether Wait
// has already finished with it.
var probeCmdProcess = func(process *os.Process) error { return process.Signal(syscall.Signal(0)) }

// cmdProcessWaited reports whether Wait already finished with the process.
func cmdProcessWaited(err error) bool {
	return errors.Is(err, os.ErrProcessDone)
}

// validateCmdSignal never reads cmd.ProcessState: os/exec calls Cancel from its
// own goroutine while Wait writes that field. os.Process records a completed
// Wait itself and may be queried concurrently, so a reaped child is reported
// as os.ErrProcessDone through it.
func validateCmdSignal(cmd *exec.Cmd, sig syscall.Signal) (*os.Process, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, errors.New("missing started command")
	}
	if sig <= 0 || sig > 64 {
		return nil, fmt.Errorf("invalid signal %d", sig)
	}
	if err := validateCmdPID(cmd.Process.Pid); err != nil {
		return nil, err
	}
	if cmdProcessWaited(probeCmdProcess(cmd.Process)) {
		return nil, os.ErrProcessDone
	}
	return cmd.Process, nil
}

func validateCmdPID(pid int) error {
	if pid <= 0 || pid == os.Getpid() || pid == os.Getppid() {
		return fmt.Errorf("refusing unsafe command PID %d", pid)
	}
	return nil
}

func killValidatedCmd(cmd *exec.Cmd, sig syscall.Signal) (StopResult, error) {
	process, err := validateCmdSignal(cmd, sig)
	if err != nil {
		return StopResult{}, err
	}
	if err := directProcessKill(process); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return StopResult{AlreadyExited: true}, nil
		}
		return StopResult{}, err
	}
	return StopResult{Killed: 1}, nil
}

func stopChildGroupDirect(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) (StopResult, error) {
	if grace < 0 || grace > 24*time.Hour {
		return StopResult{}, errors.New("grace must be between 0 and 24h")
	}
	return killValidatedCmd(cmd, sig)
}

func validateCmdStop(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) (*os.Process, error) {
	if grace < 0 || grace > 24*time.Hour {
		return nil, errors.New("grace must be between 0 and 24h")
	}
	return validateCmdSignal(cmd, sig)
}

func installCancelByStop(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal, stop func(*exec.Cmd, time.Duration, syscall.Signal) (StopResult, error)) error {
	return installCancelByStopWithObserver(cmd, grace, sig, stop, nil)
}

func installCancelByStopWithObserver(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal, stop func(*exec.Cmd, time.Duration, syscall.Signal) (StopResult, error), observer func(bool, error)) error {
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
		result, err := stop(cmd, grace, sig)
		if result.Degraded {
			err = errors.Join(ErrPidfdUnavailable, err)
		}
		if observer != nil {
			observer(result.Degraded, err)
		}
		return err
	}
	return nil
}

// stopCmdDirect stops only cmd's direct process, waiting the full grace period
// because checking liveness without Wait or a pidfd would race PID reuse.
func stopCmdDirect(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal, degraded bool) (StopResult, error) {
	process, err := validateCmdStop(cmd, grace, sig)
	if err != nil {
		return StopResult{}, err
	}
	if err := directProcessSignal(process, sig); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return StopResult{AlreadyExited: true, Degraded: degraded}, nil
		}
		return StopResult{Degraded: degraded}, err
	}
	if grace > 0 {
		timer := time.NewTimer(grace)
		<-timer.C
	}
	if err := directProcessKill(process); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return StopResult{AlreadyExited: true, Degraded: degraded}, nil
		}
		return StopResult{Degraded: degraded}, err
	}
	return StopResult{Killed: 1, Degraded: degraded}, nil
}
