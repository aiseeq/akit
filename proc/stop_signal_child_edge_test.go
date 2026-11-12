//go:build linux

package proc

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSignalChildRejectsLiveGrandchildWithoutProcessDone(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	cmd := exec.Command("sh", "-c", `sleep 30 </dev/null >/dev/null 2>&1 & echo $! > "$1"; exec sleep 30`, "sh", pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
			t.Errorf("stop test group: %v", err)
		}
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	})
	waitReady(t, pidFile)
	grandchild := readSignalChildTestPID(t, pidFile)

	realSignal := signalProcess
	var signals int
	signalProcess = func(pid int, child *trackedProcess, sig unix.Signal) error {
		signals++
		return realSignal(pid, child, sig)
	}
	t.Cleanup(func() { signalProcess = realSignal })
	err := SignalChild(grandchild, syscall.SIGTERM)
	if err == nil {
		t.Fatal("SignalChild accepted a live grandchild")
	}
	if errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("live non-child error falsely wraps os.ErrProcessDone: %v", err)
	}
	if signals != 0 {
		t.Fatalf("SignalChild sent %d signals to a non-child", signals)
	}
	if stat, err := ReadStat("/proc", grandchild); err != nil || stat.State == "Z" || stat.State == "X" {
		t.Fatalf("grandchild was affected despite rejection: stat=%+v err=%v", stat, err)
	}
}

func TestSignalChildChecksZombieBeforePidfdOpenFailure(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	})
	waitForZombie(t, cmd.Process.Pid)

	realOpen := openSignalChildPidfd
	opened := false
	openSignalChildPidfd = func(int, int) (int, error) {
		opened = true
		return -1, unix.EMFILE
	}
	t.Cleanup(func() { openSignalChildPidfd = realOpen })
	err := SignalChild(cmd.Process.Pid, syscall.SIGTERM)
	if !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("SignalChild on zombie = %v, want os.ErrProcessDone despite pidfd EMFILE", err)
	}
	if opened {
		t.Error("attempted pidfd_open for an already observed zombie")
	}
}

func TestSignalChildChecksZombieAfterPidfdOpen(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exec sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	realBefore, realSignal := beforeSignalChildPidfdOpen, signalProcess
	beforeSignalChildPidfdOpen = func(pid int) {
		if pid != cmd.Process.Pid {
			return
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Errorf("kill test child to make a zombie: %v", err)
			return
		}
		waitForZombie(t, pid)
	}
	signalled := false
	signalProcess = func(_ int, _ *trackedProcess, _ unix.Signal) error {
		signalled = true
		return nil
	}
	t.Cleanup(func() {
		beforeSignalChildPidfdOpen, signalProcess = realBefore, realSignal
	})
	err := SignalChild(cmd.Process.Pid, syscall.SIGTERM)
	if !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("SignalChild after child became zombie = %v, want os.ErrProcessDone", err)
	}
	if signalled {
		t.Error("SignalChild sent a signal after the child became a zombie")
	}
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("reap test child: %v", err)
		}
	}
}

func readSignalChildTestPID(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		t.Fatalf("invalid PID in %s: %q (%v)", path, b, err)
	}
	return pid
}
