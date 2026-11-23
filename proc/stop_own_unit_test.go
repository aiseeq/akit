//go:build linux

package proc

import (
	"context"
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

func ownUnitFixture(t *testing.T, leaf, targetCgroup string, target, caller, callerParent int, targetUID int) (string, string) {
	t.Helper()
	procRoot, cgRoot := t.TempDir(), t.TempDir()
	cg := "/user.slice/user-1000.slice/user@1000.service/agents.slice/" + leaf
	if strings.HasPrefix(leaf, "run-") || leaf == "foo.service" {
		cg = "/user.slice/user-1000.slice/user@1000.service/app.slice/" + leaf
	}
	if strings.HasPrefix(leaf, "worker-") {
		cg = "/user.slice/user-1000.slice/user@1000.service/app.slice/" + leaf
	}
	if targetCgroup == "" {
		targetCgroup = cg
	}
	add := func(pid, ppid, uid int, path string) {
		fakeStat(t, procRoot, pid, ppid, "sleep", "S")
		writeTestFile(t, filepath.Join(procRoot, fmt.Sprint(pid), "status"), fmt.Sprintf("Name:\tsleep\nTgid:\t%d\nUid:\t%d\t%d\t%d\t%d\n", pid, uid, uid+1, uid+1, uid+1))
		writeTestFile(t, filepath.Join(procRoot, fmt.Sprint(pid), "cgroup"), "0::"+path+"\n")
	}
	add(1, 0, 0, "/")
	fakeStat(t, procRoot, callerParent, 1, "systemd", "S")
	writeTestFile(t, filepath.Join(procRoot, fmt.Sprint(callerParent), "status"), fmt.Sprintf("Name:\tsystemd\nTgid:\t%d\nUid:\t1000\t1001\t1001\t1001\n", callerParent))
	writeTestFile(t, filepath.Join(procRoot, fmt.Sprint(callerParent), "cgroup"), "0::"+cg+"\n")
	add(caller, callerParent, 1000, cg)
	if target != callerParent {
		add(target, callerParent, targetUID, targetCgroup)
	}
	procs := fmt.Sprintf("%d\n", target)
	if target == 500 {
		add(target+1, target, targetUID, targetCgroup)
		add(target+2, callerParent, targetUID, targetCgroup)
		add(target+3, target, targetUID+1000, targetCgroup)
		procs += fmt.Sprintf("%d\n%d\n%d\n", target+1, target+2, target+3)
	}
	writeTestFile(t, filepath.Join(procRoot, "self", "cgroup"), "0::"+cg+"\n")
	writeTestFile(t, filepath.Join(cgRoot, strings.TrimPrefix(cg, "/"), "cgroup.procs"), procs)
	return procRoot, cgRoot
}

func TestStopOwnUnitEligibility(t *testing.T) {
	tests := []struct {
		name, leaf, targetCG, callerCG, reason string
		target, uid                            int
	}{
		{name: "success with reparented target", leaf: "agent-demo-99.scope", target: 500, uid: 1000},
		{name: "run scope", leaf: "run-example.scope", target: 500, uid: 1000},
		{name: "worker scope", leaf: "worker-example.scope", target: 500, uid: 1000},
		{name: "worker service", leaf: "worker-example.service", target: 500, uid: 1000},
		{name: "different scope", leaf: "agent-demo-99.scope", targetCG: "/user.slice/user@1000.service/agents.slice/agent-other-12.scope", target: 500, uid: 1000, reason: "different cgroup"},
		{name: "session scope", leaf: "session-1.scope", target: 500, uid: 1000, reason: "not an agent scope"},
		{name: "service leaf", leaf: "foo.service", target: 500, uid: 1000, reason: "not an agent scope"},
		{name: "agent leaf outside agents slice", leaf: "agent-demo-99.scope", callerCG: "/user.slice/user-1000.slice/user@1000.service/app.slice/agent-demo-99.scope", target: 500, uid: 1000, reason: "not an agent scope"},
		{name: "run scope outside app slice", leaf: "run-example.scope", callerCG: "/user.slice/user-1000.slice/user@1000.service/agents.slice/run-example.scope", target: 500, uid: 1000, reason: "not an agent scope"},
		{name: "run service leaf", leaf: "run-example.service", target: 500, uid: 1000, reason: "not an agent scope"},
		{name: "caller ancestor", leaf: "agent-demo-99.scope", target: 3431, uid: 1000, reason: "ancestor"},
		{name: "scope cl pid", leaf: "agent-demo-500.scope", target: 500, uid: 1000, reason: "scope launcher"},
		{name: "different uid", leaf: "agent-demo-99.scope", target: 500, uid: 2000, reason: "different uid"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			procRoot, cgRoot := ownUnitFixture(t, tc.leaf, tc.targetCG, tc.target, 600, 3431, tc.uid)
			if tc.callerCG != "" {
				writeTestFile(t, filepath.Join(procRoot, "600", "cgroup"), "0::"+tc.callerCG+"\n")
			}
			oldOpen, oldSignal := openProcessPidfd, signalProcess
			signals := 0
			openProcessPidfd = func(int, int) (int, error) { return unix.Open("/dev/null", unix.O_RDONLY, 0) }
			signalProcess = func(int, *trackedProcess, unix.Signal) error { signals++; return nil }
			t.Cleanup(func() { openProcessPidfd, signalProcess = oldOpen, oldSignal })
			_, err := stopOwnUnit(procRoot, cgRoot, 600, tc.target, 1000, 0, syscall.SIGKILL)
			if tc.reason == "" {
				if err != nil || signals != 2 {
					t.Fatalf("StopOwnUnit = (%v), signals=%d; want success and target plus descendant signals", err, signals)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.reason) || signals != 0 {
				t.Fatalf("StopOwnUnit error=%v, signals=%d; want reason %q and no signals", err, signals, tc.reason)
			}
		})
	}
}

func TestStopOwnUnitWorkerScopeMatchesManagerUID(t *testing.T) {
	procRoot, _ := ownUnitFixture(t, "worker-example.scope", "", 500, 600, 3431, 1000)
	writeTestFile(t, filepath.Join(procRoot, "600", "cgroup"), "0::/user.slice/user-1000.slice/user@2000.service/app.slice/worker-example.scope\n")
	if _, _, err := approvedOwnUnitCgroup(procRoot, 600, 1000); err == nil || !strings.Contains(err.Error(), "not an agent scope") {
		t.Fatalf("worker scope with another user manager error = %v; want not an agent scope", err)
	}
}

func TestStopOwnUnitRejectsTargetWithNonManagerParent(t *testing.T) {
	procRoot, cgRoot := ownUnitFixture(t, "agent-demo-99.scope", "", 500, 600, 3431, 1000)
	fakeStat(t, procRoot, 3431, 1, "claude", "S")
	oldOpen, oldSignal := openProcessPidfd, signalProcess
	signals := 0
	openProcessPidfd = func(int, int) (int, error) { return unix.Open("/dev/null", unix.O_RDONLY, 0) }
	signalProcess = func(int, *trackedProcess, unix.Signal) error { signals++; return nil }
	t.Cleanup(func() { openProcessPidfd, signalProcess = oldOpen, oldSignal })
	_, err := stopOwnUnit(procRoot, cgRoot, 600, 500, 1000, 0, syscall.SIGTERM)
	if err == nil || !strings.Contains(err.Error(), "not a process manager") || signals != 0 {
		t.Fatalf("StopOwnUnit error=%v signals=%d; want manager-parent refusal", err, signals)
	}
}

func TestStopOwnUnitRejectsTID(t *testing.T) {
	procRoot, cgRoot := ownUnitFixture(t, "agent-demo-99.scope", "", 500, 600, 3431, 1000)
	writeTestFile(t, filepath.Join(procRoot, "500", "status"), "Name:\tsleep\nTgid:\t480\nUid:\t1000\t1001\t1001\t1001\n")
	oldOpen, oldSignal := openProcessPidfd, signalProcess
	signals := 0
	openProcessPidfd = func(int, int) (int, error) { return unix.Open("/dev/null", unix.O_RDONLY, 0) }
	signalProcess = func(int, *trackedProcess, unix.Signal) error { signals++; return nil }
	t.Cleanup(func() { openProcessPidfd, signalProcess = oldOpen, oldSignal })
	_, err := stopOwnUnit(procRoot, cgRoot, 600, 500, 1000, 0, syscall.SIGTERM)
	if err == nil || !strings.Contains(err.Error(), "TID") || signals != 0 {
		t.Fatalf("StopOwnUnit error=%v signals=%d; want TID refusal", err, signals)
	}
}

func TestStopOwnUnitRejectsPIDOne(t *testing.T) {
	procRoot, cgRoot := ownUnitFixture(t, "agent-demo-99.scope", "", 500, 600, 3431, 1000)
	_, err := stopOwnUnit(procRoot, cgRoot, 600, 1, 1000, 0, syscall.SIGTERM)
	if err == nil || !strings.Contains(err.Error(), "refusing caller or invalid PID") {
		t.Fatalf("StopOwnUnit PID 1 error = %v; want invalid PID refusal", err)
	}
}

func TestFinishPinnedOwnUnitSkipsExitedProcess(t *testing.T) {
	fds := []int{0, 0}
	if err := unix.Pipe(fds); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	if err := unix.Close(fds[1]); err != nil {
		t.Fatal(err)
	}
	entry := &trackedOwnedProcess{
		identity:  processIdentity{pid: 500, start: "42"},
		tracked:   &trackedProcess{fd: fds[0], start: "42"},
		requested: true,
	}
	oldSignal := signalProcess
	signals := 0
	signalProcess = func(int, *trackedProcess, unix.Signal) error { signals++; return nil }
	t.Cleanup(func() { signalProcess = oldSignal })
	result := StopResult{}
	killed := killOwnedCandidates([]*trackedOwnedProcess{entry}, syscall.SIGTERM, ownedStopOps{
		killVerify:           func(int, string) error { return nil },
		checkAliveBeforeKill: true,
	}, &stopFailures{}, &result)
	if killed != 0 || signals != 0 || result.Killed != 0 {
		t.Fatalf("finishPinnedOwnUnit = killed %d, signals %d, result %+v; want no signal for exited process", killed, signals, result)
	}
}

func TestStopOwnUnitRejectsPIDReuseAfterPidfdOpen(t *testing.T) {
	procRoot, _ := ownUnitFixture(t, "agent-demo-99.scope", "", 500, 600, 3431, 1000)
	oldOpen, oldSignal := openProcessPidfd, signalProcess
	opened, signals := false, 0
	openProcessPidfd = func(pid, _ int) (int, error) {
		fd, err := unix.Open("/dev/null", unix.O_RDONLY, 0)
		if pid == 500 && !opened {
			opened = true
			fakeStatStart(t, procRoot, 500, 3431, "reused", "999")
		}
		return fd, err
	}
	signalProcess = func(int, *trackedProcess, unix.Signal) error { signals++; return nil }
	t.Cleanup(func() { openProcessPidfd, signalProcess = oldOpen, oldSignal })
	cg, clPID, err := approvedOwnUnitCgroup(procRoot, 600, 1000)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ownUnitDescendantTarget(procRoot, 600, 500, 500, cg, clPID, 1000)
	if err == nil || !strings.Contains(err.Error(), "changed during validation") || signals != 0 {
		t.Fatalf("ownUnitDescendantTarget error=%v, signals=%d; want PID reuse rejection and no signal", err, signals)
	}
}

func TestStopOwnUnitRevalidatesAfterPidfdOpen(t *testing.T) {
	procRoot, _ := ownUnitFixture(t, "agent-demo-99.scope", "", 500, 600, 3431, 1000)
	cg, clPID, err := approvedOwnUnitCgroup(procRoot, 600, 1000)
	if err != nil {
		t.Fatal(err)
	}
	oldOpen, oldAfterOpen := openProcessPidfd, afterDescendantPidfdOpen
	openProcessPidfd = func(int, int) (int, error) { return unix.Open("/dev/null", unix.O_RDONLY, 0) }
	afterDescendantPidfdOpen = func(pid int) {
		writeTestFile(t, filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"), "0::/user.slice/other.scope\n")
	}
	t.Cleanup(func() { openProcessPidfd, afterDescendantPidfdOpen = oldOpen, oldAfterOpen })
	_, err = ownUnitDescendantTarget(procRoot, 600, 500, 500, cg, clPID, 1000)
	if err == nil || !strings.Contains(err.Error(), "different cgroup") {
		t.Fatalf("ownUnitDescendantTarget after cgroup change = %v; want repeated membership refusal", err)
	}
}

func TestStopOwnUnitRevalidatesAncestryAfterPidfdOpen(t *testing.T) {
	procRoot, _ := ownUnitFixture(t, "agent-demo-99.scope", "", 500, 600, 3431, 1000)
	cg, clPID, err := approvedOwnUnitCgroup(procRoot, 600, 1000)
	if err != nil {
		t.Fatal(err)
	}
	oldOpen, oldAfterOpen := openProcessPidfd, afterDescendantPidfdOpen
	openProcessPidfd = func(int, int) (int, error) { return unix.Open("/dev/null", unix.O_RDONLY, 0) }
	afterDescendantPidfdOpen = func(pid int) {
		if pid == 501 {
			fakeStatStart(t, procRoot, 504, 500, "sleep", "42")
			fakeStatStart(t, procRoot, pid, 504, "sleep", "42")
		}
	}
	t.Cleanup(func() { openProcessPidfd, afterDescendantPidfdOpen = oldOpen, oldAfterOpen })
	_, err = ownUnitDescendantTarget(procRoot, 600, 500, 501, cg, clPID, 1000)
	if err == nil || !strings.Contains(err.Error(), "changed during validation") {
		t.Fatalf("ownUnitDescendantTarget after ancestry change = %v; want ancestry refusal", err)
	}
}

func TestStopOwnUnitRevalidatesStartAfterAncestryRead(t *testing.T) {
	procRoot, _ := ownUnitFixture(t, "agent-demo-99.scope", "", 500, 600, 3431, 1000)
	cg, clPID, err := approvedOwnUnitCgroup(procRoot, 600, 1000)
	if err != nil {
		t.Fatal(err)
	}
	oldOpen, oldAfterAncestry := openProcessPidfd, afterOwnUnitAncestryRead
	openProcessPidfd = func(int, int) (int, error) { return unix.Open("/dev/null", unix.O_RDONLY, 0) }
	afterOwnUnitAncestryRead = func(pid int) {
		if pid == 500 {
			fakeStatStart(t, procRoot, pid, 3431, "sleep", "999")
		}
	}
	t.Cleanup(func() { openProcessPidfd, afterOwnUnitAncestryRead = oldOpen, oldAfterAncestry })
	_, err = ownUnitDescendantTarget(procRoot, 600, 500, 500, cg, clPID, 1000)
	if err == nil || !strings.Contains(err.Error(), "changed during validation") {
		t.Fatalf("ownUnitDescendantTarget after stat change = %v; want PID reuse refusal", err)
	}
}

func TestStopOwnUnitRevalidatesPIDBeforeSignal(t *testing.T) {
	procRoot, _ := ownUnitFixture(t, "agent-demo-99.scope", "", 500, 600, 3431, 1000)
	cg, clPID, err := approvedOwnUnitCgroup(procRoot, 600, 1000)
	if err != nil {
		t.Fatal(err)
	}
	oldOpen, oldSignal := openProcessPidfd, signalProcess
	openProcessPidfd = func(int, int) (int, error) { return unix.Open("/dev/null", unix.O_RDONLY, 0) }
	signals, replaced := 0, false
	signalProcess = func(int, *trackedProcess, unix.Signal) error { signals++; return nil }
	t.Cleanup(func() { openProcessPidfd, signalProcess = oldOpen, oldSignal })
	ops := ownedStopOps{
		scan: func() ([]int, error) { return []int{500}, nil },
		open: func(pid int) (*trackedOwnedProcess, error) {
			return ownUnitDescendantTarget(procRoot, 600, 500, pid, cg, clPID, 1000)
		},
		verify: func(pid int, start string) error {
			if !replaced {
				replaced = true
				fakeStatStart(t, procRoot, pid, 3431, "reused", "999")
			}
			return revalidateOwnUnitDescendant(procRoot, 600, 500, pid, cg, clPID, 1000, start)
		},
	}
	result, err := stopOwnedProcesses(0, syscall.SIGKILL, ops)
	if err != nil || result.Signaled != 0 || signals != 0 {
		t.Fatalf("stop after PID reuse = (%+v, %v), signals=%d; want no signal", result, err, signals)
	}
}

func TestStopOwnUnitRevalidatesCgroupBeforeSignal(t *testing.T) {
	procRoot, _ := ownUnitFixture(t, "agent-demo-99.scope", "", 500, 600, 3431, 1000)
	cg, clPID, err := approvedOwnUnitCgroup(procRoot, 600, 1000)
	if err != nil {
		t.Fatal(err)
	}
	oldOpen, oldSignal := openProcessPidfd, signalProcess
	openProcessPidfd = func(int, int) (int, error) { return unix.Open("/dev/null", unix.O_RDONLY, 0) }
	signals := 0
	signalProcess = func(int, *trackedProcess, unix.Signal) error { signals++; return nil }
	t.Cleanup(func() { openProcessPidfd, signalProcess = oldOpen, oldSignal })
	ops := ownedStopOps{
		scan: func() ([]int, error) { return []int{500}, nil },
		open: func(pid int) (*trackedOwnedProcess, error) {
			return ownUnitDescendantTarget(procRoot, 600, 500, pid, cg, clPID, 1000)
		},
		verify: func(pid int, start string) error {
			writeTestFile(t, filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"), "0::/user.slice/other.scope\n")
			return revalidateOwnUnitDescendant(procRoot, 600, 500, pid, cg, clPID, 1000, start)
		},
	}
	result, err := stopOwnedProcesses(0, syscall.SIGTERM, ops)
	if err == nil || !strings.Contains(err.Error(), "different cgroup") || result.Signaled != 0 || signals != 0 {
		t.Fatalf("stop after cgroup change = (%+v, %v), signals=%d; want cgroup refusal and no signal", result, err, signals)
	}
}

func TestStopOwnUnitLiveScope(t *testing.T) {
	if os.Getenv("AKIT_LIVE_OWN_UNIT_HELPER") == "1" {
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0); err != nil {
			t.Fatalf("disable test subreaper for systemd adoption: %v", err)
		}
		tmp := filepath.Dir(os.Getenv("AKIT_LIVE_OWN_UNIT_PID"))
		pstop := os.Getenv("AKIT_PSTOP_PATH")
		fastPIDFile := filepath.Join(tmp, "fast.pid")
		fastLauncher := exec.Command("sh", "-c", "sleep 60 >/dev/null 2>&1 & echo $! > \"$1\"", "sh", fastPIDFile)
		if output, err := fastLauncher.CombinedOutput(); err != nil {
			t.Fatalf("start fast-exit process: %v: %s", err, output)
		}
		fastPID := readTestPID(t, fastPIDFile)
		checkReparentedTestProcess(t, fastPID, fastLauncher.Process.Pid)
		fastStart, fastCG := pinOwnUnitTestProcess(t, fastPID)
		defer cleanupOwnUnitTestProcess(fastPID, fastStart, fastCG)
		grace := 2 * time.Second
		started := time.Now()
		output, err := exec.Command(pstop, "--own-unit", "--grace", grace.String(), "--signal", "TERM", strconv.Itoa(fastPID)).CombinedOutput()
		elapsed := time.Since(started)
		if err != nil || elapsed >= grace || strings.Contains(string(output), "killed") {
			t.Fatalf("fast pstop = %v after %s: %s; want exit before %s", err, elapsed, output, grace)
		}

		childPIDFile := filepath.Join(tmp, "stubborn-child.pid")
		targetPIDFile := filepath.Join(tmp, "target-shell.pid")
		targetScript := filepath.Join(tmp, "target.sh")
		script := "#!/bin/sh\ntrap 'exit 0' TERM\nsh -c 'trap \"\" TERM; exec sleep 60' &\necho $! > \"$1\"\nwait\n"
		if err := os.WriteFile(targetScript, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		launcher := exec.Command("sh", "-c", "sh \"$1\" \"$2\" >/dev/null 2>&1 & echo $! > \"$3\"", "sh", targetScript, childPIDFile, targetPIDFile)
		if output, err := launcher.CombinedOutput(); err != nil {
			t.Fatalf("start target shell: %v: %s", err, output)
		}
		targetPID := readTestPID(t, targetPIDFile)
		childPID := waitTestPIDFile(t, childPIDFile)
		checkReparentedTestProcess(t, targetPID, launcher.Process.Pid)
		targetStart, targetCG := pinOwnUnitTestProcess(t, targetPID)
		childStart, childCG := pinOwnUnitTestProcess(t, childPID)
		if targetCG != childCG {
			t.Fatalf("target and stubborn child cgroups differ: %q != %q", targetCG, childCG)
		}
		defer cleanupOwnUnitTestProcess(targetPID, targetStart, targetCG)
		defer cleanupOwnUnitTestProcess(childPID, childStart, childCG)
		output, err = exec.Command(pstop, "--own-unit", "--grace", "200ms", "--signal", "TERM", strconv.Itoa(targetPID)).CombinedOutput()
		if err != nil || !strings.Contains(string(output), "killed 1 process after 200ms grace") {
			t.Fatalf("pstop target with stubborn child = %v: %s; want SIGKILL of one captured child", err, output)
		}
		if waitTestProcessGone(targetPID, time.Second) != nil || waitTestProcessGone(childPID, time.Second) != nil {
			t.Fatalf("pstop left target or stubborn child alive: target=%d child=%d", targetPID, childPID)
		}
		return
	}
	if os.Getenv("AKIT_LIVE_OWN_UNIT_TEST") != "1" {
		t.Skip("set AKIT_LIVE_OWN_UNIT_TEST=1 to run the disposable user scope test")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0); err != nil {
		t.Fatalf("disable test subreaper for systemd adoption: %v", err)
	}
	systemdRun, err := exec.LookPath("systemd-run")
	if err != nil {
		t.Fatalf("systemd-run is unavailable: %v", err)
	}
	pstop := os.Getenv("AKIT_PSTOP_PATH")
	if pstop == "" {
		pstop, err = exec.LookPath("pstop")
		if err != nil {
			t.Fatalf("pstop is unavailable on PATH: %v", err)
		}
	}
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "sleep.pid")
	unit := fmt.Sprintf("agent-pstoptest-%d.scope", os.Getpid())
	launcherUnit := fmt.Sprintf("agent-pstoptest-launch-%d.service", os.Getpid())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, systemdRun,
		"--user", "--wait", "--unit", launcherUnit,
		"--setenv=AKIT_LIVE_OWN_UNIT_HELPER=1",
		"--setenv=AKIT_LIVE_OWN_UNIT_PID="+pidFile,
		"--setenv=AKIT_PSTOP_PATH="+pstop,
		"--", systemdRun, "--user", "--scope", "--slice=agents.slice", "-u", unit, "--", os.Args[0], "-test.run=^TestStopOwnUnitLiveScope$")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("systemd-run test launcher: %v: %s", err, output)
	}
}

func readTestPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return mustPID(t, strings.TrimSpace(string(data)))
}

func waitTestPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pid, err := os.ReadFile(path); err == nil {
			return mustPID(t, strings.TrimSpace(string(pid)))
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for PID file %s", path)
	return 0
}

func checkReparentedTestProcess(t *testing.T, pid, oldParent int) {
	t.Helper()
	stat, err := ReadStat("/proc", pid)
	if err != nil || stat.Ppid == oldParent {
		t.Fatalf("test process was not reparented: stat=%+v err=%v", stat, err)
	}
	parent, err := ReadStat("/proc", stat.Ppid)
	if err != nil || !serviceManager(stat.Ppid, parent) {
		t.Fatalf("test process parent is not a process manager: target=%+v parent PID %d=%+v err=%v", stat, stat.Ppid, parent, err)
	}
}

func pinOwnUnitTestProcess(t *testing.T, pid int) (string, string) {
	t.Helper()
	stat, err := ReadStat("/proc", pid)
	if err != nil {
		t.Fatal(err)
	}
	cg, err := readProcessCgroup("/proc", pid)
	if err != nil {
		t.Fatal(err)
	}
	callerCG, err := readProcessCgroup("/proc", os.Getpid())
	if err != nil || cg != callerCG {
		t.Fatalf("test process escaped its temporary scope: target=%q caller=%q err=%v", cg, callerCG, err)
	}
	return stat.Start, cg
}

func cleanupOwnUnitTestProcess(pid int, start, cg string) {
	stat, err := ReadStat("/proc", pid)
	if err != nil || stat.Start != start {
		return
	}
	member, err := readProcessCgroup("/proc", pid)
	callerCG, callerErr := readProcessCgroup("/proc", os.Getpid())
	if err != nil || callerErr != nil || member != cg || member != callerCG {
		return
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err == nil {
		_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		_ = unix.Close(fd)
	}
}

func mustPID(t *testing.T, value string) int {
	t.Helper()
	pid, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	return pid
}
