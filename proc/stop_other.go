//go:build !linux

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// StopChild can only stop the direct process on systems without the Linux
// /proc and pidfd implementation. Descendants are not inspected.
func StopChild(pid int, grace time.Duration, request func() error) (StopResult, error) {
	if pid <= 1 {
		return StopResult{}, fmt.Errorf("refusing PID %d", pid)
	}
	if grace < 0 || grace > 24*time.Hour {
		return StopResult{}, errors.New("grace must be between 0 and 24h")
	}
	if request != nil {
		if err := request(); err != nil {
			return StopResult{}, fmt.Errorf("stop request: %w", err)
		}
		if grace > 0 {
			time.Sleep(grace)
		}
	}
	return killProcess(pid)
}

// StopChildGroup uses the available direct-process kill on systems without
// Linux process-group inspection. It does not stop descendants.
func StopChildGroup(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) (StopResult, error) {
	if grace < 0 || grace > 24*time.Hour {
		return StopResult{}, errors.New("grace must be between 0 and 24h")
	}
	if sig <= 0 || sig > 64 {
		return StopResult{}, fmt.Errorf("invalid signal %d", sig)
	}
	if cmd == nil || cmd.Process == nil {
		return StopResult{}, errors.New("missing started command")
	}
	return killProcess(cmd.Process.Pid)
}

// SignalChild only supports signals whose portable fallback is Process.Kill.
func SignalChild(pid int, sig syscall.Signal) error {
	if pid <= 1 {
		return fmt.Errorf("refusing PID %d", pid)
	}
	if sig != syscall.Signal(9) && sig != syscall.Signal(2) {
		return fmt.Errorf("unsupported signal %d", sig)
	}
	_, err := killProcess(pid)
	return err
}

// StopFromShell is available only on Linux.
func StopFromShell(int, time.Duration, syscall.Signal) (StopResult, error) {
	return StopResult{}, errors.New("StopFromShell is available only on Linux")
}

func killProcess(pid int) (StopResult, error) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return StopResult{}, fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := process.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return StopResult{AlreadyExited: true}, nil
		}
		return StopResult{}, err
	}
	return StopResult{Killed: 1}, nil
}
