//go:build linux

package proc

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStopChildGroupRecoversLeaderValidationRaces(t *testing.T) {
	for _, tc := range []struct {
		name  string
		point string
	}{
		{name: "gone before tree open", point: "tree"},
		{name: "ESRCH from pidfd_open", point: "before pidfd"},
		{name: "gone during validation", point: "after pidfd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, member := startRaceGroup(t)
			injectLeaderExit(t, cmd, tc.point)
			realSignalGroup := signalProcessGroup
			termRequested := false
			signalProcessGroup = func(pgid int, sig unix.Signal) error {
				if pgid == cmd.Process.Pid && sig == unix.SIGTERM {
					termRequested = true
				}
				return realSignalGroup(pgid, sig)
			}
			defer func() { signalProcessGroup = realSignalGroup }()
			result, err := StopChildGroup(cmd, 80*time.Millisecond, syscall.SIGTERM)
			if err != nil {
				t.Fatalf("StopChildGroup returned error after leader race: %v", err)
			}
			if !result.AlreadyExited {
				t.Errorf("result = %+v, want AlreadyExited after leader was reaped", result)
			}
			if !termRequested {
				t.Error("race recovery did not send the requested SIGTERM to the group")
			}
			waitForGroupMemberDead(t, member, time.Second)
		})
	}
}

func TestOpenStopTargetMarksLeaderValidationRaces(t *testing.T) {
	for _, tc := range []struct {
		name  string
		point string
	}{
		{name: "gone", point: "gone"},
		{name: "pidfd ESRCH", point: "before pidfd"},
		{name: "changed during validation", point: "after pidfd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, _ := startRaceGroup(t)
			if tc.point == "gone" {
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				if err := cmd.Wait(); err != nil {
					var exitErr *exec.ExitError
					if !errors.As(err, &exitErr) {
						t.Fatal(err)
					}
				}
			} else {
				injectLeaderExit(t, cmd, tc.point)
			}
			_, _, err := openStopTarget("/proc", os.Getpid(), cmd.Process.Pid, unix.SIGTERM)
			if !errors.Is(err, errStopTargetChanged) {
				t.Fatalf("openStopTarget error = %v, want errStopTargetChanged", err)
			}
		})
	}
}

func TestStopTargetRaceRecognizesGoneAndESRCH(t *testing.T) {
	for name, err := range map[string]error{
		"validation marker": errStopTargetChanged,
		"gone":              fs.ErrNotExist,
		"pidfd ESRCH":       wrappedStopRaceTestError{cause: unix.ESRCH},
	} {
		t.Run(name, func(t *testing.T) {
			if !stopTargetRace(err) {
				t.Fatalf("stopTargetRace(%v) = false", err)
			}
		})
	}
	if stopTargetRace(errors.New("permission denied")) {
		t.Fatal("stopTargetRace accepted an unrelated error")
	}
}

type wrappedStopRaceTestError struct{ cause error }

func (e wrappedStopRaceTestError) Error() string { return "wrapped stop race test error" }
func (e wrappedStopRaceTestError) Unwrap() error { return e.cause }

func injectLeaderExit(t *testing.T, cmd *exec.Cmd, point string) {
	t.Helper()
	oldTree, oldBefore, oldAfter := beforeStopChildGroupTree, beforeStopTargetPidfdOpen, afterStopTargetPidfdOpen
	t.Cleanup(func() {
		beforeStopChildGroupTree, beforeStopTargetPidfdOpen, afterStopTargetPidfdOpen = oldTree, oldBefore, oldAfter
	})
	var once sync.Once
	exitLeader := func(pid int) {
		if pid != cmd.Process.Pid {
			return
		}
		once.Do(func() {
			if err := cmd.Process.Kill(); err != nil {
				t.Errorf("kill test leader: %v", err)
				return
			}
			if err := cmd.Wait(); err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Errorf("wait for killed test leader: %v", err)
				}
			}
		})
	}
	switch point {
	case "tree":
		beforeStopChildGroupTree = exitLeader
	case "before pidfd":
		beforeStopTargetPidfdOpen = exitLeader
	case "after pidfd":
		afterStopTargetPidfdOpen = exitLeader
	default:
		t.Fatalf("unknown injection point %q", point)
	}
}

func TestStopChildGroupCancelRaceStress(t *testing.T) {
	iterations := 300
	if testing.Short() {
		iterations = 40
	}
	dir := t.TempDir()
	var called atomic.Int32
	jobs := make(chan int)
	workers := min(40, iterations)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				pidFile := filepath.Join(dir, strconv.Itoa(i)+".pid")
				ctx, cancel := context.WithCancel(context.Background())
				cmd := exec.CommandContext(ctx, "sh", "-c", `sh -c 'trap "" TERM; exec sleep 30' </dev/null >/dev/null 2>&1 & echo $! > "$1"; sleep 0.03`, "sh", pidFile)
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				var mu sync.Mutex
				var cancelCalled bool
				var cancelErr error
				cmd.Cancel = func() error {
					_, err := StopChildGroup(cmd, 0, syscall.SIGKILL)
					mu.Lock()
					cancelCalled, cancelErr = true, err
					mu.Unlock()
					return err
				}
				cmd.WaitDelay = time.Second
				if err := cmd.Start(); err != nil {
					cancel()
					t.Errorf("iteration %d: start command: %v", i, err)
					continue
				}
				timer := time.AfterFunc(15*time.Millisecond, cancel)
				_ = cmd.Wait()
				timer.Stop()
				cancel()
				mu.Lock()
				wasCalled, stopErr := cancelCalled, cancelErr
				mu.Unlock()
				if wasCalled {
					called.Add(1)
					if stopErr != nil {
						t.Errorf("iteration %d: Cancel returned %v", i, stopErr)
					}
					if raw, err := os.ReadFile(pidFile); err == nil {
						if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 1 {
							if !groupMemberGone(pid, time.Second) {
								t.Errorf("iteration %d: group member %d survived Cancel", i, pid)
							}
						}
					}
				}
				if cmd.Process != nil {
					if _, err := StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
						t.Errorf("iteration %d: cleanup own process group: %v", i, err)
					}
				}
			}
		})
	}
	for i := 0; i < iterations; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if int(called.Load()) < iterations*2/3 {
		t.Fatalf("Cancel ran in only %d of %d iterations", called.Load(), iterations)
	}
	t.Logf("Cancel invoked in %d/%d iterations; errors=0; checked leftovers dead", called.Load(), iterations)
}

func startRaceGroup(t *testing.T) (*exec.Cmd, int) {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "member.pid")
	ready := filepath.Join(dir, "ready")
	cmd := exec.Command("sh", "-c", `sh -c 'trap "" TERM; exec sleep 30' </dev/null >/dev/null 2>&1 & echo $! > "$1"; echo ready > "$2"; exec sleep 30`, "sh", pidFile, ready)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		if _, err := StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
			t.Errorf("cleanup test group: %v", err)
		}
	})
	waitReady(t, ready)
	return cmd, readPID(t, pidFile)
}

func waitForGroupMemberDead(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	if !groupMemberGone(pid, timeout) {
		stat, err := ReadStat("/proc", pid)
		t.Fatalf("group member %d still alive after %v: stat=%+v err=%v", pid, timeout, stat, err)
	}
}

func groupMemberGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		stat, err := ReadStat("/proc", pid)
		if err != nil || stat.State == "Z" || stat.State == "X" {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}
