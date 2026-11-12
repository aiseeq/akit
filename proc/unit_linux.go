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
	count := 0
	var failures []error
	for line := range strings.FieldsSeq(string(procs)) {
		pid, e := strconv.Atoi(line)
		if e != nil || pid <= 1 || pid == self || pid == ppid {
			continue
		}
		sent, e := signalOwnUnitProcess(procRoot, cg, pid, sig)
		if e != nil {
			failures = append(failures, e)
		} else if sent {
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

func signalOwnUnitProcess(procRoot, cg string, pid int, sig syscall.Signal) (bool, error) {
	st, err := ReadStat(procRoot, pid)
	if Gone(err) || err == nil && (st.State == "Z" || st.State == "X") {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read PID %d stat: %w", pid, err)
	}
	member, err := readProcessCgroup(procRoot, pid)
	if Gone(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read PID %d cgroup: %w", pid, err)
	}
	if member != cg {
		return false, nil
	}
	fd, err := ownUnitPidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, pidfdOpenError(fmt.Errorf("pidfd_open PID %d: %w", pid, err))
	}
	defer unix.Close(fd)
	ownUnitAfterPidfdOpen(pid)
	checked, err := ReadStat(procRoot, pid)
	if Gone(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("recheck PID %d stat: %w", pid, err)
	}
	if checked.Start != st.Start || checked.State == "Z" || checked.State == "X" {
		return false, nil
	}
	member, err = readProcessCgroup(procRoot, pid)
	if Gone(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("recheck PID %d cgroup: %w", pid, err)
	}
	if member != cg {
		return false, nil
	}
	if err := ownUnitSignal(pid, &trackedProcess{fd: fd, start: st.Start}, unix.Signal(sig)); err != nil {
		return false, fmt.Errorf("signal PID %d: %w", pid, err)
	}
	return true, nil
}

func readProcessCgroup(procRoot string, pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", fmt.Errorf("read cgroup for PID %d: %w", pid, err)
	}
	return ParseCgroup(string(data)), nil
}
