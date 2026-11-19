//go:build linux

package proc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
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
			fd, err := unix.PidfdOpen(member, 0)
			if err != nil {
				t.Fatalf("open member pidfd: %v", err)
			}
			defer unix.Close(fd)
			injectLeaderExit(t, cmd, tc.point)
			realSignal := signalProcess
			termRequested := false
			signalProcess = func(pid int, target *trackedProcess, sig unix.Signal) error {
				if sig == unix.SIGTERM {
					termRequested = true
				}
				return realSignal(pid, target, sig)
			}
			defer func() { signalProcess = realSignal }()
			result, err := StopChildGroup(cmd, 80*time.Millisecond, syscall.SIGTERM)
			if err != nil {
				t.Fatalf("StopChildGroup returned error after leader race: %v", err)
			}
			_ = result
			if !termRequested {
				t.Error("race recovery did not send the requested SIGTERM to verified descendants")
			}
			if !trackedMemberGone(fd, time.Second) {
				t.Error("group member still alive after race recovery")
			}
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
	// The explicit ESRCH predicate is redundant on Linux because Gone already
	// recognizes ESRCH through noSuchProcess; keep this case as the one accepted
	// equivalent mutation in the reviewer spec.
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
		iterations = 8
	}
	var racedCancel, leaderFirst, guaranteed atomic.Int32
	jobs := make(chan int)
	workers := min(40, iterations)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				ctx, cancel := context.WithCancel(context.Background())
				isGuaranteed := i%4 == 0
				leaderDelay := fmt.Sprintf("0.%03d", 10+rand.IntN(21))
				if isGuaranteed {
					leaderDelay = "30"
				}
				cmd := exec.CommandContext(ctx, "sh", "-c", `sh -c 'trap "" TERM; exec sleep 30' </dev/null >/dev/null 2>&1 & echo $!; exec sleep "$1"`, "sh", leaderDelay)
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
				stdout, err := cmd.StdoutPipe()
				if err != nil {
					cancel()
					t.Errorf("iteration %d: stdout pipe: %v", i, err)
					continue
				}
				if err := cmd.Start(); err != nil {
					cancel()
					t.Errorf("iteration %d: start command: %v", i, err)
					continue
				}
				raw, readErr := bufio.NewReader(stdout).ReadString('\n')
				pid, parseErr := strconv.Atoi(strings.TrimSpace(raw))
				if readErr != nil || parseErr != nil || pid <= 1 {
					t.Errorf("iteration %d: invalid member PID %q: read=%v parse=%v", i, raw, readErr, parseErr)
					cancel()
					_ = cmd.Wait()
					continue
				}
				// Pin the test's member before the cancel timer starts: until then nothing stops it, even if the leader has already exited.
				fd, err := unix.PidfdOpen(pid, 0)
				if err != nil {
					t.Errorf("iteration %d: open member pidfd: %v", i, err)
					cancel()
					_ = cmd.Wait()
					continue
				}
				delay := time.Duration(rand.IntN(16)) * time.Millisecond
				timer := time.AfterFunc(delay, cancel)
				_ = cmd.Wait()
				timer.Stop()
				cancel()
				mu.Lock()
				wasCalled, stopErr := cancelCalled, cancelErr
				mu.Unlock()
				if isGuaranteed {
					guaranteed.Add(1)
					if !wasCalled {
						t.Errorf("iteration %d: guaranteed Cancel was not called", i)
					}
				} else if wasCalled {
					racedCancel.Add(1)
				} else {
					leaderFirst.Add(1)
				}
				if wasCalled {
					if stopErr != nil {
						t.Errorf("iteration %d: Cancel returned %v", i, stopErr)
					}
					if !trackedMemberGone(fd, time.Second) {
						t.Errorf("iteration %d: group member survived Cancel", i)
					}
				} else {
					alive, err := processAlive(&trackedProcess{fd: fd})
					if err != nil || !alive {
						t.Errorf("iteration %d: member should survive leader exit: alive=%v err=%v", i, alive, err)
					}
				}
				if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
					t.Errorf("iteration %d: cleanup member: %v", i, err)
				}
				if !trackedMemberGone(fd, time.Second) {
					t.Errorf("iteration %d: member survived cleanup", i)
				}
				// TestMain is a subreaper: collect this member if it was adopted.
				var info unix.Siginfo
				if err := unix.Waitid(unix.P_PIDFD, fd, &info, unix.WEXITED|unix.WNOHANG, nil); err != nil && !errors.Is(err, unix.ECHILD) {
					t.Errorf("iteration %d: reap member: %v", i, err)
				}
				_ = unix.Close(fd)
			}
		})
	}
	for i := 0; i < iterations; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	t.Logf("race Cancel=%d, leader first=%d, guaranteed=%d, total=%d", racedCancel.Load(), leaderFirst.Load(), guaranteed.Load(), iterations)
}

func trackedMemberGone(fd int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	tracked := &trackedProcess{fd: fd}
	for {
		alive, err := processAlive(tracked)
		if err == nil && !alive {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
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
