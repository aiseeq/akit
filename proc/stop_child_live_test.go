//go:build linux

package proc

import (
	"context"
	"errors"
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

func TestMain(m *testing.M) {
	if os.Getenv("PROC_DISABLE_TEST_SUBREAPER") != "1" {
		if err := BecomeChildSubreaper(); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}

// StopChild on a real child: a child that exits on the request needs no
// SIGKILL; one that ignores it is killed after the grace.
func TestStopChildLive(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		killed bool
	}{
		{"exits on request", "echo > \"$1\"; exec sleep 30", false},
		{"ignores request", "trap '' TERM; echo > \"$1\"; while :; do sleep 0.05; done", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "ready")
			cmd := exec.Command("sh", "-c", tc.script, "sh", ready)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waitReady(t, ready)
			started := time.Now()
			result, err := StopChild(cmd.Process.Pid, 300*time.Millisecond, func() error {
				return SignalChild(cmd.Process.Pid, syscall.SIGTERM)
			})
			_ = cmd.Wait()
			if err != nil {
				t.Fatal(err)
			}
			if got := result.Killed > 0; got != tc.killed {
				t.Errorf("killed %d, want killed=%v", result.Killed, tc.killed)
			}
			if tc.killed && time.Since(started) < 300*time.Millisecond {
				t.Errorf("killed before the grace: %v", time.Since(started))
			}
		})
	}
}

func TestStopChildKillsReparentedGroupMember(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "background.pid")
	cmd := exec.Command("sh", "-c", `sleep 30 & printf '%s\n' "$!" > "$1.tmp"; mv "$1.tmp" "$1"; exit 0`, "sh", pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
			t.Errorf("stop child group: %v", err)
		}
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	})
	waitReady(t, pidFile)
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		stat, statErr := ReadStat("/proc", cmd.Process.Pid)
		if statErr == nil && stat.State == "Z" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("group leader did not exit before cleanup: %+v, %v", stat, statErr)
		}
		time.Sleep(time.Millisecond)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for {
		stat, err := ReadStat("/proc", grandchild)
		if err != nil || stat.State == "Z" || stat.State == "X" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reparented group member %d still alive: %s", grandchild, stat.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStopChildGroupKillsLiveGroup(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command("sh", "-c", "sleep 30 & echo ready > \"$1\"; while :; do sleep .05; done", "sh", ready)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
			t.Errorf("stop live child group: %v", err)
		}
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	})
	waitReady(t, ready)
	result, err := StopChildGroup(cmd, 0, syscall.SIGKILL)
	if err != nil {
		t.Fatal(err)
	}
	if result.AlreadyExited || result.Killed < 2 {
		t.Fatalf("result = %+v, want SIGKILL counted for the shell and its sleep", result)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("group leader exited successfully after SIGKILL")
	}
}

func TestStopChildGroupFallsBackWhenChildMovesToAnotherGroup(t *testing.T) {
	if os.Getenv("PROC_MOVE_PGID_HELPER") == "1" {
		pgid, _ := strconv.Atoi(os.Getenv("PROC_MOVE_PGID_TARGET"))
		if err := syscall.Setpgid(0, pgid); err != nil {
			os.Exit(3)
		}
		if err := os.WriteFile(os.Getenv("PROC_MOVE_PGID_READY"), []byte("ready"), 0o600); err != nil {
			os.Exit(4)
		}
		time.Sleep(time.Hour)
	}
	sibling := exec.Command("sleep", "30")
	sibling.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sibling.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sibling.ProcessState == nil {
			if _, err := StopChild(sibling.Process.Pid, 0, nil); err != nil {
				t.Errorf("stop sibling group leader: %v", err)
			}
			_ = sibling.Wait()
		}
	})
	ready := filepath.Join(t.TempDir(), "ready")
	mover := exec.Command(os.Args[0], "-test.run=^TestStopChildGroupFallsBackWhenChildMovesToAnotherGroup$")
	mover.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	mover.Env = append(os.Environ(), "PROC_MOVE_PGID_HELPER=1", "PROC_MOVE_PGID_TARGET="+strconv.Itoa(sibling.Process.Pid), "PROC_MOVE_PGID_READY="+ready)
	if err := mover.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if mover.ProcessState == nil {
			if _, err := StopChild(mover.Process.Pid, 0, nil); err != nil {
				t.Errorf("stop moved child: %v", err)
			}
			_ = mover.Wait()
		}
	})
	waitReady(t, ready)
	stat, err := ReadStat("/proc", mover.Process.Pid)
	if err != nil || stat.Pgid != sibling.Process.Pid {
		t.Fatalf("mover did not join sibling group: stat=%+v err=%v", stat, err)
	}
	realSignalProcess := signalProcess
	t.Cleanup(func() { signalProcess = realSignalProcess })
	directTerm := false
	signalProcess = func(pid int, process *trackedProcess, sig unix.Signal) error {
		if pid == mover.Process.Pid && sig == unix.SIGTERM {
			directTerm = true
		}
		return realSignalProcess(pid, process, sig)
	}
	result, err := StopChildGroup(mover, 100*time.Millisecond, syscall.SIGTERM)
	if err != nil {
		t.Fatal(err)
	}
	if result.AlreadyExited {
		t.Fatalf("live direct child was reported already exited: %+v", result)
	}
	if !directTerm {
		t.Fatalf("StopChildGroup did not request SIGTERM from the direct child: %+v", result)
	}
	if err := mover.Wait(); err == nil {
		t.Fatal("moved child survived requested SIGTERM")
	}
	if stat, err := ReadStat("/proc", sibling.Process.Pid); err != nil || stat.State == "Z" || stat.State == "X" {
		t.Fatalf("foreign sibling group was signaled: stat=%+v err=%v", stat, err)
	}
}

func TestStopChildGroupHonorsGraceAfterGroupRequest(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command("sh", "-c", "trap '' TERM; echo ready > \"$1\"; exec sleep 30", "sh", ready)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitReady(t, ready)
	started := time.Now()
	result, err := StopChildGroup(cmd, 200*time.Millisecond, syscall.SIGTERM)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("process survived SIGKILL after grace")
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond {
		t.Errorf("group stopped before grace elapsed: %v", elapsed)
	}
	if result.Killed == 0 {
		t.Errorf("SIGKILL did not reach the tracked child: %+v", result)
	}
}

func TestStopChildGroupRequestsTERMThenKillsReparentedGroupMember(t *testing.T) {
	for _, leader := range []string{"live", "zombie", "reaped"} {
		t.Run(leader, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "member.pid")
			ready := filepath.Join(t.TempDir(), "ready")
			script := `sh -c 'trap "" TERM; sleep 30 >/dev/null 2>&1 & echo $! > "$1"; wait' sh "$1" & while [ ! -s "$1" ]; do sleep .005; done; echo ready > "$2"; if [ "$3" = "live" ]; then exec sleep 30; fi; exit 0`
			cmd := exec.Command("sh", "-c", script, "sh", pidFile, ready, leader)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
					t.Errorf("clean process group: %v", err)
				}
				if cmd.ProcessState == nil {
					_ = cmd.Wait()
				}
			})
			waitReady(t, ready)
			member := readPID(t, pidFile)
			if leader != "live" {
				deadline := time.Now().Add(time.Second)
				for {
					stat, err := ReadStat("/proc", cmd.Process.Pid)
					if err == nil && stat.State == "Z" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("leader did not exit: %+v, %v", stat, err)
					}
					time.Sleep(time.Millisecond)
				}
				if leader == "reaped" {
					if err := cmd.Wait(); err != nil {
						t.Fatal(err)
					}
				}
			}

			real := signalProcess
			t.Cleanup(func() { signalProcess = real })
			var sent []unix.Signal
			signalProcess = func(pid int, target *trackedProcess, sig unix.Signal) error {
				sent = append(sent, sig)
				return real(pid, target, sig)
			}
			started := time.Now()
			_, err := StopChildGroup(cmd, 180*time.Millisecond, syscall.SIGTERM)
			if err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
				t.Fatalf("process group was killed before grace: %v", elapsed)
			}
			term, kill := false, false
			for _, sig := range sent {
				term = term || sig == unix.SIGTERM
				kill = kill || sig == unix.SIGKILL
			}
			if !term || !kill {
				t.Fatalf("group signals = %v, want TERM then KILL", sent)
			}
			deadline := time.Now().Add(time.Second)
			for {
				stat, err := ReadStat("/proc", member)
				if Gone(err) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("reparented member %d was not killed and reaped after TERM and SIGKILL: %+v, %v", member, stat, err)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

func TestStopChildGroupAcceptsSetsid(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("session leader survived SIGKILL")
	}
}

func TestSignalChildOnlySignalsDirectChild(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			if _, err := StopChild(cmd.Process.Pid, 0, nil); err != nil {
				t.Errorf("stop direct child: %v", err)
			}
			_ = cmd.Wait()
		}
	})
	if err := SignalChild(cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("child survived SIGTERM")
	}
	real := signalProcess
	t.Cleanup(func() { signalProcess = real })
	calls := 0
	signalProcess = func(_ int, _ *trackedProcess, _ unix.Signal) error {
		calls++
		return nil
	}
	for name, pid := range map[string]int{"self": os.Getpid(), "parent": os.Getppid()} {
		t.Run(name, func(t *testing.T) {
			if err := SignalChild(pid, syscall.SIGKILL); err == nil {
				t.Fatalf("SignalChild accepted %s PID %d", name, pid)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("signal function called for protected PID: %d times", calls)
	}
}

func TestSignalChildRejectsDescendant(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "grandchild.pid")
	a := exec.Command("sh", "-c", "sleep 30 & echo $! > \"$1\"; wait", "sh", pidPath)
	a.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := StopChildGroup(a, 0, syscall.SIGKILL); err != nil {
			t.Errorf("stop test child group: %v", err)
		}
		if a.ProcessState == nil {
			_ = a.Wait()
		}
	})
	grandchild := readPID(t, pidPath)
	real := signalProcess
	t.Cleanup(func() { signalProcess = real })
	calls := 0
	signalProcess = func(_ int, _ *trackedProcess, _ unix.Signal) error {
		calls++
		return nil
	}
	if err := SignalChild(grandchild, syscall.SIGKILL); err == nil {
		t.Fatalf("SignalChild accepted grandchild %d", grandchild)
	}
	if calls != 0 {
		t.Fatalf("signal function called for grandchild: %d times", calls)
	}
	if stat, err := ReadStat("/proc", grandchild); err != nil || stat.State == "Z" || stat.State == "X" {
		t.Fatalf("rejected signal nevertheless stopped grandchild: stat=%+v err=%v", stat, err)
	}
}

func TestSignalChildRevalidatesSubstitutedChildAfterPidfdOpen(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			if _, err := StopChild(cmd.Process.Pid, 0, nil); err != nil {
				t.Errorf("stop test child: %v", err)
			}
			_ = cmd.Wait()
		}
	})
	realRead, realBefore, realSignal := readSignalChildStat, beforeSignalChildPidfdOpen, signalProcess
	t.Cleanup(func() {
		readSignalChildStat, beforeSignalChildPidfdOpen, signalProcess = realRead, realBefore, realSignal
	})
	replacement := ""
	inPidfdWindow := false
	reads, signals := 0, 0
	readSignalChildStat = func(root string, pid int) (Stat, error) {
		stat, err := realRead(root, pid)
		reads++
		if inPidfdWindow && replacement != "" && pid == cmd.Process.Pid && err == nil && reads == 2 {
			switch replacement {
			case "start":
				stat.Start += "-reused"
			case "parent":
				stat.Ppid = os.Getppid()
			}
		}
		return stat, err
	}
	beforeSignalChildPidfdOpen = func(pid int) {
		if pid == cmd.Process.Pid {
			// Simulate the PID resolving to a different process after the first
			// /proc read but before pidfd_open.
			inPidfdWindow = true
		}
	}
	signalProcess = func(_ int, _ *trackedProcess, _ unix.Signal) error {
		signals++
		return nil
	}
	for _, replacementCase := range []string{"start", "parent"} {
		replacement = replacementCase
		inPidfdWindow = false
		reads = 0
		err := SignalChild(cmd.Process.Pid, syscall.SIGTERM)
		if err == nil {
			t.Fatalf("SignalChild accepted substituted %s identity", replacementCase)
		}
		if reads != 2 {
			t.Fatalf("read /proc %d times, want pre-open and post-open checks", reads)
		}
	}
	if signals != 0 {
		t.Fatalf("signaled substituted child %d times", signals)
	}
}

func TestSignalChildReapedReturnsProcessDone(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := SignalChild(cmd.Process.Pid, syscall.SIGTERM); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("SignalChild error = %v, want os.ErrProcessDone", err)
	}
}

func TestStopAPIsReturnCancelErrorsThroughExec(t *testing.T) {
	sentinel := errors.New("synthetic stop request failure")
	t.Run("StopChild", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sleep", "30")
		cancelResult := make(chan error, 1)
		cmd.Cancel = func() error {
			_, err := StopChild(cmd.Process.Pid, 0, func() error {
				if err := SignalChild(cmd.Process.Pid, syscall.SIGTERM); err != nil {
					return err
				}
				return sentinel
			})
			cancelResult <- err
			return err
		}
		_ = cmd.Run()
		if err := <-cancelResult; !errors.Is(err, sentinel) {
			t.Fatalf("Cmd.Cancel error %v does not wrap request error", err)
		}
	})
	t.Run("StopChildGroup", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sleep", "30")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cancelResult := make(chan error, 1)
		real := signalProcess
		t.Cleanup(func() { signalProcess = real })
		signalProcess = func(pid int, target *trackedProcess, sig unix.Signal) error {
			err := real(pid, target, sig)
			if sig == unix.Signal(syscall.SIGTERM) {
				return errors.Join(err, sentinel)
			}
			return err
		}
		cmd.Cancel = func() error {
			_, err := StopChildGroup(cmd, 0, syscall.SIGTERM)
			cancelResult <- err
			return err
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()
		if err := <-cancelResult; !errors.Is(err, sentinel) {
			t.Fatalf("Cmd.Cancel error %v does not wrap group request error", err)
		}
	})
}

func TestStopChildRejectsForeignExitedPIDAndLeavesItsProcessAlive(t *testing.T) {
	dir := t.TempDir()
	bPath, cPath := filepath.Join(dir, "b.pid"), filepath.Join(dir, "c.pid")
	script := `setsid sh -c 'sleep 2 & echo $! > "$2"; echo $$ > "$1"; exit 0' sh "$1" "$2"; sleep 2`
	a := exec.Command("sh", "-c", script, "sh", bPath, cPath)
	a.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := StopChildGroup(a, 0, syscall.SIGKILL); err != nil {
			t.Errorf("stop probe parent group: %v", err)
		}
		if a.ProcessState == nil {
			_ = a.Wait()
		}
	})
	bPID := readPID(t, bPath)
	cPID := readPID(t, cPath)
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := ReadStat("/proc", bPID); Gone(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("B=%d did not exit", bPID)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := StopChild(bPID, 0, nil); err == nil {
		t.Fatal("StopChild accepted a non-child exited PID")
	}
	if stat, err := ReadStat("/proc", cPID); err != nil || stat.State == "Z" || stat.State == "X" {
		t.Fatalf("foreign session member was stopped: stat=%+v err=%v", stat, err)
	}
	if err := a.Wait(); err != nil {
		t.Fatal(err)
	}
	// C is intentionally allowed to finish naturally; the probe never signals
	// a PID it did not start as a direct child.
	deadline = time.Now().Add(10 * time.Second)
	for {
		stat, err := ReadStat("/proc", cPID)
		if Gone(err) || err == nil && (stat.State == "Z" || stat.State == "X") {
			var status syscall.WaitStatus
			_, _ = syscall.Wait4(cPID, &status, syscall.WNOHANG, nil)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe process C=%d did not finish naturally", cPID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStopChildGroupRequiresOwnProcessGroup(t *testing.T) {
	for name, cmd := range map[string]*exec.Cmd{
		"nil command":           nil,
		"no process":            {SysProcAttr: &syscall.SysProcAttr{Setpgid: true}},
		"no attributes":         {Process: &os.Process{Pid: os.Getpid()}},
		"not setpgid":           {Process: &os.Process{Pid: os.Getpid()}, SysProcAttr: &syscall.SysProcAttr{}},
		"explicit foreign pgid": {Process: &os.Process{Pid: os.Getpid()}, SysProcAttr: &syscall.SysProcAttr{Setpgid: true, Pgid: 1}},
		"reserved pid":          {Process: &os.Process{Pid: 1}, SysProcAttr: &syscall.SysProcAttr{Setpgid: true}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := StopChildGroup(cmd, 0, syscall.SIGKILL); err == nil {
				t.Fatal("StopChildGroup accepted command without its own group")
			}
		})
	}
	if _, err := StopChildGroup(&exec.Cmd{Process: &os.Process{Pid: os.Getpid()}, SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}, 0, 0); err == nil {
		t.Fatal("StopChildGroup accepted signal zero")
	}
	if _, err := StopChildGroup(&exec.Cmd{Process: &os.Process{Pid: os.Getpid()}, SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}, -1, syscall.SIGKILL); err == nil {
		t.Fatal("StopChildGroup accepted negative grace")
	}
}

func TestOwnGroupConfigChecksSetpgidAndSetsid(t *testing.T) {
	for name, tc := range map[string]struct {
		attr *syscall.SysProcAttr
		want bool
	}{
		"nil":                   {attr: nil},
		"neither":               {attr: &syscall.SysProcAttr{}},
		"setpgid default group": {attr: &syscall.SysProcAttr{Setpgid: true}, want: true},
		"setpgid nonzero group": {attr: &syscall.SysProcAttr{Setpgid: true, Pgid: 2}},
		"setsid":                {attr: &syscall.SysProcAttr{Setsid: true}, want: true},
		"conflicting modes":     {attr: &syscall.SysProcAttr{Setpgid: true, Setsid: true}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ownGroupConfig(tc.attr); got != tc.want {
				t.Fatalf("ownGroupConfig = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateSignalTargetRejectsReservedIDsAndSignals(t *testing.T) {
	for _, tc := range []struct {
		pid int
		sig syscall.Signal
	}{{0, syscall.SIGTERM}, {1, syscall.SIGTERM}, {42, 0}, {42, 65}} {
		if err := validateSignalTarget(tc.pid, tc.sig); err == nil {
			t.Errorf("accepted pid=%d signal=%d", tc.pid, tc.sig)
		}
	}
}

func TestStopChildGroupStopsOutOfTreeMemberInLiveZombieAndReapedCases(t *testing.T) {
	for _, leaderState := range []string{"live", "zombie", "reaped"} {
		t.Run(leaderState, func(t *testing.T) {
			dir := t.TempDir()
			pidFile, ready := filepath.Join(dir, "member.pid"), filepath.Join(dir, "ready")
			script := `sh -c 'trap "" TERM; sleep 30 >/dev/null 2>&1 & echo $! > "$1"; exit 0' sh "$1" & while [ ! -s "$1" ]; do sleep .005; done; echo ready > "$2"; if [ "$3" = "live" ]; then exec sleep 30; fi; exit 0`
			cmd := exec.Command("sh", "-c", script, "sh", pidFile, ready, leaderState)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
					t.Errorf("clean process group: %v", err)
				}
				if cmd.ProcessState == nil {
					_ = cmd.Wait()
				}
			})
			waitReady(t, ready)
			member := readPID(t, pidFile)
			if leaderState != "live" {
				waitForZombie(t, cmd.Process.Pid)
				if leaderState == "reaped" {
					if err := cmd.Wait(); err != nil {
						t.Fatal(err)
					}
				}
			}

			real := signalProcess
			var sent []unix.Signal
			signalProcess = func(pid int, target *trackedProcess, sig unix.Signal) error {
				sent = append(sent, sig)
				return real(pid, target, sig)
			}
			t.Cleanup(func() { signalProcess = real })
			started := time.Now()
			_, err := StopChildGroup(cmd, 180*time.Millisecond, syscall.SIGTERM)
			if err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
				t.Fatalf("group cleanup ended before grace: %v", elapsed)
			}
			term, kill := false, false
			for _, sig := range sent {
				term = term || sig == unix.SIGTERM
				kill = kill || sig == unix.SIGKILL
			}
			if !term || !kill {
				t.Fatalf("group signals = %v, want TERM then SIGKILL", sent)
			}
			waitForGoneOrZombie(t, member)
		})
	}
}

func waitForZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		stat, err := ReadStat("/proc", pid)
		if err == nil && stat.State == "Z" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("leader %d did not become a zombie", pid)
}

func waitForGoneOrZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		stat, err := ReadStat("/proc", pid)
		if Gone(err) || err == nil && (stat.State == "Z" || stat.State == "X") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	stat, err := ReadStat("/proc", pid)
	t.Fatalf("member %d remained alive: %+v, %v", pid, stat, err)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil && strings.HasSuffix(string(b), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("PID file did not appear: %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitReady waits until the child has written its ready file: its trap is set.
func waitReady(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			text := string(data)
			if _, parseErr := strconv.Atoi(strings.TrimSpace(text)); parseErr != nil || strings.HasSuffix(text, "\n") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not get ready: %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStopChildGroupCountsEachKilledProcessOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sig   syscall.Signal
		grace time.Duration
	}{{"kill", syscall.SIGKILL, 0}, {"term", syscall.SIGTERM, 50 * time.Millisecond}} {
		ready := filepath.Join(t.TempDir(), "ready")
		cmd := startGroupLeader(t, "sh", "-c", "trap '' TERM; sleep 30 & echo ready > \"$1\"; wait", "sh", ready)
		waitReady(t, ready)
		realSignal := signalProcess
		faked := 0
		repeated := false
		signalProcess = func(pid int, process *trackedProcess, sig unix.Signal) error {
			if sig == unix.SIGKILL && pid != cmd.Process.Pid {
				faked++
				if faked == 1 {
					return nil // reported as sent while the process stays visible to the next pass
				}
				repeated = true
			}
			return realSignal(pid, process, sig)
		}
		result, err := StopChildGroup(cmd, tc.grace, tc.sig)
		signalProcess = realSignal
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !repeated {
			t.Fatalf("%s: SIGKILL to the background process was not repeated, the test does not reach the second pass", tc.name)
		}
		if result.Killed != 2 {
			t.Fatalf("%s: result = %+v, want the shell and its sleep counted once each", tc.name, result)
		}
		if err := cmd.Wait(); err == nil {
			t.Fatalf("%s: shell exited successfully after SIGKILL", tc.name)
		}
	}
}

// startGroupLeader starts name as its own process-group leader and stops the
// group at cleanup if the test did not wait for it.
func startGroupLeader(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_, _ = StopChildGroup(cmd, 0, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	return cmd
}
