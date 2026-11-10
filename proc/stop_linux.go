package proc

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// StopResult reports an already exited target and the number sent SIGKILL.
type StopResult struct {
	AlreadyExited bool
	Killed        int
}

// StopChild stops a direct child and its observed descendants. request is called
// once after the initial tree is captured; it may signal, close stdin or cancel
// a context. The caller remains responsible for Cmd.Wait and WaitDelay.
// Descendants that leave the tree before they can be observed cannot be found.
func StopChild(pid int, grace time.Duration, request func() error) (StopResult, error) {
	return stopTree("/proc", os.Getpid(), pid, grace, request, 0)
}

// StopFromShell requests every observed process to exit and stops survivors.
// It permits descendants of the caller's parent and consecutive shell ancestors.
// This ancestry rule is not a security boundary between jobs of the same shell.
func StopFromShell(pid int, grace time.Duration, sig unix.Signal) (StopResult, error) {
	if sig <= 0 || sig > 64 {
		return StopResult{}, fmt.Errorf("invalid signal %d", sig)
	}
	return stopTree("/proc", os.Getpid(), pid, grace, nil, sig)
}

func serviceManager(pid int, s Stat) bool {
	return pid <= 1 || strings.EqualFold(s.Comm, "systemd")
}

func isShell(comm string) bool {
	switch comm {
	case "bash", "sh", "dash", "zsh", "ksh":
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
	for p := s.Ppid; p > 1; {
		s, err = ReadStat(root, p)
		if err != nil {
			return nil, err
		}
		if serviceManager(p, s) {
			break
		}
		owners[p] = true
		if !isShell(s.Comm) {
			break
		}
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
	fd        int
	start     string
	requested bool
}

type processTree map[int]*trackedProcess

func (tree processTree) close() {
	for _, p := range tree {
		_ = unix.Close(p.fd)
	}
}

func openStopTarget(root string, self, pid int, sig unix.Signal) (processTree, bool, error) {
	target, err := validateStop(root, self, pid, sig != 0)
	if err != nil {
		return nil, false, err
	}
	if sig == 0 && target.Ppid != self {
		return nil, false, fmt.Errorf("PID %d is not a direct child", pid)
	}
	if target.State == "Z" || target.State == "X" {
		return nil, true, nil
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, false, fmt.Errorf("pidfd_open %d: %w", pid, err)
	}
	checked, err := validateStop(root, self, pid, sig != 0)
	if err != nil || checked.Start != target.Start {
		_ = unix.Close(fd)
		return nil, false, fmt.Errorf("PID %d changed during validation", pid)
	}
	return processTree{pid: {fd: fd, start: target.Start}}, false, nil
}

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
	if err := captureDescendants(root, tree); err != nil {
		return StopResult{}, err
	}
	if sig == 0 {
		if request == nil {
			return StopResult{}, errors.New("missing stop request")
		}
		if err := request(); err != nil {
			return StopResult{}, fmt.Errorf("stop request: %w", err)
		}
	} else if err := requestTree(tree, pid, sig); err != nil {
		return StopResult{}, err
	}
	alive, err := waitTree(root, tree, pid, sig, grace)
	if err != nil || !alive {
		return StopResult{}, err
	}
	return killTree(tree)
}

func sendRequest(pid int, p *trackedProcess, sig unix.Signal) error {
	if p.requested {
		return nil
	}
	p.requested = true
	err := unix.PidfdSendSignal(p.fd, sig, nil, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("request PID %d: %w", pid, err)
	}
	return nil
}

func requestTree(tree processTree, leader int, sig unix.Signal) error {
	if err := sendRequest(leader, tree[leader], sig); err != nil {
		return err
	}
	for pid, p := range tree {
		if pid != leader {
			if err := sendRequest(pid, p, sig); err != nil {
				return err
			}
		}
	}
	return nil
}

func processAlive(pid int, p *trackedProcess) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 0)
	if err != nil {
		return false, fmt.Errorf("poll PID %d: %w", pid, err)
	}
	return n == 0, nil
}

func treeAlive(tree processTree) (bool, error) {
	alive := false
	for pid, p := range tree {
		running, err := processAlive(pid, p)
		if err != nil {
			return false, err
		}
		alive = alive || running
	}
	return alive, nil
}

func waitTree(root string, tree processTree, leader int, sig unix.Signal, grace time.Duration) (bool, error) {
	deadline := time.Now().Add(grace)
	for {
		if err := captureDescendants(root, tree); err != nil {
			return false, err
		}
		if sig != 0 {
			if err := requestTree(tree, leader, sig); err != nil {
				return false, err
			}
		}
		alive, err := treeAlive(tree)
		if err != nil || !alive {
			return alive, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return true, nil
		}
		if remaining > 20*time.Millisecond {
			remaining = 20 * time.Millisecond
		}
		time.Sleep(remaining)
	}
}

func killTree(tree processTree) (StopResult, error) {
	killed := processTree{}
	for pid, p := range tree {
		alive, err := processAlive(pid, p)
		if err != nil {
			return StopResult{}, err
		}
		if !alive {
			continue
		}
		err = unix.PidfdSendSignal(p.fd, unix.SIGKILL, nil, 0)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return StopResult{}, fmt.Errorf("SIGKILL PID %d: %w", pid, err)
		}
		killed[pid] = p
	}
	result := StopResult{Killed: len(killed)}
	deadline := time.Now().Add(5 * time.Second)
	for len(killed) > 0 {
		for pid, p := range killed {
			alive, err := processAlive(pid, p)
			if err != nil {
				return result, err
			}
			if !alive {
				delete(killed, pid)
				continue
			}
			if time.Now().After(deadline) {
				return result, fmt.Errorf("PID %d did not exit after SIGKILL", pid)
			}
		}
		if len(killed) > 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return result, nil
}

func captureDescendants(root string, tree processTree) error {
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
				return err
			}
			changed = changed || added
		}
	}
	return nil
}

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
		return false, fmt.Errorf("pidfd_open %d: %w", pid, err)
	}
	current, err := ReadStat(root, pid)
	if err != nil {
		_ = unix.Close(fd)
		if Gone(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading pid %d: %w", pid, err)
	}
	if current.Start != s.Start || current.Ppid != s.Ppid {
		_ = unix.Close(fd)
		return false, nil
	}
	tree[pid] = &trackedProcess{fd: fd, start: s.Start}
	return true, nil
}
