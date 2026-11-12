//go:build linux

package proc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSignalCmdFallsBackWhenPidfdIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	marker, ready := dir+"/term", dir+"/ready"
	cmd := exec.Command("sh", "-c", `trap 'echo term > "$1"; exit 0' TERM; echo ready > "$2"; while :; do :; done`, "sh", marker, ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitReady(t, ready)
	oldOpen := openProcessPidfd
	openProcessPidfd = func(int, int) (int, error) { return -1, unix.EPERM }
	result, err := SignalCmdWithResult(cmd, syscall.SIGTERM)
	openProcessPidfd = oldOpen
	if err != nil || !result.Degraded {
		t.Fatalf("SignalCmdWithResult = (%+v, %v), want degraded success", result, err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child Wait: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("direct child did not receive SIGTERM: %v", err)
	}
}

func TestStopCmdFallsBackToDirectChildWhenPidfdIsUnavailable(t *testing.T) {
	cmd := exec.Command("sh", "-c", `trap '' TERM; while :; do :; done`)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	oldOpen := openProcessPidfd
	openProcessPidfd = func(int, int) (int, error) { return -1, unix.ENOSYS }
	started := time.Now()
	result, err := StopCmd(cmd, 40*time.Millisecond, syscall.SIGTERM)
	openProcessPidfd = oldOpen
	if err != nil || !result.Degraded || result.Killed != 1 {
		t.Fatalf("StopCmd = (%+v, %v), want degraded direct-child kill", result, err)
	}
	if time.Since(started) < 35*time.Millisecond {
		t.Fatalf("StopCmd waited only %s, want grace period", time.Since(started))
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("Wait succeeded after SIGKILL")
	}
}

func TestStopCmdPidfdFallbackLeavesGroupPeersAlive(t *testing.T) {
	cmd := exec.Command("sh", "-c", `trap '' TERM; while :; do :; done`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	peer := exec.Command("sleep", "30")
	peer.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: cmd.Process.Pid}
	if err := peer.Start(); err != nil {
		_ = SignalChild(cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if peer.ProcessState == nil {
			_ = SignalChild(peer.Process.Pid, syscall.SIGKILL)
			_ = peer.Wait()
		}
	})
	oldOpen := openProcessPidfd
	openProcessPidfd = func(int, int) (int, error) { return -1, unix.EPERM }
	result, err := StopCmd(cmd, 20*time.Millisecond, syscall.SIGTERM)
	openProcessPidfd = oldOpen
	if err != nil || !result.Degraded || result.Killed != 1 {
		t.Fatalf("StopCmd = (%+v, %v), want degraded direct-child kill", result, err)
	}
	if st, err := ReadStat("/proc", peer.Process.Pid); err != nil || st.State == "Z" || st.State == "X" {
		t.Fatalf("group peer was affected by direct fallback: stat=%+v err=%v", st, err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("Wait succeeded after SIGKILL")
	}
}

func TestSignalCmdAfterWaitReturnsProcessDoneWithoutSignal(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	oldSignal := directProcessSignal
	directProcessSignal = func(*os.Process, syscall.Signal) error { calls++; return nil }
	t.Cleanup(func() { directProcessSignal = oldSignal })
	if err := SignalCmd(cmd, syscall.SIGTERM); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("SignalCmd after Wait error = %v, want os.ErrProcessDone", err)
	}
	if calls != 0 {
		t.Fatalf("signal calls after Wait = %d, want 0", calls)
	}
}

func TestStopCmdAfterWaitReturnsProcessDoneWithoutSignal(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	oldOpen, oldSignal := openProcessPidfd, directProcessSignal
	opens, signals := 0, 0
	openProcessPidfd = func(int, int) (int, error) { opens++; return -1, unix.EPERM }
	directProcessSignal = func(*os.Process, syscall.Signal) error { signals++; return nil }
	t.Cleanup(func() { openProcessPidfd, directProcessSignal = oldOpen, oldSignal })
	if _, err := StopCmd(cmd, 0, syscall.SIGTERM); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("StopCmd after Wait error = %v, want os.ErrProcessDone", err)
	}
	if opens != 0 || signals != 0 {
		t.Fatalf("pidfd opens=%d signals=%d after Wait; want 0", opens, signals)
	}
}

func TestSignalCmdRefusesInvalidTargetWithoutSignaling(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.ProcessState = nil
		_ = SignalChild(cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	oldSignal := directProcessSignal
	calls := 0
	directProcessSignal = func(*os.Process, syscall.Signal) error { calls++; return nil }
	t.Cleanup(func() { directProcessSignal = oldSignal })
	cmd.ProcessState = &os.ProcessState{}
	if err := SignalCmd(cmd, syscall.SIGTERM); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("ProcessState refusal = %v, want os.ErrProcessDone", err)
	}
	cmd.ProcessState = nil
	if _, err := validateCmdSignal(cmd, 0); err == nil {
		t.Fatal("validateCmdSignal accepted signal zero")
	}
	if _, err := validateCmdSignal(cmd, 65); err == nil {
		t.Fatal("validateCmdSignal accepted signal 65")
	}
	for _, tc := range []struct {
		cmd *exec.Cmd
		sig syscall.Signal
	}{
		{cmd: nil, sig: syscall.SIGTERM},
		{cmd: &exec.Cmd{}, sig: syscall.SIGTERM},
		{cmd: cmd, sig: 0},
		{cmd: cmd, sig: 65},
	} {
		if err := SignalCmd(tc.cmd, tc.sig); err == nil {
			t.Fatalf("SignalCmd(%v, %d) unexpectedly succeeded", tc.cmd, tc.sig)
		}
	}
	if calls != 0 {
		t.Fatalf("direct signal calls = %d, want 0", calls)
	}
}

func TestSignalCmdAndStopCmdRejectUnsafeAndForeignPIDs(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	original := cmd.Process
	t.Cleanup(func() {
		cmd.Process = original
		_ = SignalChild(original.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	oldSignal, oldKill := directProcessSignal, directProcessKill
	signals, kills := 0, 0
	directProcessSignal = func(*os.Process, syscall.Signal) error { signals++; return nil }
	directProcessKill = func(*os.Process) error { kills++; return nil }
	t.Cleanup(func() { directProcessSignal, directProcessKill = oldSignal, oldKill })
	for _, pid := range []int{-1, 0, -2, os.Getpid(), os.Getppid(), 1} {
		cmd.Process = &os.Process{Pid: pid}
		if err := SignalCmd(cmd, syscall.SIGTERM); err == nil {
			t.Errorf("SignalCmd accepted PID %d", pid)
		}
		if _, err := StopCmd(cmd, 0, syscall.SIGTERM); err == nil {
			t.Errorf("StopCmd accepted PID %d", pid)
		}
	}
	cmd.Process = original
	if signals != 0 || kills != 0 {
		t.Fatalf("signal calls=%d kill calls=%d, want 0", signals, kills)
	}
}

func TestValidateLinuxCmdChildRequiresCurrentProcessAsParent(t *testing.T) {
	root := t.TempDir()
	fakeStat(t, root, 1, 0, "init", "S")
	fakeStat(t, root, 20, os.Getpid(), "owned-child", "S")
	fakeStat(t, root, 21, 19, "foreign-process", "S")
	oldRoot := cmdProcRoot
	cmdProcRoot = root
	t.Cleanup(func() { cmdProcRoot = oldRoot })
	if err := validateLinuxCmdChild(20); err != nil {
		t.Fatalf("direct child validation = %v", err)
	}
	if err := validateLinuxCmdChild(21); err == nil {
		t.Fatal("accepted positive PID whose PPID is not this process")
	}
}

func TestValidateCmdPIDRejectsUnsafeNumbers(t *testing.T) {
	for _, pid := range []int{-1, 0, -2, os.Getpid(), os.Getppid()} {
		if err := validateCmdPID(pid); err == nil {
			t.Errorf("validateCmdPID(%d) accepted unsafe PID", pid)
		}
	}
}

func TestCancelByStopReturnsDegradedErrorThroughWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := t.TempDir() + "/ready"
	cmd := exec.CommandContext(ctx, "sh", "-c", `trap 'exit 0' TERM; echo ready > "$1"; while :; do :; done`, "sh", ready)
	var observedDegraded bool
	var observedCancelErr error
	if err := installCancelByStopWithObserver(cmd, 0, syscall.SIGTERM, func(cmd *exec.Cmd, _ time.Duration, sig syscall.Signal) (StopResult, error) {
		_ = cmd.Process.Signal(sig)
		return StopResult{Degraded: true}, nil
	}, func(degraded bool, err error) { observedDegraded, observedCancelErr = degraded, err }); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitReady(t, ready)
	cancel()
	err := cmd.Wait()
	if !errors.Is(err, ErrPidfdUnavailable) {
		t.Fatalf("Wait error = %v, want ErrPidfdUnavailable", err)
	}
	if !errors.Is(observedCancelErr, ErrPidfdUnavailable) {
		t.Fatalf("observer error = %v, want ErrPidfdUnavailable", observedCancelErr)
	}
	if !observedDegraded {
		t.Fatal("observer degradation flag = false, want true")
	}
	if ctx.Err() == nil {
		t.Fatal("context cancellation was not recorded")
	}
}

func TestCancelByStopKilledErrorMatchesDefaultCancel(t *testing.T) {
	run := func(custom bool) (error, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", `trap '' TERM; while :; do :; done`)
		if custom {
			if err := CancelByStop(cmd, 30*time.Millisecond, syscall.SIGTERM); err != nil {
				return nil, err
			}
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmd.Wait(), ctx.Err()
	}
	defaultErr, defaultContextErr := run(false)
	customErr, customContextErr := run(true)
	if defaultContextErr == nil || customContextErr == nil {
		t.Fatalf("context errors: default=%v custom=%v", defaultContextErr, customContextErr)
	}
	var defaultExit, customExit *exec.ExitError
	if !errors.As(defaultErr, &defaultExit) || !errors.As(customErr, &customExit) {
		t.Fatalf("Wait errors: default=%T %v custom=%T %v; want ExitError in both", defaultErr, defaultErr, customErr, customErr)
	}
	if defaultErr.Error() != customErr.Error() {
		t.Fatalf("Wait error differs from default Cancel: default=%q custom=%q", defaultErr, customErr)
	}
}

func TestStopCmdDirectWaitsGraceAndKills(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	oldSignal, oldKill := directProcessSignal, directProcessKill
	defer func() { directProcessSignal, directProcessKill = oldSignal, oldKill }()
	signaled, killed := false, false
	directProcessSignal = func(_ *os.Process, sig syscall.Signal) error {
		signaled = sig == syscall.SIGTERM
		return nil
	}
	directProcessKill = func(process *os.Process) error {
		killed = true
		return process.Kill()
	}
	started := time.Now()
	result, err := stopCmdDirect(cmd, 40*time.Millisecond, syscall.SIGTERM, false)
	if err != nil || result.Degraded || result.Killed != 1 {
		t.Fatalf("stopCmdDirect = (%+v, %v)", result, err)
	}
	if !signaled || !killed || time.Since(started) < 35*time.Millisecond {
		t.Fatalf("signal=%v kill=%v elapsed=%s; want signal, grace, kill", signaled, killed, time.Since(started))
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("Wait succeeded after direct Kill")
	}
}

func TestCancelByStopDirectHandlerInstallsAndStopsChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	marker, ready := t.TempDir()+"/term", t.TempDir()+"/ready"
	cmd := exec.CommandContext(ctx, "sh", "-c", `trap 'echo term > "$1"; exit 0' TERM; echo ready > "$2"; while :; do :; done`, "sh", marker, ready)
	called := 0
	if err := installCancelByStop(cmd, 30*time.Millisecond, syscall.SIGTERM, func(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) (StopResult, error) {
		called++
		return stopCmdDirectState(cmd, grace, sig, false, false)
	}); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitReady(t, ready)
	cancel()
	if err := cmd.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context.Canceled", err)
	}
	if called != 1 {
		t.Fatalf("installed stop calls = %d, want 1", called)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("direct child did not receive signal: %v", err)
	}
}

func TestPidfdUnavailableRefusesVerifiedHelpers(t *testing.T) {
	tests := []struct {
		name string
		err  error
		run  func(*exec.Cmd) error
	}{
		{name: "descendants", err: unix.EPERM, run: func(*exec.Cmd) error {
			_, err := StopDescendants(0, syscall.SIGTERM)
			return err
		}},
		{name: "session", err: unix.ENOSYS, run: func(cmd *exec.Cmd) error {
			rec := sessionRecordFor(t, cmd.Process.Pid)
			_, err := StopSession(rec, 0, syscall.SIGTERM)
			return err
		}},
		{name: "descendant signal", err: unix.EPERM, run: func(cmd *exec.Cmd) error {
			return SignalDescendant(cmd.Process.Pid, syscall.SIGTERM)
		}},
		{name: "own unit", err: unix.ENOSYS, run: func(*exec.Cmd) error {
			_, err := SignalOwnUnit(syscall.SIGTERM)
			return err
		}},
		{name: "process group", err: unix.EPERM, run: func(cmd *exec.Cmd) error {
			_, err := StopChildGroup(cmd, 0, syscall.SIGTERM)
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sleep", "30")
			if tc.name == "session" {
				cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			}
			if tc.name == "process group" {
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = SignalChild(cmd.Process.Pid, syscall.SIGKILL)
					_ = cmd.Wait()
				}
			})
			oldOpen := openProcessPidfd
			defer func() { openProcessPidfd = oldOpen }()
			openProcessPidfd = func(int, int) (int, error) { return -1, tc.err }
			err := tc.run(cmd)
			if !errors.Is(err, ErrPidfdUnavailable) {
				t.Fatalf("error = %v, want ErrPidfdUnavailable", err)
			}
			if stat, err := ReadStat("/proc", cmd.Process.Pid); err != nil || stat.State == "Z" || stat.State == "X" {
				t.Fatalf("refusal affected own process: stat=%+v err=%v", stat, err)
			}
		})
	}
}
