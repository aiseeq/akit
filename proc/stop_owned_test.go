//go:build linux

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStopDescendantsStopsAdoptedGrandchildAndNotSibling(t *testing.T) {
	if os.Getenv("PROC_STOP_DESCENDANTS_HELPER") == "1" {
		if err := BecomeChildSubreaper(); err != nil {
			t.Fatal(err)
		}
		ownedCmd := exec.Command("sleep", "30")
		if err := ownedCmd.Start(); err != nil {
			t.Fatal(err)
		}
		marker, pidPath := os.Getenv("PROC_STOP_DESCENDANTS_MARKER"), os.Getenv("PROC_STOP_DESCENDANTS_PID")
		ready, stubbornReady := marker+".ready", marker+".stubborn-ready"
		cmd := exec.Command("sh", "-c", `sh -c 'trap "echo term > $1" TERM; echo ready > $2; while :; do :; done' sh "$1" "$2" & termChild=$!; sh -c 'trap "" TERM; echo ready > $1; while :; do :; done' sh "$3" & stubborn=$!; while [ ! -f "$2" ] || [ ! -f "$3" ]; do sleep 0.01; done; echo "$termChild $stubborn" > "$4"; exit 0`, "sh", marker, ready, stubbornReady, pidPath)
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		pids := readPIDs(t, pidPath)
		result, err := StopDescendants(100*time.Millisecond, syscall.SIGTERM)
		if err != nil {
			t.Fatal(err)
		}
		if result.Killed == 0 {
			t.Fatalf("SIGKILL survivors = 0, want at least the TERM-ignoring descendant; result=%+v", result)
		}
		waitReady(t, marker)
		if err := ownedCmd.Wait(); err == nil {
			t.Fatal("StopDescendants unexpectedly left its owned Cmd child running")
		}
		for _, pid := range pids {
			waitForZombie(t, pid)
			var status syscall.WaitStatus
			if _, err := syscall.Wait4(pid, &status, 0, nil); err != nil {
				t.Fatalf("reap adopted test descendant %d: %v", pid, err)
			}
		}
		return
	}
	outsider := exec.Command("sleep", "30")
	if err := outsider.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if outsider.ProcessState == nil {
			if err := SignalChild(outsider.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("stop sibling test process: %v", err)
			}
			_ = outsider.Wait()
		}
	})
	dir := t.TempDir()
	marker, pidPath := filepath.Join(dir, "term"), filepath.Join(dir, "descendant.pid")
	helper := exec.Command(os.Args[0], "-test.run=^TestStopDescendantsStopsAdoptedGrandchildAndNotSibling$")
	helper.Env = append(os.Environ(), "PROC_DISABLE_TEST_SUBREAPER=1", "PROC_STOP_DESCENDANTS_HELPER=1", "PROC_STOP_DESCENDANTS_MARKER="+marker, "PROC_STOP_DESCENDANTS_PID="+pidPath)
	if output, err := helper.CombinedOutput(); err != nil {
		t.Fatalf("subreaper helper: %v: %s", err, output)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("adopted descendant did not run TERM trap: %v", err)
	}
	if st, err := ReadStat("/proc", outsider.Process.Pid); err != nil || st.State == "Z" || st.State == "X" {
		t.Fatalf("sibling process was affected: stat=%+v err=%v", st, err)
	}
}

func TestStopOwnedProcessesPreflightsPidfdsBeforeAnySignal(t *testing.T) {
	oldSignal := signalProcess
	signals := 0
	signalProcess = func(int, *trackedProcess, unix.Signal) error { signals++; return nil }
	t.Cleanup(func() { signalProcess = oldSignal })
	openedFD := -1
	openCount := 0
	ops := ownedStopOps{
		scan: func() ([]int, error) { return []int{40, 41}, nil },
		open: func(pid int) (*trackedOwnedProcess, error) {
			openCount++
			if pid == 41 {
				return nil, fmt.Errorf("second target: %w", ErrPidfdUnavailable)
			}
			fd, err := unix.Open("/dev/null", unix.O_RDONLY, 0)
			if err != nil {
				return nil, err
			}
			openedFD = fd
			return &trackedOwnedProcess{identity: processIdentity{pid: pid, start: "1"}, tracked: &trackedProcess{fd: fd, start: "1"}}, nil
		},
	}
	_, err := stopOwnedProcesses(0, syscall.SIGTERM, ops)
	if !errors.Is(err, ErrPidfdUnavailable) {
		t.Fatalf("stopOwnedProcesses error = %v, want ErrPidfdUnavailable", err)
	}
	if signals != 0 {
		t.Fatalf("signals = %d, want 0", signals)
	}
	if openedFD >= 0 {
		if _, err := unix.FcntlInt(uintptr(openedFD), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
			t.Fatalf("preflight fd remains open after отказ: %v", err)
		}
	}
}

func TestSignalDescendantSignalsGrandchild(t *testing.T) {
	dir := t.TempDir()
	marker, ready, pidPath := filepath.Join(dir, "term"), filepath.Join(dir, "ready"), filepath.Join(dir, "pid")
	if os.Getenv("PROC_SIGNAL_DESCENDANT_TARGET") == "1" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		defer signal.Stop(signals)
		if err := os.WriteFile(os.Getenv("PROC_SIGNAL_DESCENDANT_READY"), []byte("ready\n"), 0600); err != nil {
			t.Fatal(err)
		}
		<-signals
		if err := os.WriteFile(os.Getenv("PROC_SIGNAL_DESCENDANT_MARKER"), []byte("term\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	script := `"$1" -test.run=^TestSignalDescendantSignalsGrandchild$ & child=$!; echo "$child" > "$2"; wait`
	cmd := exec.Command("sh", "-c", script, "sh", os.Args[0], pidPath)
	cmd.Env = append(os.Environ(), "PROC_SIGNAL_DESCENDANT_TARGET=1", "PROC_SIGNAL_DESCENDANT_READY="+ready, "PROC_SIGNAL_DESCENDANT_MARKER="+marker)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	targetPID := 0
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			if targetPID > 1 {
				_ = SignalDescendant(targetPID, syscall.SIGKILL)
			}
			_ = SignalChild(cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	waitReady(t, ready)
	targetPID = readPID(t, pidPath)
	if err := SignalDescendant(targetPID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for test-owned parent: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("grandchild did not receive SIGTERM: %v", err)
	}
}

func TestSignalDescendantRejectsNonDescendantAlive(t *testing.T) {
	if os.Getenv("PROC_SIGNAL_DESCENDANT_OUTSIDER_HELPER") == "1" {
		pid, err := strconv.Atoi(os.Getenv("PROC_SIGNAL_DESCENDANT_OUTSIDER_PID"))
		if err != nil {
			t.Fatal(err)
		}
		if err := SignalDescendant(pid, syscall.SIGTERM); err == nil {
			t.Fatal("accepted non-descendant")
		}
		oldSignal := signalProcess
		calls := 0
		signalProcess = func(int, *trackedProcess, unix.Signal) error { calls++; return nil }
		defer func() { signalProcess = oldSignal }()
		if err := SignalDescendant(os.Getpid(), syscall.SIGTERM); err == nil {
			t.Fatal("accepted caller PID")
		}
		if calls != 0 {
			t.Fatalf("caller signal attempts = %d, want 0", calls)
		}
		return
	}
	outsider := exec.Command("sleep", "30")
	if err := outsider.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if outsider.ProcessState == nil {
			_ = SignalChild(outsider.Process.Pid, syscall.SIGKILL)
			_ = outsider.Wait()
		}
	})
	helper := exec.Command(os.Args[0], "-test.run=^TestSignalDescendantRejectsNonDescendantAlive$")
	helper.Env = append(os.Environ(), "PROC_SIGNAL_DESCENDANT_OUTSIDER_HELPER=1", "PROC_SIGNAL_DESCENDANT_OUTSIDER_PID="+strconv.Itoa(outsider.Process.Pid))
	if output, err := helper.CombinedOutput(); err != nil {
		t.Fatalf("non-descendant helper: %v: %s", err, output)
	}
	if st, err := ReadStat("/proc", outsider.Process.Pid); err != nil || st.State == "Z" || st.State == "X" {
		t.Fatalf("non-descendant was affected: stat=%+v err=%v", st, err)
	}
}

func TestSignalDescendantRejectsStartChangeAfterPidfdOpen(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = SignalChild(cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	changed := false
	postOpenReads := 0
	oldTarget, oldAfter := readCommandTargetStat, afterDescendantPidfdOpen
	readCommandTargetStat = func(root string, pid int) (Stat, error) {
		stat, err := ReadStat(root, pid)
		if err == nil && changed && pid == cmd.Process.Pid {
			postOpenReads++
			if postOpenReads == 1 {
				stat.Start = "0"
			}
		}
		return stat, err
	}
	afterDescendantPidfdOpen = func(pid int) {
		if pid == cmd.Process.Pid {
			changed = true
		}
	}
	t.Cleanup(func() { readCommandTargetStat, afterDescendantPidfdOpen = oldTarget, oldAfter })
	if err := SignalDescendant(cmd.Process.Pid, syscall.SIGTERM); err == nil {
		t.Fatal("accepted changed descendant identity")
	}
	if st, err := ReadStat("/proc", cmd.Process.Pid); err != nil || st.State == "Z" || st.State == "X" {
		t.Fatalf("changed descendant was signaled: stat=%+v err=%v", st, err)
	}
}

func TestStopSessionStopsLiveSessionAndLeavesOtherSession(t *testing.T) {
	dir := t.TempDir()
	marker, ready, memberFile := filepath.Join(dir, "term"), filepath.Join(dir, "ready"), filepath.Join(dir, "member.pid")
	cmd := exec.Command("sh", "-c", `trap 'echo term > "$1"; exit 0' TERM; echo ready > "$3"; sleep 30 & echo $! > "$2"; wait`, "sh", marker, memberFile, ready)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	rec := sessionRecordFor(t, cmd.Process.Pid)
	waitReady(t, ready)
	outsider := exec.Command("sleep", "30")
	outsider.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := outsider.Start(); err != nil {
		_ = SignalChild(cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		t.Fatal(err)
	}
	outsiderRec := sessionRecordFor(t, outsider.Process.Pid)
	t.Cleanup(func() {
		if outsider.ProcessState == nil {
			_, _ = StopSession(outsiderRec, 0, syscall.SIGKILL)
			_ = outsider.Wait()
		}
	})
	if _, err := StopSession(rec, 100*time.Millisecond, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("session leader trap exit: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("session leader TERM trap was not observed: %v", err)
	}
	member := readPID(t, memberFile)
	waitForGoneOrZombie(t, member)
	if st, err := ReadStat("/proc", outsider.Process.Pid); err != nil || st.State == "Z" || st.State == "X" {
		t.Fatalf("other session was affected: stat=%+v err=%v", st, err)
	}
}

func TestStopSessionRefusesMembersAfterLeaderExits(t *testing.T) {
	dir := t.TempDir()
	marker, ready, memberFile := filepath.Join(dir, "term"), filepath.Join(dir, "ready"), filepath.Join(dir, "member.pid")
	script := `sh -c 'trap "echo term > $1" TERM; echo ready > $2; while :; do :; done' sh "$1" "$2" & child=$!; while [ ! -f "$2" ]; do sleep 0.01; done; echo $child > "$3"; exit 0`
	cmd := exec.Command("sh", "-c", script, "sh", marker, ready, memberFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	rec := sessionRecordFor(t, cmd.Process.Pid)
	waitReady(t, ready)
	member := readPID(t, memberFile)
	memberFD, err := unix.PidfdOpen(member, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(memberFD) })
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := StopSession(rec, 100*time.Millisecond, syscall.SIGTERM); !errors.Is(err, ErrSessionLeaderGone) {
		t.Fatalf("StopSession error = %v, want ErrSessionLeaderGone", err)
	}
	if err := unix.PidfdSendSignal(memberFD, unix.SIGKILL, nil, 0); err != nil {
		t.Fatalf("stop test-owned member after refusal: %v", err)
	}
	waitForGoneOrZombie(t, member)
}

func TestStopSessionRefusesReusedSIDWhenLeaderIsGone(t *testing.T) {
	root := t.TempDir()
	writeFakeSessionStat(t, root, 1, "init", "S", 0, 1, 1, "1")
	writeFakeSessionStat(t, root, 20, "caller", "S", 1, 20, 20, "20")
	writeFakeSessionStat(t, root, 51, "replacement member", "S", 1, 50, 50, "200")
	err := validateSessionTarget(root, 20, SessionRecord{Pid: 50, Start: "42"}, 42)
	if !errors.Is(err, ErrSessionLeaderGone) {
		t.Fatalf("validateSessionTarget error = %v, want ErrSessionLeaderGone", err)
	}
}

func TestStopSessionRefusesUnsafeRecords(t *testing.T) {
	t.Run("missing start", func(t *testing.T) {
		for _, start := range []string{"", "0"} {
			if _, err := StopSession(SessionRecord{Pid: 2, Start: start}, 0, syscall.SIGTERM); !errors.Is(err, ErrNoStartRecord) {
				t.Fatalf("Start %q error = %v, want ErrNoStartRecord", start, err)
			}
		}
	})
	t.Run("reserved pid", func(t *testing.T) {
		if err := validateSessionPID(1); err == nil {
			t.Fatal("accepted PID 1")
		}
	})
	for _, tc := range []struct {
		name      string
		selfSID   int
		parentSID int
		leader    string
		leaderSID int
		start     string
		wantErr   bool
	}{
		{name: "caller session", selfSID: 50, parentSID: 50, leader: "worker", leaderSID: 50, start: "42", wantErr: true},
		{name: "ancestor session", selfSID: 55, parentSID: 50, leader: "worker", leaderSID: 50, start: "42", wantErr: true},
		{name: "system manager", selfSID: 55, parentSID: 55, leader: "systemd", leaderSID: 50, start: "42", wantErr: true},
		{name: "start mismatch", selfSID: 55, parentSID: 55, leader: "worker", leaderSID: 50, start: "43", wantErr: true},
		{name: "not session leader", selfSID: 55, parentSID: 55, leader: "worker", leaderSID: 51, start: "42", wantErr: true},
		{name: "safe target", selfSID: 55, parentSID: 55, leader: "worker", leaderSID: 50, start: "42"},
		{name: "dead leader", selfSID: 55, parentSID: 55, leader: "worker", leaderSID: 50, start: "42", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFakeSessionStat(t, root, 1, "init", "S", 0, 1, 1, "1")
			writeFakeSessionStat(t, root, 10, "systemd", "S", 1, 10, 10, "10")
			writeFakeSessionStat(t, root, 20, "caller", "S", 30, 20, tc.selfSID, "20")
			writeFakeSessionStat(t, root, 30, "parent", "S", 10, 30, tc.parentSID, "30")
			state := "S"
			if tc.name == "dead leader" {
				state = "Z"
			}
			writeFakeSessionStat(t, root, 50, tc.leader, state, 10, 50, tc.leaderSID, tc.start)
			err := validateSessionTarget(root, 20, SessionRecord{Pid: 50, Start: "42"}, 42)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateSessionTarget error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestStopSessionRejectsLiveLeaderRecordMismatchesWithoutSignals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setsid bool
	}{
		{name: "wrong start", setsid: true},
		{name: "not session leader", setsid: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sleep", "30")
			if tc.setsid {
				cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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
			rec := sessionRecordFor(t, cmd.Process.Pid)
			if tc.name == "wrong start" {
				start, _ := numericStart(rec.Start)
				rec.Start = strconv.FormatUint(start+1, 10)
			}
			realSignal := signalProcess
			calls := 0
			signalProcess = func(int, *trackedProcess, unix.Signal) error { calls++; return nil }
			t.Cleanup(func() { signalProcess = realSignal })
			if _, err := StopSession(rec, 0, syscall.SIGTERM); err == nil {
				t.Fatal("accepted mismatched session leader")
			}
			if calls != 0 {
				t.Fatalf("signaled before refusing mismatched leader: calls=%d", calls)
			}
			if st, err := ReadStat("/proc", cmd.Process.Pid); err != nil || st.State == "Z" || st.State == "X" {
				t.Fatalf("mismatched session leader was affected: stat=%+v err=%v", st, err)
			}
			signalProcess = realSignal
			if err := SignalChild(cmd.Process.Pid, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
		})
	}
}

func TestSessionPIDsUsesNumericStartTime(t *testing.T) {
	root := t.TempDir()
	writeFakeSessionStat(t, root, 1, "init", "S", 0, 1, 1, "1")
	writeFakeSessionStat(t, root, 60, "old", "S", 20, 50, 50, "99")
	writeFakeSessionStat(t, root, 61, "newer", "S", 20, 50, 50, "101")
	writeFakeSessionStat(t, root, 62, "same", "S", 20, 50, 50, "100")
	pids, err := sessionPIDs(root, 20, 50, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pids, []int{61, 62}) {
		t.Fatalf("session PIDs = %v, want [61 62]", pids)
	}
}

func TestOwnedSessionTargetRechecksStartAndSIDAfterPidfdOpen(t *testing.T) {
	for _, change := range []string{"start", "sid"} {
		t.Run(change, func(t *testing.T) {
			cmd := exec.Command("sleep", "30")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = SignalChild(cmd.Process.Pid, syscall.SIGKILL)
					_ = cmd.Wait()
				}
			})
			stat, err := ReadStat("/proc", cmd.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			realRead, realAfter := readSessionStat, afterSessionPidfdOpen
			t.Cleanup(func() { readSessionStat, afterSessionPidfdOpen = realRead, realAfter })
			readSessionStat = ReadStat
			afterSessionPidfdOpen = func(pid int) {
				if pid != cmd.Process.Pid {
					return
				}
				readSessionStat = func(root string, got int) (Stat, error) {
					result, err := ReadStat(root, got)
					if got == pid && change == "start" {
						result.Start = "1"
					}
					if got == pid && change == "sid" {
						result.Sid++
					}
					return result, err
				}
			}
			if _, err := ownedSessionTarget("/proc", cmd.Process.Pid, 1, cmd.Process.Pid); !errors.Is(err, errStopTargetChanged) {
				t.Fatalf("ownedSessionTarget error = %v, want changed identity (actual start %s)", err, stat.Start)
			}
		})
	}
}

func TestStopSessionRechecksLeaderAfterPidfdOpenBeforeMembers(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = SignalChild(cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	rec := sessionRecordFor(t, cmd.Process.Pid)
	oldRead, oldAfter, oldSignal := readSessionStat, afterSessionPidfdOpen, signalProcess
	changed := false
	readSessionStat = func(root string, pid int) (Stat, error) {
		st, err := ReadStat(root, pid)
		if changed && pid == rec.Pid && err == nil {
			st.Start = "1"
		}
		return st, err
	}
	afterSessionPidfdOpen = func(pid int) {
		if pid == rec.Pid {
			changed = true
		}
	}
	signals := 0
	signalProcess = func(int, *trackedProcess, unix.Signal) error { signals++; return nil }
	t.Cleanup(func() { readSessionStat, afterSessionPidfdOpen, signalProcess = oldRead, oldAfter, oldSignal })
	if _, err := StopSession(rec, 0, syscall.SIGTERM); !errors.Is(err, errStopTargetChanged) {
		t.Fatalf("StopSession error = %v, want changed leader identity", err)
	}
	if signals != 0 {
		t.Fatalf("signals = %d, want 0 before validating all session members", signals)
	}
}

func TestOwnedDescendantTargetRechecksParentChainAndStartAfterPidfdOpen(t *testing.T) {
	for _, change := range []string{"parent chain", "start time"} {
		t.Run(change, func(t *testing.T) {
			cmd := exec.Command("sleep", "30")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = SignalChild(cmd.Process.Pid, syscall.SIGKILL)
					_ = cmd.Wait()
				}
			})
			var peer *exec.Cmd
			if change == "parent chain" {
				peer = exec.Command("sleep", "30")
				if err := peer.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if peer.ProcessState == nil {
						_ = SignalChild(peer.Process.Pid, syscall.SIGKILL)
						_ = peer.Wait()
					}
				})
			}
			realAncestry, realTarget, realAfter := readCommandAncestryStat, readCommandTargetStat, afterDescendantPidfdOpen
			t.Cleanup(func() {
				readCommandAncestryStat, readCommandTargetStat, afterDescendantPidfdOpen = realAncestry, realTarget, realAfter
			})
			afterDescendantPidfdOpen = func(pid int) {
				if pid != cmd.Process.Pid {
					return
				}
				if change == "parent chain" {
					readCommandAncestryStat = func(root string, got int) (Stat, error) {
						stat, err := ReadStat(root, got)
						if got == pid {
							stat.Ppid = peer.Process.Pid
						}
						return stat, err
					}
				} else {
					readCommandTargetStat = func(root string, got int) (Stat, error) {
						stat, err := ReadStat(root, got)
						if got == pid {
							stat.Start = "0"
						}
						return stat, err
					}
				}
			}
			if _, err := ownedDescendantTarget("/proc", os.Getpid(), cmd.Process.Pid); !errors.Is(err, errStopTargetChanged) {
				t.Fatalf("ownedDescendantTarget error = %v, want changed identity", err)
			}
		})
	}
}

func sessionRecordFor(t *testing.T, pid int) SessionRecord {
	t.Helper()
	stat, err := ReadStat("/proc", pid)
	if err != nil {
		t.Fatal(err)
	}
	return SessionRecord{Pid: pid, Start: stat.Start}
}

func writeFakeSessionStat(t *testing.T, root string, pid int, name, state string, ppid, pgid, sid int, start string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	fields := []string{state, strconv.Itoa(ppid), strconv.Itoa(pgid), strconv.Itoa(sid)}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, start)
	line := fmt.Sprintf("%d (%s) %s\n", pid, name, strings.Join(fields, " "))
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
}
