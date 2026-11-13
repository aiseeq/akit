//go:build linux

package proc

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type trackedOwnedProcess struct {
	identity  processIdentity
	tracked   *trackedProcess
	requested bool
	killed    bool
}

var afterDescendantPidfdOpen = func(int) {}
var readSessionStat = ReadStat
var openSessionPidfd = func(pid, flags int) (int, error) { return openProcessPidfd(pid, flags) }
var afterSessionPidfdOpen = func(int) {}
var pollSessionLeader = unix.Poll

func sessionLeaderAlive(fd int) (bool, error) {
	for {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, err := pollSessionLeader(fds, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, err
		}
		return fds[0].Revents == 0, nil
	}
}

// StopDescendants signals every verified descendant of this process, then
// SIGKILLs survivors after grace. It never reaps direct children; their Cmd
// owners retain responsibility for Wait. Callers must be child subreapers to
// find descendants reparented after an intermediate process exits.
// If pidfd_open is unavailable (ErrPidfdUnavailable: ENOSYS or EPERM) in the
// first batch, no signals are sent. If it becomes unavailable later, the scan
// stops and ErrPidfdUnavailable is returned with StopResult.Signaled counting
// processes already sent the requested signal. Any other pidfd_open error
// skips that process in the current batch; a later batch may open and signal
// it. Such errors are returned with the result, except a process that exited
// or changed identity, which is not an error.
func StopDescendants(grace time.Duration, sig syscall.Signal) (StopResult, error) {
	if err := validateOwnedStopArgs(grace, sig); err != nil {
		return StopResult{}, err
	}
	self := os.Getpid()
	if err := ensurePidfdAvailable(self); err != nil {
		return StopResult{}, err
	}
	ops := ownedStopOps{
		scan:   func() ([]int, error) { return descendantPIDs("/proc", self) },
		open:   func(pid int) (*trackedOwnedProcess, error) { return ownedDescendantTarget("/proc", self, pid) },
		verify: func(pid int, start string) error { return revalidateDescendant("/proc", self, pid, start) },
	}
	return stopOwnedProcesses(grace, sig, ops)
}

// StopSession signals verified members of rec's detached session and then
// SIGKILLs survivors after grace. It requires the original live session leader
// and holds its pidfd throughout the stop. For each batch, the leader is checked
// through that pidfd before scanning and after every member pidfd is opened and
// checked. If the leader is live at both checks, selected members were verified
// as members of the recorded session while its original leader was live. A
// member may leave the session, or the leader may exit, after the second check;
// pidfds still pin the exact processes verified in the session, and the
// requested signal to those processes remains authorized. When the requested
// signal is not SIGKILL, each pinned process is checked again before the final
// SIGKILL after grace: one that has left the session by then is not killed and
// is not reported as an error. If the leader exits before any requested
// signal is sent, the batch is not signaled and ErrSessionLeaderGone is returned.
// After at least one requested signal is sent, leader exit ends discovery of
// new members; the stop waits out grace (or until all pinned members exit) and
// SIGKILLs only members already pinned before the leader exit. Other guard
// failures stop before the current batch is signaled and are returned. Orphans
// of an exited leader cannot be identified safely; use StopDescendants as a
// subreaper or a dedicated systemd scope. If pidfd_open is unavailable
// (ErrPidfdUnavailable: ENOSYS or EPERM)
// in the first batch, no signals are sent. If it becomes unavailable later,
// the scan stops and ErrPidfdUnavailable is returned with StopResult.Signaled
// counting processes already sent the requested signal. Any other pidfd_open
// error skips that process in the current batch; a later batch may open and
// signal it. Such errors are returned with the result, except a process that
// exited or changed identity, which is not an error.
func StopSession(rec SessionRecord, grace time.Duration, sig syscall.Signal) (StopResult, error) {
	if err := validateOwnedStopArgs(grace, sig); err != nil {
		return StopResult{}, err
	}
	start, err := parseRecordedStart(rec.Start)
	if err != nil {
		return StopResult{}, err
	}
	if err := validateSessionPID(rec.Pid); err != nil {
		return StopResult{}, err
	}
	self := os.Getpid()
	if err := ensurePidfdAvailable(self); err != nil {
		return StopResult{}, err
	}
	leaderFD, err := openVerifiedSessionLeader("/proc", self, rec, start)
	if err != nil {
		return StopResult{}, err
	}
	defer unix.Close(leaderFD)
	ops := ownedStopOps{
		scan: func() ([]int, error) { return sessionPIDs("/proc", self, rec.Pid, start) },
		open: func(pid int) (*trackedOwnedProcess, error) { return ownedSessionTarget("/proc", rec.Pid, start, pid) },
		verify: func(pid int, startText string) error {
			return revalidateSessionMember("/proc", rec.Pid, start, pid, startText)
		},
		guard: func() error {
			alive, checkErr := sessionLeaderAlive(leaderFD)
			if checkErr != nil {
				return fmt.Errorf("check session leader PID %d: %w", rec.Pid, checkErr)
			}
			if !alive {
				return fmt.Errorf("%w: PID %d exited during stop", ErrSessionLeaderGone, rec.Pid)
			}
			return nil
		},
	}
	return stopOwnedProcesses(grace, sig, ops)
}

// SignalDescendant signals one verified descendant of the caller. Orphaned
// descendants can be verified only when the caller is a child subreaper.
// pidfd_open pins the process identity; if it is unavailable, no signal is sent.
func SignalDescendant(pid int, sig syscall.Signal) error {
	if err := validateSignalTarget(pid, sig); err != nil {
		return err
	}
	self := os.Getpid()
	if pid == self {
		return fmt.Errorf("refusing to signal the caller PID %d", pid)
	}
	target, err := ownedDescendantTarget("/proc", self, pid)
	if err != nil {
		return err
	}
	defer unix.Close(target.tracked.fd)
	if err := revalidateDescendant("/proc", self, pid, target.identity.start); err != nil {
		return err
	}
	if err := signalProcess(pid, target.tracked, unix.Signal(sig)); err != nil {
		return err
	}
	return nil
}

type ownedStopOps struct {
	scan   func() ([]int, error)
	open   func(int) (*trackedOwnedProcess, error)
	verify func(int, string) error
	guard  func() error
}

func stopOwnedProcesses(grace time.Duration, sig syscall.Signal, ops ownedStopOps) (StopResult, error) {
	tracked := make(map[processIdentity]*trackedOwnedProcess)
	defer closeOwnedProcesses(tracked)
	var failures stopFailures
	result := StopResult{}
	deadline := time.Now().Add(grace)
	pidfdUnavailable := false
	guardFailed := false
	leaderGone := false
	for {
		if err := checkOwnedStopGuard(ops); err != nil {
			if errors.Is(err, ErrSessionLeaderGone) && result.Signaled > 0 {
				leaderGone = true
			} else {
				failures.add(0, err)
				guardFailed = true
			}
			break
		}
		pids, err := ops.scan()
		if err != nil {
			failures.add(0, err)
			pidfdUnavailable = errors.Is(err, ErrPidfdUnavailable)
			break
		}
		previous := snapshotOwnedProcesses(tracked)
		batch, err := prepareOwnedCandidates(pids, ops, tracked, &failures, false)
		if err != nil {
			failures.add(0, err)
			pidfdUnavailable = errors.Is(err, ErrPidfdUnavailable)
			break
		}
		if err := checkOwnedStopGuard(ops); err != nil {
			discardNewOwnedProcesses(tracked, previous)
			if errors.Is(err, ErrSessionLeaderGone) && result.Signaled > 0 {
				leaderGone = true
			} else {
				failures.add(0, err)
				guardFailed = true
			}
			break
		}
		requestOwnedCandidates(batch, sig, &failures, &result)
		if sig == syscall.SIGKILL || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(min(5*time.Millisecond, time.Until(deadline)))
	}
	if sig != syscall.SIGKILL && !pidfdUnavailable && !guardFailed && !leaderGone {
		if err := checkOwnedStopGuard(ops); err != nil {
			if errors.Is(err, ErrSessionLeaderGone) && result.Signaled > 0 {
				leaderGone = true
			} else {
				failures.add(0, err)
				guardFailed = true
			}
		}
	}
	if sig != syscall.SIGKILL && !pidfdUnavailable && !guardFailed {
		if leaderGone {
			waitOwnedStopGrace(deadline, tracked, &failures)
			finishPinnedOwnedStop(sig, ops, tracked, &failures, &result)
		} else {
			finishOwnedStop(sig, ops, tracked, &failures, &result)
		}
	}
	result.AlreadyExited = len(tracked) == 0
	return result, failures.err()
}

func checkOwnedStopGuard(ops ownedStopOps) error {
	if ops.guard == nil {
		return nil
	}
	return ops.guard()
}

func finishOwnedStop(sig syscall.Signal, ops ownedStopOps, tracked map[processIdentity]*trackedOwnedProcess, failures *stopFailures, result *StopResult) {
	pids, err := ops.scan()
	if err != nil {
		failures.add(0, err)
		return
	}
	previous := snapshotOwnedProcesses(tracked)
	batch, err := prepareOwnedCandidates(pids, ops, tracked, failures, true)
	if err != nil {
		failures.add(0, err)
		return
	}
	if err := checkOwnedStopGuard(ops); err != nil {
		discardNewOwnedProcesses(tracked, previous)
		if errors.Is(err, ErrSessionLeaderGone) && result.Signaled > 0 {
			waitOwnedStopGrace(time.Now(), tracked, failures)
			finishPinnedOwnedStop(sig, ops, tracked, failures, result)
			return
		}
		failures.add(0, err)
		return
	}
	result.Killed += killOwnedCandidates(batch, sig, ops, failures, result)
}

func snapshotOwnedProcesses(tracked map[processIdentity]*trackedOwnedProcess) map[processIdentity]struct{} {
	previous := make(map[processIdentity]struct{}, len(tracked))
	for identity := range tracked {
		previous[identity] = struct{}{}
	}
	return previous
}

func discardNewOwnedProcesses(tracked map[processIdentity]*trackedOwnedProcess, previous map[processIdentity]struct{}) {
	for identity, entry := range tracked {
		if _, existed := previous[identity]; existed {
			continue
		}
		_ = unix.Close(entry.tracked.fd)
		delete(tracked, identity)
	}
}

func finishPinnedOwnedStop(sig syscall.Signal, ops ownedStopOps, tracked map[processIdentity]*trackedOwnedProcess, failures *stopFailures, result *StopResult) {
	batch := make([]*trackedOwnedProcess, 0, len(tracked))
	for _, entry := range tracked {
		batch = append(batch, entry)
	}
	result.Killed += killOwnedCandidates(batch, sig, ops, failures, result)
}

func waitOwnedStopGrace(deadline time.Time, tracked map[processIdentity]*trackedOwnedProcess, failures *stopFailures) {
	for time.Now().Before(deadline) {
		anyAlive := false
		for _, entry := range tracked {
			alive, err := processAlive(entry.tracked)
			if err != nil {
				recordOwnedFailure(failures, entry.identity.pid, err)
				anyAlive = true
				continue
			}
			anyAlive = anyAlive || alive
		}
		if !anyAlive {
			return
		}
		time.Sleep(min(5*time.Millisecond, time.Until(deadline)))
	}
}

func prepareOwnedCandidates(pids []int, ops ownedStopOps, tracked map[processIdentity]*trackedOwnedProcess, failures *stopFailures, includeRequested bool) ([]*trackedOwnedProcess, error) {
	var batch []*trackedOwnedProcess
	for _, pid := range pids {
		candidate, err := ops.open(pid)
		if err != nil {
			if errors.Is(err, ErrPidfdUnavailable) {
				return nil, err
			}
			recordOwnedFailure(failures, pid, err)
			continue
		}
		entry := rememberOwnedProcess(tracked, candidate)
		if includeRequested || !entry.requested {
			batch = append(batch, entry)
		}
	}
	return batch, nil
}

func requestOwnedCandidates(batch []*trackedOwnedProcess, sig syscall.Signal, failures *stopFailures, result *StopResult) {
	for _, entry := range batch {
		pid := entry.identity.pid
		if err := signalProcess(pid, entry.tracked, unix.Signal(sig)); err != nil {
			recordOwnedFailure(failures, pid, err)
			continue
		}
		if !entry.requested {
			result.Signaled++
		}
		entry.requested = true
		if sig == syscall.SIGKILL && !entry.killed {
			entry.killed = true
			result.Killed++
		}
	}
}

func killOwnedCandidates(batch []*trackedOwnedProcess, sig syscall.Signal, ops ownedStopOps, failures *stopFailures, result *StopResult) int {
	killed := 0
	for _, entry := range batch {
		pid := entry.identity.pid
		if !entry.requested {
			if requestOwnedProcess(pid, sig, entry, failures) != nil {
				continue
			}
			result.Signaled++
		}
		if err := ops.verify(pid, entry.identity.start); err != nil {
			recordOwnedFailure(failures, pid, err)
			continue
		}
		if err := signalProcess(pid, entry.tracked, unix.SIGKILL); err != nil {
			recordOwnedFailure(failures, pid, err)
			continue
		}
		if !entry.killed {
			entry.killed = true
			killed++
		}
	}
	return killed
}

func requestOwnedProcess(pid int, sig syscall.Signal, entry *trackedOwnedProcess, failures *stopFailures) error {
	if err := signalProcess(pid, entry.tracked, unix.Signal(sig)); err != nil {
		recordOwnedFailure(failures, pid, err)
		return err
	}
	entry.requested = true
	return nil
}

func recordOwnedFailure(failures *stopFailures, pid int, err error) {
	if reportOwnedRace(err) {
		failures.add(pid, err)
	}
}

func validateSessionTarget(root string, self int, rec SessionRecord, recordedStart uint64) error {
	if err := validateSessionDoesNotContainCaller(root, self, rec.Pid); err != nil {
		return err
	}
	leader, err := readSessionStat(root, rec.Pid)
	if err != nil {
		if Gone(err) {
			return fmt.Errorf("%w: PID %d", ErrSessionLeaderGone, rec.Pid)
		}
		return fmt.Errorf("read session leader %d: %w", rec.Pid, err)
	}
	if serviceManager(rec.Pid, leader) {
		return fmt.Errorf("refusing system manager session leader PID %d", rec.Pid)
	}
	if leader.State == "Z" || leader.State == "X" {
		return fmt.Errorf("%w: PID %d is not live", ErrSessionLeaderGone, rec.Pid)
	}
	leaderStart, err := numericStart(leader.Start)
	if err != nil || leaderStart != recordedStart {
		return fmt.Errorf("%w: PID %d start does not match record", ErrSessionLeaderGone, rec.Pid)
	}
	if leader.Sid != rec.Pid {
		return fmt.Errorf("%w: PID %d is not its session leader", ErrSessionLeaderGone, rec.Pid)
	}
	return nil
}

func openVerifiedSessionLeader(root string, self int, rec SessionRecord, recordedStart uint64) (int, error) {
	if err := validateSessionDoesNotContainCaller(root, self, rec.Pid); err != nil {
		return -1, err
	}
	if err := validateSessionTarget(root, self, rec, recordedStart); err != nil {
		return -1, err
	}
	fd, err := openSessionPidfd(rec.Pid, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return -1, fmt.Errorf("%w: PID %d", ErrSessionLeaderGone, rec.Pid)
		}
		return -1, fmt.Errorf("pidfd_open session leader PID %d: %w", rec.Pid, pidfdOpenError(err))
	}
	afterSessionPidfdOpen(rec.Pid)
	if err := validateSessionTarget(root, self, rec, recordedStart); err != nil {
		_ = unix.Close(fd)
		return -1, errors.Join(errStopTargetChanged, err)
	}
	return fd, nil
}

func validateSessionPID(pid int) error {
	if pid <= 1 {
		return fmt.Errorf("refusing session leader PID %d", pid)
	}
	return nil
}

func validateOwnedStopArgs(grace time.Duration, sig syscall.Signal) error {
	if grace < 0 || grace > 24*time.Hour {
		return errors.New("grace must be between 0 and 24h")
	}
	if sig <= 0 || sig > 64 {
		return fmt.Errorf("invalid signal %d", sig)
	}
	return nil
}

func parseRecordedStart(start string) (uint64, error) {
	if start == "" || start == "0" {
		return 0, ErrNoStartRecord
	}
	value, err := strconv.ParseUint(start, 10, 64)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("invalid session start %q", start)
	}
	return value, nil
}

func numericStart(start string) (uint64, error) {
	return strconv.ParseUint(start, 10, 64)
}

func validateSessionDoesNotContainCaller(root string, self, sessionID int) error {
	seen := map[int]bool{}
	for pid := self; pid > 0; {
		if seen[pid] {
			return errors.New("cyclic process ancestry while checking session")
		}
		seen[pid] = true
		stat, err := readSessionStat(root, pid)
		if err != nil {
			return fmt.Errorf("read PID %d while checking caller session: %w", pid, err)
		}
		if stat.Sid == sessionID {
			return fmt.Errorf("refusing caller or ancestor session %d", sessionID)
		}
		if pid == 1 {
			break
		}
		pid = stat.Ppid
	}
	return nil
}

func descendantPIDs(root string, self int) ([]int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read process list %s: %w", root, err)
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || pid == self {
			continue
		}
		stat, err := ReadStat(root, pid)
		if err != nil {
			if Gone(err) || errors.Is(err, os.ErrPermission) {
				continue
			}
			return nil, fmt.Errorf("read process stat PID %d: %w", pid, err)
		}
		if stat.State == "Z" || stat.State == "X" || serviceManager(pid, stat) {
			continue
		}
		if _, err := processAncestry(root, pid, self); err == nil {
			pids = append(pids, pid)
		} else if !errors.Is(err, errNotDescendant) && !Gone(err) && !errors.Is(err, os.ErrPermission) && !errors.Is(err, os.ErrProcessDone) {
			return nil, fmt.Errorf("verify PID %d ancestry: %w", pid, err)
		}
	}
	return pids, nil
}

func ownedDescendantTarget(root string, self, pid int) (*trackedOwnedProcess, error) {
	before, err := processAncestry(root, pid, self)
	if err != nil {
		return nil, err
	}
	stat, err := readCommandTargetStat(root, pid)
	if err != nil {
		return nil, err
	}
	if serviceManager(pid, stat) {
		return nil, fmt.Errorf("refusing process manager PID %d", pid)
	}
	fd, err := openProcessPidfd(pid, 0)
	if err != nil {
		return nil, fmt.Errorf("pidfd_open descendant PID %d: %w", pid, pidfdOpenError(err))
	}
	afterDescendantPidfdOpen(pid)
	after, err := processAncestry(root, pid, self)
	checked, statErr := readCommandTargetStat(root, pid)
	if err != nil || !sameProcessChain(before, after) || statErr != nil || checked.Start != stat.Start {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("descendant PID %d changed during validation: %w", pid, errors.Join(errStopTargetChanged, err, statErr))
	}
	return &trackedOwnedProcess{identity: processIdentity{pid: pid, start: stat.Start}, tracked: &trackedProcess{fd: fd, start: stat.Start}}, nil
}

func sameProcessChain(a, b []processIdentity) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func revalidateDescendant(root string, self, pid int, start string) error {
	chain, err := processAncestry(root, pid, self)
	if err != nil || len(chain) == 0 || chain[0].start != start {
		return errors.Join(errStopTargetChanged, err)
	}
	stat, err := readCommandTargetStat(root, pid)
	if err != nil || stat.Start != start || serviceManager(pid, stat) {
		return errors.Join(errStopTargetChanged, err)
	}
	return nil
}

func sessionPIDs(root string, self, sessionID int, minStart uint64) ([]int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read process list %s: %w", root, err)
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || pid == self {
			continue
		}
		stat, err := readSessionStat(root, pid)
		if err != nil {
			if Gone(err) || errors.Is(err, os.ErrPermission) {
				continue
			}
			return nil, fmt.Errorf("read process stat PID %d: %w", pid, err)
		}
		if stat.State == "Z" || stat.State == "X" || serviceManager(pid, stat) || stat.Sid != sessionID {
			continue
		}
		start, err := numericStart(stat.Start)
		if err == nil && start >= minStart {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func ownedSessionTarget(root string, sessionID int, minStart uint64, pid int) (*trackedOwnedProcess, error) {
	if pid <= 1 {
		return nil, fmt.Errorf("refusing session member PID %d", pid)
	}
	stat, err := readSessionStat(root, pid)
	if err != nil {
		return nil, err
	}
	start, err := numericStart(stat.Start)
	if err != nil || stat.Sid != sessionID || start < minStart || stat.State == "Z" || stat.State == "X" || serviceManager(pid, stat) {
		return nil, fmt.Errorf("PID %d is not a member of recorded session", pid)
	}
	fd, err := openSessionPidfd(pid, 0)
	if err != nil {
		return nil, fmt.Errorf("pidfd_open session PID %d: %w", pid, pidfdOpenError(err))
	}
	afterSessionPidfdOpen(pid)
	checked, err := readSessionStat(root, pid)
	checkedStart, startErr := numericStart(checked.Start)
	if err != nil || startErr != nil || checked.Sid != sessionID || checkedStart != start || checked.State == "Z" || checked.State == "X" {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("session PID %d changed during validation: %w", pid, errors.Join(errStopTargetChanged, err, startErr))
	}
	return &trackedOwnedProcess{identity: processIdentity{pid: pid, start: checked.Start}, tracked: &trackedProcess{fd: fd, start: checked.Start}}, nil
}

func revalidateSessionMember(root string, sessionID int, minStart uint64, pid int, startText string) error {
	stat, err := readSessionStat(root, pid)
	if err != nil {
		return errors.Join(errStopTargetChanged, err)
	}
	start, err := numericStart(stat.Start)
	if err != nil || stat.Sid != sessionID || startText != stat.Start || start < minStart || stat.State == "Z" || stat.State == "X" || serviceManager(pid, stat) {
		return errors.Join(errStopTargetChanged, err)
	}
	return nil
}

func closeOwnedProcesses(processes map[processIdentity]*trackedOwnedProcess) {
	for _, process := range processes {
		_ = unix.Close(process.tracked.fd)
	}
}

func rememberOwnedProcess(tracked map[processIdentity]*trackedOwnedProcess, candidate *trackedOwnedProcess) *trackedOwnedProcess {
	if current := tracked[candidate.identity]; current != nil {
		_ = unix.Close(candidate.tracked.fd)
		return current
	}
	tracked[candidate.identity] = candidate
	return candidate
}

func reportOwnedRace(err error) bool {
	return !Gone(err) && !errors.Is(err, unix.ESRCH) && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, errStopTargetChanged)
}
