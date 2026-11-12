package proc

import (
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
				return cmd.Process.Signal(syscall.SIGTERM)
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
	cmd := exec.Command("sh", "-c", "sleep 30 & echo $! > \"$1\"; exit 0", "sh", pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := StopChildGroup(cmd, 0, nil); err != nil {
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
	if _, err := StopChildGroup(cmd, 0, nil); err != nil {
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
		if _, err := StopChildGroup(cmd, 0, nil); err != nil {
			t.Errorf("stop live child group: %v", err)
		}
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	})
	waitReady(t, ready)
	result, err := StopChildGroup(cmd, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("group leader exited successfully after SIGKILL")
	}
	if result.Killed == 0 {
		t.Errorf("StopChildGroup did not report a killed tree member: %+v", result)
	}
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
		if _, err := StopChildGroup(a, 0, nil); err != nil {
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
	deadline = time.Now().Add(3 * time.Second)
	for {
		if _, err := ReadStat("/proc", cPID); Gone(err) {
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
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := StopChildGroup(cmd, 0, nil); err == nil {
				t.Fatal("StopChildGroup accepted command without its own group")
			}
		})
	}
}

func TestSafeChildGroupGuardsEveryProtectedID(t *testing.T) {
	if safeChildGroup(10, 10, 11, 12) {
		t.Error("accepted caller's process group")
	}
	if safeChildGroup(11, 10, 11, 12) {
		t.Error("accepted parent process group")
	}
	if safeChildGroup(12, 10, 11, 12) {
		t.Error("accepted caller's session group")
	}
	for _, pgid := range []int{-1, 0, 1} {
		if safeChildGroup(pgid, 10, 11, 12) {
			t.Errorf("accepted invalid group ID %d", pgid)
		}
	}
	if !safeChildGroup(13, 10, 11, 12) {
		t.Error("rejected a distinct valid group")
	}
}

func TestStopChildGroupIgnoresForeignProcessUsingGroupNumber(t *testing.T) {
	parent, err := ReadStat("/proc", os.Getppid())
	if err != nil {
		t.Fatal(err)
	}
	own, err := unix.Getpgid(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	sid, err := unix.Getsid(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !safeChildGroup(os.Getpid(), own, parent.Pgid, sid) {
		t.Skip("test process PID is a protected group ID")
	}
	real := signalProcessGroup
	t.Cleanup(func() { signalProcessGroup = real })
	called := false
	signalProcessGroup = func(_ int, _ unix.Signal) error {
		called = true
		return nil
	}
	cmd := &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}, SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}
	if _, err := StopChildGroup(cmd, 0, nil); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("signaled group whose number belongs to a process that is not our child")
	}
}

func TestCommandOwnsGroupLeaderRequiresUnwaitedDirectChild(t *testing.T) {
	cmd := &exec.Cmd{Process: &os.Process{Pid: 42}}
	if !commandOwnsGroupLeader(cmd, 7, Stat{Ppid: 7, Pgid: 42}) {
		t.Fatal("rejected an unwaited direct child that leads its group")
	}
	for name, tc := range map[string]struct {
		cmd    *exec.Cmd
		self   int
		target Stat
	}{
		"not our child":    {cmd: cmd, self: 7, target: Stat{Ppid: 8, Pgid: 42}},
		"not group leader": {cmd: cmd, self: 7, target: Stat{Ppid: 7, Pgid: 9}},
		"already waited":   {cmd: &exec.Cmd{Process: cmd.Process, ProcessState: &os.ProcessState{}}, self: 7, target: Stat{Ppid: 7, Pgid: 42}},
	} {
		t.Run(name, func(t *testing.T) {
			if commandOwnsGroupLeader(tc.cmd, tc.self, tc.target) {
				t.Fatal("accepted an unrelated or already-waited process")
			}
		})
	}
}

func TestStopChildGroupIgnoresESRCH(t *testing.T) {
	real := signalProcessGroup
	t.Cleanup(func() { signalProcessGroup = real })
	signalProcessGroup = func(_ int, _ unix.Signal) error { return unix.ESRCH }
	cmd := &exec.Cmd{Process: &os.Process{Pid: 2_000_000_000}, SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}
	if _, err := StopChildGroup(cmd, 0, nil); err != nil {
		t.Fatalf("ESRCH should mean the group is already gone: %v", err)
	}
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
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not get ready: %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
