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

// StopDescendants signals every verified descendant of this process, then
// SIGKILLs survivors after grace. It never reaps direct children; their Cmd
// owners retain responsibility for Wait. Callers must be child subreapers to
// find descendants reparented after an intermediate process exits.
// If pidfd_open is unavailable, it returns ErrPidfdUnavailable without signaling.
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
// SIGKILLs survivors after grace. The session ID cannot be reused by a new
// session leader while any process from the recorded session remains.
// If pidfd_open is unavailable, it returns ErrPidfdUnavailable without signaling.
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
	if err := validateSessionTarget("/proc", self, rec, start); err != nil {
		return StopResult{}, err
	}
	ops := ownedStopOps{
		scan: func() ([]int, error) { return sessionPIDs("/proc", self, rec.Pid, start) },
		open: func(pid int) (*trackedOwnedProcess, error) { return ownedSessionTarget("/proc", rec.Pid, start, pid) },
		verify: func(pid int, startText string) error {
			return revalidateSessionMember("/proc", rec.Pid, start, pid, startText)
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
}

func stopOwnedProcesses(grace time.Duration, sig syscall.Signal, ops ownedStopOps) (StopResult, error) {
	tracked := make(map[processIdentity]*trackedOwnedProcess)
	defer closeOwnedProcesses(tracked)
	var failures stopFailures
	result := StopResult{}
	deadline := time.Now().Add(grace)
	for {
		pids, err := ops.scan()
		if err != nil {
			failures.add(0, err)
			break
		}
		requestOwnedCandidates(pids, sig, ops, tracked, &failures, &result)
		if sig == syscall.SIGKILL || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sig != syscall.SIGKILL {
		pids, err := ops.scan()
		if err != nil {
			failures.add(0, err)
		} else {
			result.Killed += killOwnedCandidates(pids, sig, ops, tracked, &failures)
		}
	}
	result.AlreadyExited = len(tracked) == 0
	return result, failures.err()
}

func requestOwnedCandidates(pids []int, sig syscall.Signal, ops ownedStopOps, tracked map[processIdentity]*trackedOwnedProcess, failures *stopFailures, result *StopResult) {
	for _, pid := range pids {
		candidate, err := ops.open(pid)
		if err != nil {
			recordOwnedFailure(failures, pid, err)
			continue
		}
		entry := rememberOwnedProcess(tracked, candidate)
		if entry.requested {
			continue
		}
		if err := signalProcess(pid, entry.tracked, unix.Signal(sig)); err != nil {
			recordOwnedFailure(failures, pid, err)
			continue
		}
		entry.requested = true
		if sig == syscall.SIGKILL && !entry.killed {
			entry.killed = true
			result.Killed++
		}
	}
}

func killOwnedCandidates(pids []int, sig syscall.Signal, ops ownedStopOps, tracked map[processIdentity]*trackedOwnedProcess, failures *stopFailures) int {
	killed := 0
	for _, pid := range pids {
		candidate, err := ops.open(pid)
		if err != nil {
			recordOwnedFailure(failures, pid, err)
			continue
		}
		entry := rememberOwnedProcess(tracked, candidate)
		if !entry.requested && requestOwnedProcess(pid, sig, entry, failures) != nil {
			continue
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
			return nil
		}
		return fmt.Errorf("read session leader %d: %w", rec.Pid, err)
	}
	if serviceManager(rec.Pid, leader) {
		return fmt.Errorf("refusing system manager session leader PID %d", rec.Pid)
	}
	if leader.State == "Z" || leader.State == "X" {
		return nil
	}
	leaderStart, err := numericStart(leader.Start)
	if err != nil || leaderStart != recordedStart {
		return fmt.Errorf("session leader PID %d start does not match record", rec.Pid)
	}
	if leader.Sid != rec.Pid {
		return fmt.Errorf("PID %d is not its session leader", rec.Pid)
	}
	return nil
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
