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
	if _, err := StopChild(cmd.Process.Pid, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
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

func TestExitedChildGroupNeverTargetsCallerOrParent(t *testing.T) {
	parent, err := ReadStat("/proc", os.Getppid())
	if err != nil {
		t.Fatal(err)
	}
	own, err := unix.Getpgid(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	real := signalProcessGroup
	t.Cleanup(func() { signalProcessGroup = real })
	var called []int
	signalProcessGroup = func(pgid int, _ unix.Signal) error {
		called = append(called, pgid)
		return nil
	}
	for _, pgid := range []int{-1, 0, own, parent.Pgid} {
		if err := killExitedChildGroup("/proc", os.Getpid(), pgid); err != nil {
			t.Fatalf("pgid %d: %v", pgid, err)
		}
	}
	if len(called) != 0 {
		t.Fatalf("signaled protected groups: %v", called)
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
