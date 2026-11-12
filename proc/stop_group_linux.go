//go:build linux

package proc

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// BecomeChildSubreaper makes this process adopt orphaned descendants. It is
// process-wide and must be called before starting commands whose detached
// descendants need cleanup.
func BecomeChildSubreaper() error {
	return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}

var afterCommandTargetPidfdOpen = func(int) {}
var readCommandTargetStat = ReadStat
var readCommandAncestryStat = ReadStat
var errNotDescendant = errors.New("process is not our descendant")

type processIdentity struct {
	pid   int
	start string
}

func discoverCommandTargets(root string, self, leader int) ([]int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read process list %s: %w", root, err)
	}
	stats := make(map[int]Stat, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		stat, statErr := ReadStat(root, pid)
		if statErr != nil {
			if Gone(statErr) || errors.Is(statErr, os.ErrPermission) {
				continue
			}
			return nil, fmt.Errorf("read process stat for PID %d: %w", pid, statErr)
		}
		stats[pid] = stat
	}
	var targets []int
	for pid, stat := range stats {
		if pid == self || stat.State == "Z" || stat.State == "X" || serviceManager(pid, stat) {
			continue
		}
		chain, descendant := snapshotAncestry(stats, pid, self)
		if !descendant {
			continue
		}
		inTree := pid == leader
		for _, item := range chain[1:] {
			if item.pid == leader {
				inTree = true
				break
			}
		}
		if inTree || stat.Pgid == leader {
			targets = append(targets, pid)
		}
	}
	slices.Sort(targets)
	return targets, nil
}

func snapshotAncestry(stats map[int]Stat, target, self int) ([]processIdentity, bool) {
	seen := make(map[int]bool)
	chain := make([]processIdentity, 0, 8)
	for pid := target; pid > 1; {
		if seen[pid] {
			return nil, false
		}
		seen[pid] = true
		stat, ok := stats[pid]
		if !ok || stat.State == "Z" || stat.State == "X" {
			return nil, false
		}
		chain = append(chain, processIdentity{pid: pid, start: stat.Start})
		if pid == self {
			return chain, true
		}
		pid = stat.Ppid
	}
	return nil, false
}

// processAncestry returns target-to-self identities, with self included. Each
// link is read from /proc; callers compare a second walk after pidfd_open.
func processAncestry(root string, target, self int) ([]processIdentity, error) {
	seen := make(map[int]bool)
	chain := make([]processIdentity, 0, 8)
	for pid := target; pid > 1; {
		if seen[pid] {
			return nil, fmt.Errorf("%w: cyclic ancestry", errNotDescendant)
		}
		seen[pid] = true
		stat, err := readCommandAncestryStat(root, pid)
		if err != nil || stat.State == "Z" || stat.State == "X" {
			if err == nil {
				err = os.ErrProcessDone
			}
			return nil, err
		}
		chain = append(chain, processIdentity{pid: pid, start: stat.Start})
		if pid == self {
			return chain, nil
		}
		pid = stat.Ppid
	}
	return nil, fmt.Errorf("%w: PID %d", errNotDescendant, target)
}

func verifiedCommandTarget(root string, self, leader, pid int) (*trackedProcess, error) {
	before, err := processAncestry(root, pid, self)
	if err != nil {
		return nil, err
	}
	stat, err := readCommandTargetStat(root, pid)
	if err != nil {
		return nil, err
	}
	if !commandMembership(before, pid, leader, stat.Pgid) {
		return nil, fmt.Errorf("PID %d left command tree and group", pid)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, fmt.Errorf("pidfd_open target PID %d: %w", pid, err)
	}
	afterCommandTargetPidfdOpen(pid)
	after, err := processAncestry(root, pid, self)
	if err != nil || len(after) == 0 || after[0].pid != before[0].pid || after[0].start != before[0].start {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("PID %d changed during ownership validation: %w", pid, errors.Join(errStopTargetChanged, err))
	}
	checked, err := readCommandTargetStat(root, pid)
	if err != nil || checked.Start != stat.Start || !commandMembership(after, pid, leader, checked.Pgid) {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("PID %d changed during target validation: %w", pid, errors.Join(errStopTargetChanged, err))
	}
	return &trackedProcess{fd: fd, start: stat.Start}, nil
}

func commandMembership(chain []processIdentity, pid, leader, pgid int) bool {
	if pgid == leader {
		return true
	}
	if pid == leader {
		return true
	}
	for _, item := range chain[1:] {
		if item.pid == leader {
			return true
		}
	}
	return false
}

func drainCommandTargets(root string, self, leader int, signaledTargets map[int]signalMark, failures *stopFailures) int {
	killed, emptyScans := 0, 0
	deadline := time.Now().Add(500 * time.Millisecond)
	for signaledTargets[leader].sig != 0 && time.Now().Before(deadline) {
		stat, err := ReadStat(root, leader)
		if Gone(err) || err == nil && (stat.State == "Z" || stat.State == "X") {
			break
		}
		if err != nil {
			failures.add(leader, err)
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	for time.Now().Before(deadline) && emptyScans < 3 {
		targets, err := discoverCommandTargets(root, self, leader)
		if err != nil {
			failures.add(0, err)
			return killed
		}
		if len(targets) == 0 {
			emptyScans++
			time.Sleep(5 * time.Millisecond)
			continue
		}
		emptyScans = 0
		priorSignals := len(signaledTargets)
		killed += requestOwnedTargets(root, self, leader, targets, unix.SIGKILL, signaledTargets, failures)
		if len(signaledTargets) == priorSignals {
			allSignaled := true
			for _, pid := range targets {
				allSignaled = allSignaled && signaledTargets[pid].sig != 0
			}
			if !allSignaled {
				return killed
			}
		}
	}
	return killed
}

func requestOwnedTargets(root string, self, leader int, targets []int, sig unix.Signal, signaledTargets map[int]signalMark, failures *stopFailures) int {
	sent := 0
	for _, pid := range targets {
		tracked, err := verifiedCommandTarget(root, self, leader, pid)
		if err != nil {
			if !Gone(err) && !errors.Is(err, unix.ESRCH) && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, errStopTargetChanged) {
				failures.add(pid, err)
			}
			continue
		}
		err = signalProcess(pid, tracked, sig)
		_ = unix.Close(tracked.fd)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			failures.add(pid, fmt.Errorf("signal %s: %w", unix.SignalName(sig), err))
			continue
		}
		// Killed counts each process once; a reused PID is a new process.
		if mark := signaledTargets[pid]; mark.start != tracked.start || mark.sig != unix.SIGKILL {
			if sig == unix.SIGKILL {
				sent++
			}
			signaledTargets[pid] = signalMark{sig: sig, start: tracked.start}
		}
	}
	return sent
}

func reapCommandGroupZombies(leader int) error {
	self := os.Getpid()
	deadline := time.Now().Add(time.Second)
	var failures stopFailures
	for {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return fmt.Errorf("read process list while reaping command group %d: %w", leader, err)
		}
		active := false
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid <= 1 {
				continue
			}
			stat, err := ReadStat("/proc", pid)
			if Gone(err) || errors.Is(err, os.ErrPermission) {
				continue
			}
			if err != nil {
				failures.add(pid, fmt.Errorf("read process stat while reaping command group: %w", err))
				continue
			}
			if !directCommandGroupChild(pid, leader, self, stat) {
				continue
			}
			if commandGroupZombie(pid, leader, self, stat) {
				var status syscall.WaitStatus
				waited, waitErr := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
				if errors.Is(waitErr, syscall.ECHILD) {
					continue
				}
				if waitErr != nil {
					failures.add(pid, fmt.Errorf("wait for adopted command-group child: %w", waitErr))
					continue
				}
				if waited != 0 && waited != pid {
					failures.add(pid, fmt.Errorf("wait returned unexpected PID %d", waited))
				}
				continue
			}
			if stat.State != "Z" {
				active = true
			}
		}
		if !active || time.Now().After(deadline) {
			return failures.err()
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func directCommandGroupChild(pid, leader, self int, stat Stat) bool {
	return pid != leader && stat.Ppid == self && stat.Pgid == leader
}

func commandGroupZombie(pid, leader, self int, stat Stat) bool {
	return directCommandGroupChild(pid, leader, self, stat) && stat.State == "Z"
}

// signalMark records the strongest signal sent to one process identity.
type signalMark struct {
	sig   unix.Signal
	start string
}
