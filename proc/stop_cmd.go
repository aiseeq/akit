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

func validateCmdSignal(cmd *exec.Cmd, sig syscall.Signal) (*os.Process, error) {
	return validateCmdSignalState(cmd, sig, true)
}

func validateCmdSignalState(cmd *exec.Cmd, sig syscall.Signal, checkProcessState bool) (*os.Process, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, errors.New("missing started command")
	}
	if checkProcessState && cmd.ProcessState != nil {
		return nil, os.ErrProcessDone
	}
	if sig <= 0 || sig > 64 {
		return nil, fmt.Errorf("invalid signal %d", sig)
	}
	if err := validateCmdPID(cmd.Process.Pid); err != nil {
		return nil, err
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
	process, err := validateCmdSignalState(cmd, sig, true)
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

func validateCmdStopState(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal, checkProcessState bool) (*os.Process, error) {
	if grace < 0 || grace > 24*time.Hour {
		return nil, errors.New("grace must be between 0 and 24h")
	}
	return validateCmdSignalState(cmd, sig, checkProcessState)
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
	return stopCmdDirectState(cmd, grace, sig, degraded, true)
}

func stopCmdDirectState(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal, degraded, checkProcessState bool) (StopResult, error) {
	process, err := validateCmdStopState(cmd, grace, sig, checkProcessState)
	if err != nil {
		return StopResult{}, err
	}
	unsupported := false
	if err := directProcessSignal(process, sig); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return StopResult{AlreadyExited: true, Degraded: degraded}, nil
		}
		// Windows delivers only Kill; a gentler signal has no effect there, so
		// waiting the grace period would only delay the Kill that follows.
		if !errors.Is(err, errors.ErrUnsupported) || sig == syscall.SIGKILL {
			return StopResult{Degraded: degraded}, err
		}
		unsupported = true
	}
	if grace > 0 && !unsupported {
		timer := time.NewTimer(grace)
		<-timer.C
	}
	if err := directProcessKill(process); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return StopResult{AlreadyExited: true, Degraded: degraded, SignalUnsupported: unsupported}, nil
		}
		return StopResult{Degraded: degraded, SignalUnsupported: unsupported}, err
	}
	return StopResult{Killed: 1, Degraded: degraded, SignalUnsupported: unsupported}, nil
}
