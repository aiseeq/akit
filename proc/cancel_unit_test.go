//go:build linux

package proc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCancelByStopContextKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	marker, childFile := filepath.Join(dir, "term"), filepath.Join(dir, "child")
	cmd := exec.CommandContext(ctx, "sh", "-c", `trap 'echo term > "$1"; exit 0' TERM; sleep 30 & echo $! > "$2"; wait`, "sh", marker, childFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	if err := CancelByStop(cmd, 0, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		if _, err := os.Stat(childFile); err == nil {
			ready = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ready {
		t.Fatal("child process did not start")
	}
	cancel()
	err := cmd.Wait()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("child did not receive SIGTERM: %v", err)
	}
	b, err := os.ReadFile(childFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if err := waitTestProcessGone(pid, 2*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestCancelByStopWithoutCancellationDoesNotSignal(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "exit 0")
	if err := CancelByStop(cmd, 100*time.Millisecond, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}

func TestCancelByStopUsesDirectChildForOrdinaryCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.CommandContext(ctx, "sh", "-c", `trap 'exit 0' TERM; echo ready > "$1"; while :; do :; done`, "sh", ready)
	if err := CancelByStop(cmd, 100*time.Millisecond, syscall.SIGTERM); err != nil {
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
}

func TestCancelByStopGroupIncludesGroupPeer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := CancelByStop(cmd, 0, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	peer := exec.Command("sleep", "30")
	peer.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: cmd.Process.Pid}
	if err := peer.Start(); err != nil {
		_, _ = StopChildGroup(cmd, 0, syscall.SIGKILL)
		_ = cmd.Wait()
		t.Fatal(err)
	}
	cancel()
	err := cmd.Wait()
	if !errors.Is(err, context.Canceled) {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("Wait error = %v, want context cancellation or process exit", err)
		}
	}
	if err := waitTestProcessGone(peer.Process.Pid, 2*time.Second); err != nil {
		if signalErr := SignalChild(peer.Process.Pid, syscall.SIGKILL); signalErr == nil {
			_ = peer.Wait()
		}
		t.Fatal(err)
	}
}

func TestCancelByStopTreatsNonzeroPgidAsDirectChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leader := exec.Command("sleep", "30")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if leader.ProcessState == nil {
			_ = SignalChild(leader.Process.Pid, syscall.SIGKILL)
			_ = leader.Wait()
		}
	})
	cmd := exec.CommandContext(ctx, "sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.Process.Pid}
	if err := CancelByStop(cmd, 0, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	peer := exec.Command("sleep", "30")
	peer.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.Process.Pid}
	if err := peer.Start(); err != nil {
		_ = SignalChild(cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		t.Fatal(err)
	}
	oldGroup, oldSingle := stopCmdGroup, stopCmdSingle
	groupCalls, singleCalls := 0, 0
	stopCmdGroup = func(cmd *exec.Cmd, _ time.Duration, sig syscall.Signal) (StopResult, error) {
		groupCalls++
		return StopResult{}, SignalCmd(cmd, sig)
	}
	stopCmdSingle = func(_ int, _ time.Duration, request func() error) (StopResult, error) {
		singleCalls++
		return StopResult{}, request()
	}
	defer func() { stopCmdGroup, stopCmdSingle = oldGroup, oldSingle }()
	t.Cleanup(func() {
		if peer.ProcessState == nil {
			_ = SignalChild(peer.Process.Pid, syscall.SIGKILL)
			_ = peer.Wait()
		}
	})
	cancel()
	if err := cmd.Wait(); !errors.Is(err, context.Canceled) {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("Wait error = %v", err)
		}
	}
	if groupCalls != 0 || singleCalls != 1 {
		t.Fatalf("StopCmd route = group:%d single:%d; want direct child only", groupCalls, singleCalls)
	}
	if st, err := ReadStat("/proc", peer.Process.Pid); err != nil || st.State == "Z" || st.State == "X" {
		t.Fatalf("nonzero-Pgid peer was stopped: stat=%+v err=%v", st, err)
	}
}

func TestSignalOwnUnitRejectsUnsafeCgroups(t *testing.T) {
	for _, cg := range []string{"/user.slice/session-1.scope", "/user.slice/user@1000.service", "/init.scope", "/system.slice/foo.slice"} {
		t.Run(strings.ReplaceAll(cg, "/", "_"), func(t *testing.T) {
			procRoot, cgRoot := t.TempDir(), t.TempDir()
			fakeStat(t, procRoot, 10, 1, "systemd", "S")
			fakeStat(t, procRoot, 20, 10, "worker", "S")
			writeTestFile(t, filepath.Join(procRoot, "self", "cgroup"), "0::"+cg+"\n")
			oldEnv := ownUnitEnv
			oldMainPID := ownUnitMainPID
			ownUnitEnv = func(key string) string {
				if key == "INVOCATION_ID" {
					return "id"
				}
				return ""
			}
			ownUnitMainPID = func(context.Context, bool, string) (int, error) { return 20, nil }
			t.Cleanup(func() { ownUnitEnv, ownUnitMainPID = oldEnv, oldMainPID })
			calls := 0
			oldSignal := ownUnitSignal
			ownUnitSignal = func(int, *trackedProcess, unix.Signal) error { calls++; return nil }
			t.Cleanup(func() { ownUnitSignal = oldSignal })
			if _, err := signalOwnUnit(context.Background(), procRoot, cgRoot, 20, 10, syscall.SIGTERM); !errors.Is(err, ErrUnsafeUnitCgroup) {
				t.Fatalf("unsafe cgroup error = %v, want ErrUnsafeUnitCgroup", err)
			}
			if calls != 0 {
				t.Fatalf("sent signals on rejection: %d", calls)
			}
		})
	}
}

func waitTestProcessGone(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := ReadStat("/proc", pid)
		if Gone(err) || err == nil && (st.State == "Z" || st.State == "X") {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("test child process remained alive")
}

func TestSignalOwnUnitRejectsNonMainProcess(t *testing.T) {
	procRoot, cgRoot := t.TempDir(), t.TempDir()
	fakeStat(t, procRoot, 10, 1, "systemd", "S")
	fakeStat(t, procRoot, 20, 99, "worker", "S")
	writeTestFile(t, filepath.Join(procRoot, "self", "cgroup"), "0::/system.slice/worker.service\n")
	oldEnv := ownUnitEnv
	oldMainPID := ownUnitMainPID
	ownUnitEnv = func(string) string { return "id" }
	ownUnitMainPID = func(context.Context, bool, string) (int, error) { return 20, nil }
	t.Cleanup(func() { ownUnitEnv, ownUnitMainPID = oldEnv, oldMainPID })
	if _, err := signalOwnUnit(context.Background(), procRoot, cgRoot, 20, 10, syscall.SIGTERM); !errors.Is(err, ErrNotServiceMainProcess) {
		t.Fatalf("non-main process error = %v, want ErrNotServiceMainProcess", err)
	}
}

func TestSignalOwnUnitRequiresInvocationAndSystemdMainPID(t *testing.T) {
	procRoot, cgRoot := t.TempDir(), t.TempDir()
	fakeStat(t, procRoot, 10, 1, "systemd", "S")
	fakeStat(t, procRoot, 20, 10, "worker", "S")
	writeTestFile(t, filepath.Join(procRoot, "self", "cgroup"), "0::/system.slice/worker.service\n")
	oldEnv, oldMainPID := ownUnitEnv, ownUnitMainPID
	t.Cleanup(func() { ownUnitEnv, ownUnitMainPID = oldEnv, oldMainPID })
	ownUnitEnv = func(string) string { return "" }
	if _, err := signalOwnUnit(context.Background(), procRoot, cgRoot, 20, 10, syscall.SIGTERM); !errors.Is(err, ErrNoInvocationID) {
		t.Fatalf("missing invocation error = %v, want ErrNoInvocationID", err)
	}
	ownUnitEnv = func(string) string { return "invocation" }
	ownUnitMainPID = func(context.Context, bool, string) (int, error) { return 21, nil }
	if _, err := signalOwnUnit(context.Background(), procRoot, cgRoot, 20, 10, syscall.SIGTERM); !errors.Is(err, ErrNotUnitMainPID) {
		t.Fatalf("MainPID mismatch error = %v, want ErrNotUnitMainPID", err)
	}
	ownUnitMainPID = func(context.Context, bool, string) (int, error) { return 0, errors.New("systemctl failed") }
	if _, err := signalOwnUnit(context.Background(), procRoot, cgRoot, 20, 10, syscall.SIGTERM); !errors.Is(err, ErrNotUnitMainPID) {
		t.Fatalf("MainPID query error = %v, want ErrNotUnitMainPID", err)
	}
}

func TestSignalOwnUnitRequiresSystemdParent(t *testing.T) {
	procRoot, cgRoot := t.TempDir(), t.TempDir()
	fakeStat(t, procRoot, 10, 1, "ordinary-parent", "S")
	fakeStat(t, procRoot, 20, 10, "worker", "S")
	writeTestFile(t, filepath.Join(procRoot, "self", "cgroup"), "0::/system.slice/worker.service\n")
	oldEnv, oldMainPID := ownUnitEnv, ownUnitMainPID
	ownUnitEnv = func(string) string { return "invocation" }
	ownUnitMainPID = func(context.Context, bool, string) (int, error) { return 20, nil }
	t.Cleanup(func() { ownUnitEnv, ownUnitMainPID = oldEnv, oldMainPID })
	if _, err := signalOwnUnit(context.Background(), procRoot, cgRoot, 20, 10, syscall.SIGTERM); !errors.Is(err, ErrNotSystemdParent) {
		t.Fatalf("parent check error = %v, want ErrNotSystemdParent", err)
	}
}

func TestOwnServiceCgroupSelectsSystemdManagerForUnitPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		cg   string
		user bool
	}{
		{name: "system", cg: "/system.slice/worker.service"},
		{name: "user", cg: "/user.slice/user-1000.slice/user@1000.service/app.slice/worker.service", user: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fakeStat(t, root, 10, 1, "systemd", "S")
			fakeStat(t, root, 20, 10, "worker", "S")
			writeTestFile(t, filepath.Join(root, "self", "cgroup"), "0::"+tc.cg+"\n")
			oldEnv, oldMainPID := ownUnitEnv, ownUnitMainPID
			ownUnitEnv = func(string) string { return "invocation" }
			called := false
			ownUnitMainPID = func(_ context.Context, user bool, unit string) (int, error) {
				called = true
				if user != tc.user || unit != "worker.service" {
					t.Fatalf("MainPID query = (user=%v, unit=%q), want (user=%v, worker.service)", user, unit, tc.user)
				}
				return 20, nil
			}
			t.Cleanup(func() { ownUnitEnv, ownUnitMainPID = oldEnv, oldMainPID })
			if _, err := ownServiceCgroup(context.Background(), root, 20, 10); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("systemd MainPID was not queried")
			}
		})
	}
}

func TestSignalOwnUnitDoesNotSignalParent(t *testing.T) {
	procRoot, cgRoot := t.TempDir(), t.TempDir()
	cg := "/system.slice/worker.service"
	fakeStat(t, procRoot, 10, 1, "systemd", "S")
	fakeStat(t, procRoot, 20, 10, "worker", "S")
	writeTestFile(t, filepath.Join(procRoot, "self", "cgroup"), "0::"+cg+"\n")
	writeTestFile(t, filepath.Join(procRoot, "10", "cgroup"), "0::"+cg+"\n")
	writeTestFile(t, filepath.Join(cgRoot, cg, "cgroup.procs"), "10\n20\n")
	oldEnv, oldMainPID, oldSignal, oldOpen := ownUnitEnv, ownUnitMainPID, ownUnitSignal, ownUnitPidfdOpen
	ownUnitEnv = func(string) string { return "invocation" }
	ownUnitMainPID = func(context.Context, bool, string) (int, error) { return 20, nil }
	readFD, writeFD, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readFD.Close()
	defer writeFD.Close()
	ownUnitPidfdOpen = func(int, int) (int, error) { return int(readFD.Fd()), nil }
	calls := 0
	ownUnitSignal = func(pid int, _ *trackedProcess, _ unix.Signal) error {
		calls++
		if pid == 10 {
			t.Fatalf("signaled service parent PID %d", pid)
		}
		return nil
	}
	t.Cleanup(func() {
		ownUnitEnv, ownUnitMainPID, ownUnitSignal, ownUnitPidfdOpen = oldEnv, oldMainPID, oldSignal, oldOpen
	})
	if _, err := signalOwnUnit(context.Background(), procRoot, cgRoot, 20, 10, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("signal calls = %d, want 0 after excluding parent and self", calls)
	}
}

func TestSignalOwnUnitDetectsStartReplacementAfterPidfdOpen(t *testing.T) {
	procRoot, cgRoot := t.TempDir(), t.TempDir()
	cg := "/system.slice/worker.service"
	fakeStat(t, procRoot, 10, 1, "systemd", "S")
	fakeStat(t, procRoot, 20, 10, "worker", "S")
	fakeStat(t, procRoot, 30, 20, "worker-child", "S")
	writeTestFile(t, filepath.Join(procRoot, "self", "cgroup"), "0::"+cg+"\n")
	writeTestFile(t, filepath.Join(procRoot, "30", "cgroup"), "0::"+cg+"\n")
	writeTestFile(t, filepath.Join(cgRoot, cg, "cgroup.procs"), "30\n")
	oldEnv, oldMainPID, oldOpen, oldSignal, oldAfter := ownUnitEnv, ownUnitMainPID, ownUnitPidfdOpen, ownUnitSignal, ownUnitAfterPidfdOpen
	ownUnitEnv = func(string) string { return "invocation" }
	ownUnitMainPID = func(context.Context, bool, string) (int, error) { return 20, nil }
	readFD, writeFD, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readFD.Close()
	defer writeFD.Close()
	ownUnitPidfdOpen = func(int, int) (int, error) { return int(readFD.Fd()), nil }
	ownUnitAfterPidfdOpen = func(pid int) { fakeStatStart(t, procRoot, pid, 20, "worker-child", "changed-start") }
	calls := 0
	ownUnitSignal = func(int, *trackedProcess, unix.Signal) error { calls++; return nil }
	t.Cleanup(func() {
		ownUnitEnv, ownUnitMainPID, ownUnitPidfdOpen, ownUnitSignal, ownUnitAfterPidfdOpen = oldEnv, oldMainPID, oldOpen, oldSignal, oldAfter
	})
	count, err := signalOwnUnit(context.Background(), procRoot, cgRoot, 20, 10, syscall.SIGTERM)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 || calls != 0 {
		t.Fatalf("replacement process was signaled: count=%d calls=%d", count, calls)
	}
}

func fakeStatStart(t *testing.T, root string, pid, parent int, name, start string) {
	t.Helper()
	fakeStat(t, root, pid, parent, name, "S")
	path := filepath.Join(root, strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data))
	fields[len(fields)-1] = start
	if err := os.WriteFile(path, []byte(strings.Join(fields, " ")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSignalOwnUnitRechecksCgroupAfterPidfdOpen(t *testing.T) {
	procRoot, cgRoot := t.TempDir(), t.TempDir()
	cg := "/system.slice/worker.service"
	fakeStat(t, procRoot, 10, 1, "systemd", "S")
	fakeStat(t, procRoot, 20, 10, "worker", "S")
	fakeStat(t, procRoot, 30, 20, "worker-child", "S")
	writeTestFile(t, filepath.Join(procRoot, "self", "cgroup"), "0::"+cg+"\n")
	writeTestFile(t, filepath.Join(procRoot, "30", "cgroup"), "0::"+cg+"\n")
	writeTestFile(t, filepath.Join(cgRoot, cg, "cgroup.procs"), "20\n30\n")
	oldEnv, oldOpen, oldSignal, oldAfter, oldMainPID := ownUnitEnv, ownUnitPidfdOpen, ownUnitSignal, ownUnitAfterPidfdOpen, ownUnitMainPID
	ownUnitEnv = func(string) string { return "id" }
	ownUnitMainPID = func(context.Context, bool, string) (int, error) { return 20, nil }
	readFD, writeFD, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readFD.Close()
	defer writeFD.Close()
	ownUnitPidfdOpen = func(int, int) (int, error) { return int(readFD.Fd()), nil }
	ownUnitAfterPidfdOpen = func(pid int) {
		writeTestFile(t, filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"), "0::/system.slice/other.service\n")
	}
	calls := 0
	ownUnitSignal = func(int, *trackedProcess, unix.Signal) error { calls++; return nil }
	t.Cleanup(func() {
		ownUnitEnv, ownUnitPidfdOpen, ownUnitSignal, ownUnitAfterPidfdOpen, ownUnitMainPID = oldEnv, oldOpen, oldSignal, oldAfter, oldMainPID
	})
	count, err := signalOwnUnit(context.Background(), procRoot, cgRoot, 20, 10, syscall.SIGTERM)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 || calls != 0 {
		t.Fatalf("changed cgroup received signal: count=%d calls=%d", count, calls)
	}
}

func TestSignalOwnUnitLiveService(t *testing.T) {
	if os.Getenv("AKIT_LIVE_SYSTEMD_HELPER") == "1" {
		child := exec.Command("sleep", "30")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		count, err := SignalOwnUnit(syscall.SIGTERM)
		if err != nil || count != 1 {
			if signalErr := SignalChild(child.Process.Pid, syscall.SIGKILL); signalErr == nil {
				_ = child.Wait()
			}
			t.Fatalf("SignalOwnUnit = (%d, %v), want (1, nil)", count, err)
		}
		err = child.Wait()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("unit child Wait = %v, want SIGTERM", err)
		}
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
			t.Fatalf("unit child Wait status = %v, want SIGTERM", exitErr.Sys())
		}
		if err := os.WriteFile(os.Getenv("AKIT_LIVE_SYSTEMD_MARKER"), []byte("ran\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if os.Getenv("AKIT_LIVE_SYSTEMD_TEST") != "1" {
		t.Skip("set AKIT_LIVE_SYSTEMD_TEST=1 to run the disposable systemd user service")
	}
	systemdRun, err := exec.LookPath("systemd-run")
	if err != nil {
		t.Skipf("systemd-run unavailable: %v", err)
	}
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		t.Skipf("systemctl unavailable: %v", err)
	}
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		t.Skip("XDG_RUNTIME_DIR is unset; no user systemd manager is available")
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, "systemd", "private")); err != nil {
		t.Skipf("user systemd manager socket unavailable: %v", err)
	}
	manager := exec.Command(systemctl, "--user", "show-environment")
	if output, err := manager.CombinedOutput(); err != nil {
		t.Fatalf("query available user systemd manager: %v: %s", err, strings.TrimSpace(string(output)))
	}
	base := fmt.Sprintf("akit-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	marker := filepath.Join(t.TempDir(), "helper-ran")
	unit := exec.CommandContext(ctx, systemdRun, "--user", "--unit", base+".service", "--wait", "--collect", "--setenv=AKIT_LIVE_SYSTEMD_HELPER=1", "--setenv=AKIT_LIVE_SYSTEMD_MARKER="+marker, "--", os.Args[0], "-test.run=^TestSignalOwnUnitLiveService$")
	if output, err := unit.CombinedOutput(); err != nil {
		t.Fatalf("temporary systemd service failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("systemd helper did not run: %v", err)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}
