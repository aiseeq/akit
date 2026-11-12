//go:build linux

package proc

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStopChildGroupDoesNotSignalForeignGroupMember(t *testing.T) {
	if os.Getenv("PROC_GROUP_OWNER_ROLE") == "caller" {
		pidPath := os.Getenv("PROC_GROUP_OWNER_PID")
		readyPath := os.Getenv("PROC_GROUP_OWNER_READY")
		cmd := exec.Command("sleep", "30")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			os.Exit(10)
		}
		if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
			os.Exit(11)
		}
		if err := waitFile(readyPath, 5*time.Second); err != nil {
			os.Exit(12)
		}
		if _, err := StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
			os.Exit(13)
		}
		if err := cmd.Wait(); err == nil {
			os.Exit(14)
		}
		os.Exit(0)
	}
	if os.Getenv("PROC_GROUP_OWNER_ROLE") == "outsider" {
		pgid, _ := strconv.Atoi(os.Getenv("PROC_GROUP_OWNER_PGID"))
		if err := syscall.Setpgid(0, pgid); err != nil {
			os.Exit(20)
		}
		if err := os.WriteFile(os.Getenv("PROC_GROUP_OWNER_READY"), []byte("ready"), 0o600); err != nil {
			os.Exit(21)
		}
		time.Sleep(time.Hour)
		os.Exit(0)
	}

	dir := t.TempDir()
	pidPath, readyPath := filepath.Join(dir, "group.pid"), filepath.Join(dir, "outsider.ready")
	caller := exec.Command(os.Args[0], "-test.run=^TestStopChildGroupDoesNotSignalForeignGroupMember$")
	caller.Env = append(os.Environ(), "PROC_GROUP_OWNER_ROLE=caller", "PROC_GROUP_OWNER_PID="+pidPath, "PROC_GROUP_OWNER_READY="+readyPath)
	if err := caller.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if caller.ProcessState == nil {
			_, _ = StopChild(caller.Process.Pid, 0, nil)
			_ = caller.Wait()
		}
	})
	groupLeader := readPID(t, pidPath)
	outsider := exec.Command(os.Args[0], "-test.run=^TestStopChildGroupDoesNotSignalForeignGroupMember$")
	outsider.Env = append(os.Environ(), "PROC_GROUP_OWNER_ROLE=outsider", "PROC_GROUP_OWNER_PGID="+strconv.Itoa(groupLeader), "PROC_GROUP_OWNER_READY="+readyPath)
	if err := outsider.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if outsider.ProcessState == nil {
			if _, err := StopChild(outsider.Process.Pid, 0, nil); err != nil {
				t.Errorf("stop test-owned outsider: %v", err)
			}
			_ = outsider.Wait()
		}
	})
	waitReady(t, readyPath)
	if _, err := processAncestry("/proc", outsider.Process.Pid, caller.Process.Pid); err == nil {
		t.Fatal("accepted the caller's sibling as its descendant")
	}
	groupTargets, err := discoverCommandTargets("/proc", caller.Process.Pid, groupLeader)
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range groupTargets {
		if pid == outsider.Process.Pid {
			t.Fatal("discovery accepted an unrelated process solely by group membership")
		}
	}
	if err := caller.Wait(); err != nil {
		t.Fatalf("StopChildGroup helper: %v", err)
	}
	if stat, err := ReadStat("/proc", outsider.Process.Pid); err != nil || stat.State == "Z" || stat.State == "X" {
		t.Fatalf("foreign group member was signaled: stat=%+v err=%v", stat, err)
	}
}

func TestStopChildGroupRechecksMembershipAfterPidfdOpen(t *testing.T) {
	if os.Getenv("PROC_GROUP_MOVE_ROLE") == "helper" {
		pgid, _ := strconv.Atoi(os.Getenv("PROC_GROUP_MOVE_PGID"))
		if err := syscall.Setpgid(0, pgid); err != nil {
			os.Exit(40)
		}
		if err := os.WriteFile(os.Getenv("PROC_GROUP_MOVE_READY"), []byte("ready"), 0o600); err != nil {
			os.Exit(41)
		}
		if err := waitFile(os.Getenv("PROC_GROUP_MOVE_NOW"), 5*time.Second); err != nil {
			os.Exit(42)
		}
		if err := syscall.Setpgid(0, 0); err != nil {
			os.Exit(43)
		}
		if err := os.WriteFile(os.Getenv("PROC_GROUP_MOVE_DONE"), []byte("done"), 0o600); err != nil {
			os.Exit(44)
		}
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	cmd := exec.Command("sleep", "30")
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
	dir := t.TempDir()
	readyPath, movePath, donePath := filepath.Join(dir, "ready"), filepath.Join(dir, "move"), filepath.Join(dir, "done")
	outsider := exec.Command(os.Args[0], "-test.run=^TestStopChildGroupRechecksMembershipAfterPidfdOpen$")
	outsider.Env = append(os.Environ(), "PROC_GROUP_MOVE_ROLE=helper", "PROC_GROUP_MOVE_PGID="+strconv.Itoa(cmd.Process.Pid), "PROC_GROUP_MOVE_READY="+readyPath, "PROC_GROUP_MOVE_NOW="+movePath, "PROC_GROUP_MOVE_DONE="+donePath)
	if err := outsider.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if outsider.ProcessState == nil {
			if _, err := StopChild(outsider.Process.Pid, 0, nil); err != nil {
				t.Errorf("stop test-owned outsider: %v", err)
			}
			_ = outsider.Wait()
		}
	})
	waitReady(t, readyPath)
	realOpenHook, realSignal := afterCommandTargetPidfdOpen, signalProcess
	var moved, signaled bool
	afterCommandTargetPidfdOpen = func(pid int) {
		if pid == outsider.Process.Pid {
			moved = true
			if err := os.WriteFile(movePath, []byte("move"), 0o600); err != nil {
				t.Errorf("request outsider group change: %v", err)
			}
			if err := waitFile(donePath, time.Second); err != nil {
				t.Errorf("wait for outsider group change: %v", err)
			}
		}
	}
	signalProcess = func(pid int, target *trackedProcess, sig unix.Signal) error {
		if pid == outsider.Process.Pid {
			signaled = true
		}
		return realSignal(pid, target, sig)
	}
	t.Cleanup(func() {
		afterCommandTargetPidfdOpen, signalProcess = realOpenHook, realSignal
	})
	if _, err := StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if !moved {
		t.Fatal("did not exercise the post-pidfd group membership change")
	}
	if signaled {
		t.Fatal("signaled a process after it left the command group and tree")
	}
	if stat, err := ReadStat("/proc", outsider.Process.Pid); err != nil || stat.State == "Z" || stat.State == "X" {
		t.Fatalf("outsider did not survive: stat=%+v err=%v", stat, err)
	}
}

func TestStopChildGroupDropsStaleTreeMembershipAfterPidfdOpen(t *testing.T) {
	if os.Getenv("PROC_STALE_OWNER_ROLE") == "target" {
		if err := waitFile(os.Getenv("PROC_STALE_OWNER_MOVE"), 5*time.Second); err != nil {
			os.Exit(70)
		}
		if err := syscall.Setpgid(0, 0); err != nil {
			os.Exit(71)
		}
		if err := os.WriteFile(os.Getenv("PROC_STALE_OWNER_DONE"), []byte("done"), 0o600); err != nil {
			os.Exit(72)
		}
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	dir := t.TempDir()
	movePath, donePath := filepath.Join(dir, "move"), filepath.Join(dir, "done")
	cmd := exec.Command("sh", "-c", `"$1" -test.run=^TestStopChildGroupDropsStaleTreeMembershipAfterPidfdOpen$; wait`, "sh", os.Args[0])
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "PROC_STALE_OWNER_ROLE=target", "PROC_STALE_OWNER_MOVE="+movePath, "PROC_STALE_OWNER_DONE="+donePath)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_, _ = StopChildGroup(cmd, 0, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	// The command shell is the lower PID and is always visited before its helper.
	deadline := time.Now().Add(5 * time.Second)
	var target int
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir("/proc")
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid <= cmd.Process.Pid {
				continue
			}
			stat, err := ReadStat("/proc", pid)
			if err == nil && stat.Ppid == cmd.Process.Pid && stat.Pgid == cmd.Process.Pid {
				target = pid
				break
			}
		}
		if target != 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if target == 0 {
		t.Fatal("did not find test helper under command shell")
	}
	var moved, signaled bool
	realHook, realSignal := afterCommandTargetPidfdOpen, signalProcess
	afterCommandTargetPidfdOpen = func(pid int) {
		if pid != target || moved {
			return
		}
		moved = true
		if err := cmd.Process.Kill(); err != nil {
			t.Errorf("exit test command leader: %v", err)
		}
		if err := cmd.Wait(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Errorf("wait for test command leader: %v", err)
			}
		}
		if err := os.WriteFile(movePath, []byte("move"), 0o600); err != nil {
			t.Errorf("request helper group change: %v", err)
		}
		if err := waitFile(donePath, time.Second); err != nil {
			t.Errorf("wait for helper group change: %v", err)
		}
	}
	signalProcess = func(pid int, tracked *trackedProcess, sig unix.Signal) error {
		if pid == target {
			signaled = true
		}
		return realSignal(pid, tracked, sig)
	}
	t.Cleanup(func() { afterCommandTargetPidfdOpen, signalProcess = realHook, realSignal })
	if _, err := StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if !moved || signaled {
		t.Fatalf("stale tree membership: moved=%v signaled=%v", moved, signaled)
	}
	if stat, err := ReadStat("/proc", target); err != nil || stat.State == "Z" || stat.State == "X" {
		t.Fatalf("target did not survive after leaving tree and group: stat=%+v err=%v", stat, err)
	}
	if err := SignalChild(target, syscall.SIGKILL); err != nil {
		t.Fatalf("stop test-owned adopted helper: %v", err)
	}
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(target, &status, 0, nil); err != nil {
		t.Fatalf("reap test-owned helper: %v", err)
	}
}

func TestVerifiedCommandTargetRejectsChangedAncestryStartTime(t *testing.T) {
	cmd := exec.Command("sleep", "30")
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
	realRead, realHook := readCommandAncestryStat, afterCommandTargetPidfdOpen
	opened := false
	readCommandAncestryStat = func(root string, pid int) (Stat, error) {
		stat, err := realRead(root, pid)
		if pid == cmd.Process.Pid && opened && err == nil {
			stat.Start += "-reused"
		}
		return stat, err
	}
	afterCommandTargetPidfdOpen = func(pid int) {
		if pid == cmd.Process.Pid {
			opened = true
		}
	}
	t.Cleanup(func() { readCommandAncestryStat, afterCommandTargetPidfdOpen = realRead, realHook })
	if _, err := verifiedCommandTarget("/proc", os.Getpid(), cmd.Process.Pid, cmd.Process.Pid); !errors.Is(err, errStopTargetChanged) {
		t.Fatalf("verifiedCommandTarget error = %v, want changed ancestry identity", err)
	}
}

func TestVerifiedCommandTargetPinsAndRechecksStartTime(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			if _, err := StopChild(cmd.Process.Pid, 0, nil); err != nil {
				t.Errorf("stop test-owned child: %v", err)
			}
			_ = cmd.Wait()
		}
	})
	realRead, realAfter := readCommandTargetStat, afterCommandTargetPidfdOpen
	opened := false
	readCommandTargetStat = func(root string, pid int) (Stat, error) {
		stat, err := realRead(root, pid)
		if pid == cmd.Process.Pid && opened && err == nil {
			stat.Start += "-reused"
		}
		return stat, err
	}
	afterCommandTargetPidfdOpen = func(pid int) {
		if pid == cmd.Process.Pid {
			opened = true
		}
	}
	t.Cleanup(func() { readCommandTargetStat, afterCommandTargetPidfdOpen = realRead, realAfter })
	if _, err := verifiedCommandTarget("/proc", os.Getpid(), cmd.Process.Pid, cmd.Process.Pid); !errors.Is(err, errStopTargetChanged) {
		t.Fatalf("verifiedCommandTarget error = %v, want changed identity", err)
	}
	opened = false
	readCommandTargetStat = realRead
	tracked, err := verifiedCommandTarget("/proc", os.Getpid(), cmd.Process.Pid, cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(tracked.fd) })
	if err := unix.PidfdSendSignal(tracked.fd, 0, nil, 0); err != nil {
		t.Fatalf("verified target has no usable pidfd: %v", err)
	}
	if alive, err := processAlive(tracked); err != nil || !alive {
		t.Fatalf("verified target is not pinned by a live pidfd: alive=%v err=%v", alive, err)
	}
}

func TestVerifiedCommandTargetRechecksParentChainAfterPidfdOpen(t *testing.T) {
	if os.Getenv("PROC_PARENT_RACE_HELPER") == "1" {
		pidPath := os.Getenv("PROC_PARENT_RACE_PIDFILE")
		middle := exec.Command("sh", "-c", `sleep 30 & echo $! > "$1"; wait`, "sh", pidPath)
		middle.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := middle.Start(); err != nil {
			os.Exit(60)
		}
		if err := waitFile(pidPath, 5*time.Second); err != nil {
			os.Exit(61)
		}
		target := readPID(t, pidPath)
		realHook := afterCommandTargetPidfdOpen
		afterCommandTargetPidfdOpen = func(pid int) {
			if pid == target {
				if err := SignalChild(middle.Process.Pid, syscall.SIGKILL); err != nil {
					os.Exit(62)
				}
				if err := middle.Wait(); err == nil {
					os.Exit(64)
				}
			}
		}
		_, err := verifiedCommandTarget("/proc", os.Getpid(), middle.Process.Pid, target)
		afterCommandTargetPidfdOpen = realHook
		if !errors.Is(err, errStopTargetChanged) {
			os.Exit(63)
		}
		os.Exit(0)
	}

	pidPath := filepath.Join(t.TempDir(), "target.pid")
	helper := exec.Command(os.Args[0], "-test.run=^TestVerifiedCommandTargetRechecksParentChainAfterPidfdOpen$")
	helper.Env = append(os.Environ(), "PROC_PARENT_RACE_HELPER=1", "PROC_PARENT_RACE_PIDFILE="+pidPath, "PROC_DISABLE_TEST_SUBREAPER=1")
	if err := helper.Run(); err != nil {
		t.Fatalf("parent-chain helper: %v", err)
	}
	target := readPID(t, pidPath)
	stat, err := ReadStat("/proc", target)
	if err != nil || stat.State == "Z" || stat.State == "X" {
		t.Fatalf("target was signaled after it left the caller's ancestry: stat=%+v err=%v", stat, err)
	}
	if err := SignalChild(target, syscall.SIGKILL); err != nil {
		t.Fatalf("clean test-owned adopted target: %v", err)
	}
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(target, &status, 0, nil); err != nil {
		t.Fatalf("reap test-owned adopted target: %v", err)
	}
}

func TestStopChildGroupStopsAdoptedMembersOnlyAsSubreaper(t *testing.T) {
	if os.Getenv("PROC_SUBREAPER_HELPER") == "1" {
		if err := BecomeChildSubreaper(); err != nil {
			os.Exit(50)
		}
		pidPath := os.Getenv("PROC_SUBREAPER_PIDFILE")
		cmd := exec.Command("bash", "-c", `sleep 30 & a=$!; sleep 30 & b=$!; printf '%s %s\n' "$a" "$b" > "$1"; exit`, "bash", pidPath)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			os.Exit(51)
		}
		if err := cmd.Wait(); err != nil {
			os.Exit(52)
		}
		adopted := readPIDs(t, pidPath)
		for _, pid := range adopted {
			if err := SignalChild(pid, syscall.SIGKILL); err != nil {
				os.Exit(56)
			}
			deadline := time.Now().Add(time.Second)
			for {
				stat, err := ReadStat("/proc", pid)
				if err == nil && stat.State == "Z" && stat.Ppid == os.Getpid() {
					break
				}
				if time.Now().After(deadline) {
					os.Exit(57)
				}
				time.Sleep(time.Millisecond)
			}
		}
		if _, err := StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
			os.Exit(53)
		}
		for _, pid := range adopted {
			stat, err := ReadStat("/proc", pid)
			if err != nil || stat.State != "Z" || stat.Ppid != os.Getpid() {
				os.Exit(54)
			}
			var status syscall.WaitStatus
			if waited, waitErr := syscall.Wait4(pid, &status, syscall.WNOHANG, nil); waited != pid || waitErr != nil {
				os.Exit(55)
			}
		}
		os.Exit(0)
	}
	t.Run("subreaper adopts", func(t *testing.T) {
		pidPath := filepath.Join(t.TempDir(), "members")
		helper := exec.Command(os.Args[0], "-test.run=^TestStopChildGroupStopsAdoptedMembersOnlyAsSubreaper$")
		helper.Env = append(os.Environ(), "PROC_SUBREAPER_HELPER=1", "PROC_SUBREAPER_PIDFILE="+pidPath, "PROC_DISABLE_TEST_SUBREAPER=1")
		if err := helper.Run(); err != nil {
			for _, pid := range readPIDs(t, pidPath) {
				if stat, err := ReadStat("/proc", pid); err == nil && stat.Ppid == os.Getpid() {
					_ = SignalChild(pid, syscall.SIGKILL)
					var status syscall.WaitStatus
					_, _ = syscall.Wait4(pid, &status, 0, nil)
				}
			}
			t.Fatalf("subreaper helper: %v", err)
		}
		// The helper verified that these were already direct children before
		// StopChildGroup; their exit status remains for the owning Wait4.
	})
	t.Run("without subreaper", func(t *testing.T) {
		pidPath := filepath.Join(t.TempDir(), "members")
		caller := exec.Command(os.Args[0], "-test.run=^TestStopChildGroupDoesNotAdoptUnownedMembers$")
		caller.Env = append(os.Environ(), "PROC_NO_SUBREAPER_PIDFILE="+pidPath, "PROC_DISABLE_TEST_SUBREAPER=1")
		if err := caller.Run(); err != nil {
			t.Fatalf("non-subreaper helper: %v", err)
		}
		for _, pid := range readPIDs(t, pidPath) {
			// TestMain is a subreaper, so these are now direct children owned by
			// this test process and can be safely cleaned up by PID.
			if err := SignalChild(pid, syscall.SIGKILL); err != nil {
				t.Errorf("clean test-owned adopted PID %d: %v", pid, err)
			}
			var status syscall.WaitStatus
			_, _ = syscall.Wait4(pid, &status, 0, nil)
		}
	})
}

func TestStopChildGroupDoesNotReapExistingDirectChild(t *testing.T) {
	if pgidText := os.Getenv("PROC_DIRECT_GROUP_MEMBER_PGID"); pgidText != "" {
		pgid, err := strconv.Atoi(pgidText)
		if err != nil || pgid <= 1 {
			os.Exit(80)
		}
		if err := syscall.Setpgid(0, pgid); err != nil {
			os.Exit(81)
		}
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	outsider := exec.Command(os.Args[0], "-test.run=^TestStopChildGroupDoesNotReapExistingDirectChild$")
	outsider.Env = append(os.Environ(), "PROC_DIRECT_GROUP_MEMBER_PGID="+strconv.Itoa(cmd.Process.Pid))
	if err := outsider.Start(); err != nil {
		_, _ = StopChildGroup(cmd, 0, syscall.SIGKILL)
		_ = cmd.Wait()
		t.Fatal(err)
	}
	if _, err := StopChildGroup(cmd, 40*time.Millisecond, syscall.SIGTERM); err != nil {
		t.Errorf("stop test group: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Error("leader unexpectedly exited successfully")
	}
	if err := outsider.Wait(); err == nil {
		t.Fatal("group member unexpectedly exited successfully")
	} else {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("Cmd.Wait lost its direct child status: %v", err)
		}
	}
}

func TestStopChildGroupDoesNotAdoptUnownedMembers(t *testing.T) {
	path := os.Getenv("PROC_NO_SUBREAPER_PIDFILE")
	if path == "" {
		return
	}
	cmd := exec.Command("bash", "-c", `sleep 30 & a=$!; sleep 30 & b=$!; printf '%s %s\n' "$a" "$b" > "$1"; exit`, "bash", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		os.Exit(30)
	}
	if err := cmd.Wait(); err != nil {
		os.Exit(31)
	}
	pids := readPIDs(t, path)
	if _, err := StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
		os.Exit(32)
	}
	for _, pid := range pids {
		stat, err := ReadStat("/proc", pid)
		if err != nil || stat.State == "Z" || stat.State == "X" {
			os.Exit(33)
		}
	}
	os.Exit(0)
}

func readPIDs(t *testing.T, path string) []int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(b))
	pids := make([]int, 0, len(fields))
	for _, field := range fields {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 1 {
			t.Fatalf("invalid PID %q", field)
		}
		pids = append(pids, pid)
	}
	if len(pids) != 2 {
		t.Fatalf("got %d PIDs in %q, want 2", len(pids), b)
	}
	return pids
}

func waitFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return os.ErrDeadlineExceeded
}
