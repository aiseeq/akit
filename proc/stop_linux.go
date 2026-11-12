package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var errStopTargetChanged = errors.New("stop target changed during validation")

// StopChild stops the direct child and descendants observed during tree walks,
// including descendants later reparented to this process or another subreaper.
// It also captures new descendants of tracked processes during grace. Each
// captured pidfd refers to that specific process; signaling it after it exits
// returns ESRCH. request is called once after the initial tree is captured; it
// may signal, close stdin or cancel a context. The caller remains responsible
// for Cmd.Wait and WaitDelay. Descendants that leave the tree before they can
// be observed cannot be found. A failure for one process does not stop the
// rest; the result includes an error naming each failed PID.
func StopChild(pid int, grace time.Duration, request func() error) (StopResult, error) {
	return stopTree("/proc", os.Getpid(), pid, grace, request, 0)
}

// StopChildGroup stops verified descendants in cmd's process tree or process
// group. cmd must have been started by the caller with Start or Run; a Cmd
// assembled around an existing Process is unsupported. Each recipient is
// checked through its parent chain, pinned by pidfd, then checked again before
// each signal. Without BecomeChildSubreaper, descendants reparented to init
// cease to be verifiably ours and are left alone. After signaling, zombies
// among this process's direct children in cmd's process group are reaped, except
// cmd.Process, whose status belongs to cmd.Wait. A separate Cmd whose process
// was placed in this group is outside this contract and may lose its Wait
// status. Adopted zombies in other groups, including descendants that changed
// groups with setsid or setpgid, remain until their owner or process exit reaps
// them.
func StopChildGroup(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) (StopResult, error) {
	pid, err := validateChildGroupCommand(cmd, grace, sig)
	if err != nil {
		return StopResult{}, err
	}
	self := os.Getpid()
	beforeStopChildGroupTree(pid)
	initial, err := discoverCommandTargets("/proc", self, pid)
	if err != nil {
		return StopResult{}, err
	}
	for tries := 0; len(initial) == 0 && tries < 5; tries++ {
		stat, statErr := ReadStat("/proc", pid)
		if statErr != nil || stat.State == "Z" || stat.State == "X" {
			break
		}
		timer := time.NewTimer(5 * time.Millisecond)
		<-timer.C
		initial, err = discoverCommandTargets("/proc", self, pid)
		if err != nil {
			return StopResult{}, err
		}
	}
	if len(initial) == 0 {
		if err := reapCommandGroupZombies(pid); err != nil {
			return StopResult{}, err
		}
		return StopResult{AlreadyExited: true}, nil
	}
	signaledTargets := make(map[int]bool, len(initial))
	var failures stopFailures
	requestOwnedTargets("/proc", self, pid, initial, unix.Signal(sig), signaledTargets, &failures)
	var result StopResult
	if sig != syscall.SIGKILL {
		if grace > 0 {
			timer := time.NewTimer(grace)
			<-timer.C
		}
		survivors, scanErr := discoverCommandTargets("/proc", self, pid)
		if scanErr != nil {
			failures.add(0, scanErr)
		} else {
			result.Killed = requestOwnedTargets("/proc", self, pid, survivors, unix.SIGKILL, signaledTargets, &failures)
			result.Killed += drainCommandTargets("/proc", self, pid, signaledTargets, &failures)
		}
	} else if sig == syscall.SIGKILL {
		result.Killed = drainCommandTargets("/proc", self, pid, signaledTargets, &failures)
	}
	if err := reapCommandGroupZombies(pid); err != nil {
		failures.add(0, err)
	}
	return result, failures.err()
}

func validateChildGroupCommand(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) (int, error) {
	if grace < 0 || grace > 24*time.Hour {
		return 0, errors.New("grace must be between 0 and 24h")
	}
	if cmd == nil || cmd.Process == nil {
		return 0, errors.New("missing started command")
	}
	if !ownGroupConfig(cmd.SysProcAttr) {
		return 0, errors.New("command was not started as its own process-group or session leader")
	}
	pid := cmd.Process.Pid
	if err := validateSignalTarget(pid, sig); err != nil {
		return 0, err
	}
	return pid, nil
}

func ownGroupConfig(attr *syscall.SysProcAttr) bool {
	return attr != nil && (attr.Setpgid && attr.Pgid == 0 && !attr.Setsid || attr.Setsid && !attr.Setpgid)
}

func stopTargetRace(err error) bool {
	return errors.Is(err, errStopTargetChanged) || Gone(err) || errors.Is(err, unix.ESRCH)
}

var beforeStopChildGroupTree = func(int) {}
var beforeStopTargetPidfdOpen = func(int) {}
var afterStopTargetPidfdOpen = func(int) {}

var readSignalChildStat = ReadStat
var beforeSignalChildPidfdOpen = func(int) {}
var openSignalChildPidfd = unix.PidfdOpen

// SignalChild signals only a direct child after checking its parent and start
// time both before and after opening a pidfd. The pidfd pins identity at send.
func SignalChild(pid int, sig syscall.Signal) error {
	if err := validateSignalTarget(pid, sig); err != nil {
		return err
	}
	self := os.Getpid()
	target, err := readSignalChildStat("/proc", pid)
	if err != nil {
		if Gone(err) {
			return fmt.Errorf("read child process %d: %w", pid, os.ErrProcessDone)
		}
		return fmt.Errorf("read child process %d: %w", pid, err)
	}
	if target.Ppid != self {
		return fmt.Errorf("PID %d is not a direct child", pid)
	}
	if target.State == "Z" || target.State == "X" {
		return fmt.Errorf("child process %d: %w", pid, os.ErrProcessDone)
	}
	beforeSignalChildPidfdOpen(pid)
	fd, err := openSignalChildPidfd(pid, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("pidfd_open child %d: %w", pid, errors.Join(errStopTargetChanged, os.ErrProcessDone))
		}
		return fmt.Errorf("pidfd_open %d: %w", pid, err)
	}
	defer unix.Close(fd)
	checked, err := readSignalChildStat("/proc", pid)
	if Gone(err) {
		return fmt.Errorf("PID %d changed during validation: %w", pid, errors.Join(errStopTargetChanged, os.ErrProcessDone))
	}
	if err != nil || !sameDirectChild(self, target, checked) {
		return fmt.Errorf("PID %d changed during validation: %w", pid, errors.Join(errStopTargetChanged, os.ErrProcessDone))
	}
	if checked.State == "Z" || checked.State == "X" {
		return fmt.Errorf("child process %d: %w", pid, os.ErrProcessDone)
	}
	err = signalProcess(pid, &trackedProcess{fd: fd, start: target.Start}, unix.Signal(sig))
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("signal child PID %d: %w", pid, errors.Join(errStopTargetChanged, os.ErrProcessDone))
		}
		return fmt.Errorf("signal child PID %d: %w", pid, err)
	}
	return nil
}

func validateSignalTarget(pid int, sig syscall.Signal) error {
	if pid <= 1 {
		return fmt.Errorf("refusing PID %d", pid)
	}
	if sig <= 0 || sig > 64 {
		return fmt.Errorf("invalid signal %d", sig)
	}
	return nil
}

func sameDirectChild(self int, before, after Stat) bool {
	return after.Ppid == self && after.Start == before.Start
}

// StopFromShell sends sig to the target and the descendants observed before the
// request, then SIGKILL to tracked survivors after the grace. It permits
// descendants of the caller's parent and of the consecutive shells directly
// above it; a non-shell above those shells is not an owner. This ancestry rule
// is not a security boundary between jobs of the same shell. A failure for a
// single process does not stop the rest: the result comes with an error naming
// each failed PID.
func StopFromShell(pid int, grace time.Duration, sig unix.Signal) (StopResult, error) {
	if sig <= 0 || sig > 64 {
		return StopResult{}, fmt.Errorf("invalid signal %d", sig)
	}
	return stopTree("/proc", os.Getpid(), pid, grace, nil, sig)
}

func serviceManager(pid int, s Stat) bool {
	return pid <= 1 || strings.EqualFold(s.Comm, "systemd")
}

// isShell tells a shell by the program it runs, not by comm: a script started
// by its path with a #!/bin/sh line is named after the script, and comm can be
// renamed. An unreadable exe (another user's process) is not taken for a shell,
// so the owner chain ends there.
func isShell(root string, pid int) bool {
	exe, err := os.Readlink(root + "/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return false
	}
	exe = strings.TrimSuffix(exe, " (deleted)")
	switch exe[strings.LastIndexByte(exe, '/')+1:] {
	case "bash", "sh", "dash", "zsh", "ksh", "ksh93", "mksh":
		return true
	}
	return false
}

func stopOwners(root string, self int, shells bool) (map[int]bool, error) {
	s, err := ReadStat(root, self)
	if err != nil {
		return nil, err
	}
	owners := map[int]bool{}
	if !serviceManager(self, s) {
		owners[self] = true
	}
	if !shells {
		return owners, nil
	}
	// Only consecutive shells above the caller own targets. A non-shell (a
	// terminal, a multiplexer, an agent, any program) runs other jobs too and
	// is never an owner, even as the direct parent: a shell that runs stop as
	// its last command replaces itself with stop, and the parent seen then is
	// whatever started that shell. Programs stop their own children with
	// StopChild.
	for p := s.Ppid; p > 1; {
		s, err = ReadStat(root, p)
		if err != nil {
			return nil, err
		}
		if serviceManager(p, s) || !isShell(root, p) {
			break
		}
		owners[p] = true
		p = s.Ppid
	}
	return owners, nil
}

func validateStop(root string, self, pid int, shells bool) (Stat, error) {
	if pid <= 1 {
		return Stat{}, fmt.Errorf("refusing PID %d", pid)
	}
	ancestors := map[int]bool{}
	for p := self; p > 1; {
		if ancestors[p] {
			return Stat{}, errors.New("cyclic ancestry")
		}
		ancestors[p] = true
		s, err := ReadStat(root, p)
		if err != nil {
			return Stat{}, err
		}
		p = s.Ppid
	}
	if ancestors[pid] {
		return Stat{}, fmt.Errorf("PID %d is an ancestor", pid)
	}
	target, err := ReadStat(root, pid)
	if err != nil {
		return Stat{}, err
	}
	if serviceManager(pid, target) {
		return Stat{}, fmt.Errorf("refusing service manager PID %d", pid)
	}
	owners, err := stopOwners(root, self, shells)
	if err != nil {
		return Stat{}, err
	}
	seen := map[int]bool{}
	for p := target.Ppid; p > 1; {
		if owners[p] {
			return target, nil
		}
		if seen[p] {
			break
		}
		seen[p] = true
		s, err := ReadStat(root, p)
		if err != nil {
			return Stat{}, err
		}
		p = s.Ppid
	}
	return Stat{}, fmt.Errorf("PID %d is not a permitted descendant", pid)
}

type trackedProcess struct {
	fd    int
	start string
}

type processTree map[int]*trackedProcess

func (tree processTree) close() {
	for _, p := range tree {
		_ = unix.Close(p.fd)
	}
}

// stopFailures collects failures of single processes, so that one failure does
// not keep the request or SIGKILL from the rest of the tree.
type stopFailures struct {
	byPID   map[int][]string
	general []string
	causes  []error
}

func (f *stopFailures) add(pid int, err error) {
	msg := err.Error()
	if pid <= 0 {
		if !slices.Contains(f.general, msg) {
			f.general = append(f.general, msg)
			f.causes = append(f.causes, err)
		}
		return
	}
	if f.byPID == nil {
		f.byPID = map[int][]string{}
	}
	if !slices.Contains(f.byPID[pid], msg) {
		f.byPID[pid] = append(f.byPID[pid], msg)
		f.causes = append(f.causes, fmt.Errorf("PID %d: %w", pid, err))
	}
}

func (f *stopFailures) err() error {
	if len(f.byPID) == 0 && len(f.general) == 0 {
		return nil
	}
	return errors.Join(f.causes...)
}

// signalProcess sends sig through the process's pidfd. Tests replace it to
// make the signal fail for one process.
var signalProcess = func(_ int, p *trackedProcess, sig unix.Signal) error {
	return unix.PidfdSendSignal(p.fd, sig, nil, 0)
}

func openStopTarget(root string, self, pid int, sig unix.Signal) (processTree, bool, error) {
	target, err := validateStop(root, self, pid, sig != 0)
	if err != nil {
		if Gone(err) {
			return nil, false, fmt.Errorf("stat child process %d: %w", pid, errors.Join(errStopTargetChanged, err))
		}
		return nil, false, err
	}
	if sig == 0 && target.Ppid != self {
		return nil, false, fmt.Errorf("PID %d is not a direct child", pid)
	}
	if target.State == "Z" || target.State == "X" {
		return nil, true, nil
	}
	beforeStopTargetPidfdOpen(pid)
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return nil, false, fmt.Errorf("pidfd_open %d: %w", pid, errors.Join(errStopTargetChanged, err))
		}
		return nil, false, fmt.Errorf("pidfd_open %d: %w", pid, err)
	}
	afterStopTargetPidfdOpen(pid)
	checked, err := validateStop(root, self, pid, sig != 0)
	if err != nil || checked.Start != target.Start {
		_ = unix.Close(fd)
		return nil, false, fmt.Errorf("PID %d changed during validation: %w", pid, errStopTargetChanged)
	}
	return processTree{pid: {fd: fd, start: target.Start}}, false, nil
}

// stopTree validates and opens the target, captures its tree and sends the
// request to the captured processes only: processes started later, such as a
// cleanup the target runs on the request, are tracked and get only SIGKILL if
// they outlive the grace. Failures before the request abort; later failures of
// single processes are collected and returned with the result.
func stopTree(root string, self, pid int, grace time.Duration, request func() error, sig unix.Signal) (StopResult, error) {
	if grace < 0 || grace > 24*time.Hour {
		return StopResult{}, errors.New("grace must be between 0 and 24h")
	}
	tree, exited, err := openStopTarget(root, self, pid, sig)
	if err != nil {
		return StopResult{}, err
	}
	if exited {
		return StopResult{AlreadyExited: true}, nil
	}
	defer tree.close()
	var failures stopFailures
	if err := captureDescendants(root, tree, &failures); err != nil {
		return StopResult{}, err
	}
	if sig == 0 && request != nil {
		if err := request(); err != nil {
			return StopResult{}, fmt.Errorf("stop request: %w", err)
		}
	} else if sig != 0 {
		requestTree(tree, pid, sig, &failures)
	}
	var result StopResult
	if waitTree(root, tree, grace, &failures) {
		result = killTree(tree, &failures)
	}
	return result, failures.err()
}

func requestTree(tree processTree, leader int, sig unix.Signal, failures *stopFailures) {
	pids := []int{leader}
	for pid := range tree {
		if pid != leader {
			pids = append(pids, pid)
		}
	}
	for _, pid := range pids {
		err := signalProcess(pid, tree[pid], sig)
		if err != nil && !errors.Is(err, unix.ESRCH) {
			failures.add(pid, fmt.Errorf("request %s: %w", unix.SignalName(sig), err))
		}
	}
}

func processAlive(p *trackedProcess) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 0)
	// The Go runtime's own signals interrupt the call; it is safe to repeat.
	for errors.Is(err, unix.EINTR) {
		n, err = unix.Poll(fds, 0)
	}
	if err != nil {
		return false, fmt.Errorf("poll: %w", err)
	}
	return n == 0, nil
}

// treeAlive treats a process whose state cannot be polled as alive, so that it
// still gets SIGKILL after the grace.
func treeAlive(tree processTree, failures *stopFailures) bool {
	alive := false
	for pid, p := range tree {
		running, err := processAlive(p)
		if err != nil {
			failures.add(pid, err)
			running = true
		}
		alive = alive || running
	}
	return alive
}

func waitTree(root string, tree processTree, grace time.Duration, failures *stopFailures) bool {
	deadline := time.Now().Add(grace)
	for {
		if err := captureDescendants(root, tree, failures); err != nil {
			failures.add(0, err)
		}
		if !treeAlive(tree, failures) {
			return false
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return true
		}
		if remaining > 20*time.Millisecond {
			remaining = 20 * time.Millisecond
		}
		time.Sleep(remaining)
	}
}

func killTree(tree processTree, failures *stopFailures) StopResult {
	killed := processTree{}
	for pid, p := range tree {
		alive, err := processAlive(p)
		if err != nil {
			failures.add(pid, err)
		} else if !alive {
			continue
		}
		err = signalProcess(pid, p, unix.SIGKILL)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			failures.add(pid, fmt.Errorf("SIGKILL: %w", err))
			continue
		}
		killed[pid] = p
	}
	result := StopResult{Killed: len(killed)}
	deadline := time.Now().Add(5 * time.Second)
	for len(killed) > 0 {
		for pid, p := range killed {
			alive, err := processAlive(p)
			if err != nil {
				failures.add(pid, fmt.Errorf("after SIGKILL: %w", err))
			}
			if err != nil || !alive {
				delete(killed, pid)
			}
		}
		if len(killed) == 0 {
			break
		}
		if time.Now().After(deadline) {
			for pid := range killed {
				failures.add(pid, errors.New("SIGKILL: did not exit within 5s"))
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return result
}

// captureDescendants adds the not yet tracked descendants of tracked
// processes. Only an unreadable process list is returned as an error; a failure
// for a single process is collected and the scan goes on.
func captureDescendants(root string, tree processTree, failures *stopFailures) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read process list %s: %w", root, err)
	}
	stats := map[int]Stat{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		s, err := ReadStat(root, pid)
		if err == nil {
			stats[pid] = s
		}
	}
	for changed := true; changed; {
		changed = false
		for pid, s := range stats {
			added, err := captureProcess(root, tree, stats, pid, s)
			if err != nil {
				failures.add(pid, err)
			}
			changed = added || changed
		}
	}
	return nil
}

// captureProcess tracks pid if it is a child of a tracked process. An error is
// a failure for this process alone; the caller collects it and goes on.
func captureProcess(root string, tree processTree, stats map[int]Stat, pid int, s Stat) (bool, error) {
	if _, ok := tree[pid]; ok || s.State == "Z" || s.State == "X" || serviceManager(pid, s) {
		return false, nil
	}
	parent, ok := tree[s.Ppid]
	if !ok {
		return false, nil
	}
	ps, ok := stats[s.Ppid]
	if !ok || ps.Start != parent.start {
		return false, nil
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pidfd_open: %w", err)
	}
	current, err := ReadStat(root, pid)
	if err != nil {
		_ = unix.Close(fd)
		if Gone(err) {
			return false, nil
		}
		return false, err
	}
	if current.Start != s.Start || current.Ppid != s.Ppid {
		_ = unix.Close(fd)
		return false, nil
	}
	// The parent's stat was read at another moment of the scan: its PID may
	// have been reused since. While the tracked parent has not exited, its PID
	// is still its own, so the child's parent PID read above names it.
	alive, err := processAlive(parent)
	if err != nil {
		_ = unix.Close(fd)
		return false, fmt.Errorf("parent %d: %w", s.Ppid, err)
	}
	if !alive {
		_ = unix.Close(fd)
		return false, nil
	}
	tree[pid] = &trackedProcess{fd: fd, start: s.Start}
	return true, nil
}
