//go:build linux

package proc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var ownUnitProcRoot = "/proc"
var ownUnitCgroupRoot = "/sys/fs/cgroup"
var ownUnitEnv = os.Getenv
var ownUnitPidfdOpen = func(pid, flags int) (int, error) { return openProcessPidfd(pid, flags) }
var ownUnitSignal = signalProcess
var ownUnitAfterPidfdOpen = func(int) {}
var ownUnitMainPID = querySystemdMainPID
var ownUnitDefaultContext = context.Background()

// SignalOwnUnit signals verified members of this systemd service cgroup except
// itself and its parent. It verifies INVOCATION_ID, systemd as parent, and that
// systemd reports this process as MainPID. A process can move cgroups between
// the final membership check and pidfd signal; only the manager or the owner of
// a delegated subtree can do that. If pidfd_open is unavailable, it refuses
// with ErrPidfdUnavailable and sends no signals.
func SignalOwnUnit(sig syscall.Signal) (int, error) {
	return SignalOwnUnitContext(ownUnitDefaultContext, sig)
}

// SignalOwnUnitContext is SignalOwnUnit with a context for the systemd MainPID query.
func SignalOwnUnitContext(ctx context.Context, sig syscall.Signal) (int, error) {
	self := os.Getpid()
	if err := validateSignalTarget(self, sig); err != nil {
		return 0, err
	}
	if err := ensurePidfdAvailable(self); err != nil {
		return 0, err
	}
	return signalOwnUnit(ctx, ownUnitProcRoot, ownUnitCgroupRoot, self, os.Getppid(), sig)
}

func signalOwnUnit(ctx context.Context, procRoot, cgRoot string, self, ppid int, sig syscall.Signal) (int, error) {
	if err := validateSignalTarget(self, sig); err != nil {
		return 0, err
	}
	cg, err := ownServiceCgroup(ctx, procRoot, self, ppid)
	if err != nil {
		return 0, err
	}
	procs, err := os.ReadFile(filepath.Join(cgRoot, strings.TrimPrefix(cg, "/"), "cgroup.procs"))
	if err != nil {
		return 0, fmt.Errorf("read service cgroup.procs: %w", err)
	}
	var targets []*ownUnitTarget
	var failures []error
	for line := range strings.FieldsSeq(string(procs)) {
		pid, e := strconv.Atoi(line)
		if e != nil || pid <= 1 || pid == self || pid == ppid {
			continue
		}
		target, e := prepareOwnUnitProcess(procRoot, cg, pid)
		if e != nil {
			if errors.Is(e, ErrPidfdUnavailable) {
				for _, opened := range targets {
					_ = unix.Close(opened.fd)
				}
				return 0, errors.Join(ErrPidfdUnavailable, e)
			}
			failures = append(failures, e)
		} else if target != nil {
			targets = append(targets, target)
		}
	}
	defer func() {
		for _, target := range targets {
			_ = unix.Close(target.fd)
		}
	}()
	count := 0
	for _, target := range targets {
		if e := ownUnitSignal(target.pid, &trackedProcess{fd: target.fd, start: target.start}, unix.Signal(sig)); e != nil {
			failures = append(failures, fmt.Errorf("signal PID %d: %w", target.pid, e))
		} else {
			count++
		}
	}
	return count, errors.Join(failures...)
}

func ownServiceCgroup(ctx context.Context, procRoot string, self, ppid int) (string, error) {
	if ownUnitEnv("INVOCATION_ID") == "" {
		return "", ErrNoInvocationID
	}
	selfStat, err := ReadStat(procRoot, self)
	if err != nil || selfStat.Ppid != ppid {
		return "", ErrNotServiceMainProcess
	}
	parent, err := ReadStat(procRoot, ppid)
	if err != nil || !serviceManager(ppid, parent) {
		return "", ErrNotSystemdParent
	}
	cgroupBytes, err := os.ReadFile(filepath.Join(procRoot, "self", "cgroup"))
	if err != nil {
		return "", fmt.Errorf("read own cgroup: %w", err)
	}
	cg := ParseCgroup(string(cgroupBytes))
	parts := strings.Split(strings.Trim(cg, "/"), "/")
	if cg == "" || len(parts) == 0 {
		return "", fmt.Errorf("%w: invalid own cgroup", ErrUnsafeUnitCgroup)
	}
	unit := parts[len(parts)-1]
	if !strings.HasSuffix(unit, ".service") || strings.HasPrefix(unit, "user@") {
		return "", fmt.Errorf("%w: leaf %q is not a service", ErrUnsafeUnitCgroup, unit)
	}
	userManager := false
	for _, part := range parts[:len(parts)-1] {
		if strings.HasPrefix(part, "user@") && strings.HasSuffix(part, ".service") {
			userManager = true
			break
		}
	}
	mainPID, err := ownUnitMainPID(ctx, userManager, unit)
	if err != nil || mainPID != self {
		return "", errors.Join(ErrNotUnitMainPID, err)
	}
	return cg, nil
}

func querySystemdMainPID(parent context.Context, userManager bool, unit string) (int, error) {
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		return 0, fmt.Errorf("find systemctl: %w", err)
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	args := []string{"show", "-p", "MainPID", "--value", unit}
	if userManager {
		args = append([]string{"--user"}, args...)
	}
	output, err := exec.CommandContext(ctx, systemctl, args...).Output()
	if err != nil {
		return 0, fmt.Errorf("query systemd MainPID: %w", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 1 {
		return 0, errors.New("systemctl MainPID output is not one number")
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 1 {
		return 0, errors.New("systemctl MainPID is not a valid process ID")
	}
	return pid, nil
}

type ownUnitTarget struct {
	pid   int
	fd    int
	start string
}

func prepareOwnUnitProcess(procRoot, cg string, pid int) (*ownUnitTarget, error) {
	st, err := ReadStat(procRoot, pid)
	if Gone(err) || err == nil && (st.State == "Z" || st.State == "X") {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read PID %d stat: %w", pid, err)
	}
	member, err := readProcessCgroup(procRoot, pid)
	if Gone(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read PID %d cgroup: %w", pid, err)
	}
	if member != cg {
		return nil, nil
	}
	fd, err := ownUnitPidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil, nil
	}
	if err != nil {
		return nil, pidfdOpenError(fmt.Errorf("pidfd_open PID %d: %w", pid, err))
	}
	ownUnitAfterPidfdOpen(pid)
	checked, err := ReadStat(procRoot, pid)
	if Gone(err) {
		_ = unix.Close(fd)
		return nil, nil
	}
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("recheck PID %d stat: %w", pid, err)
	}
	if checked.Start != st.Start || checked.State == "Z" || checked.State == "X" {
		_ = unix.Close(fd)
		return nil, nil
	}
	member, err = readProcessCgroup(procRoot, pid)
	if Gone(err) {
		_ = unix.Close(fd)
		return nil, nil
	}
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("recheck PID %d cgroup: %w", pid, err)
	}
	if member != cg {
		_ = unix.Close(fd)
		return nil, nil
	}
	return &ownUnitTarget{pid: pid, fd: fd, start: st.Start}, nil
}

func readProcessCgroup(procRoot string, pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", fmt.Errorf("read cgroup for PID %d: %w", pid, err)
	}
	return ParseCgroup(string(data)), nil
}
