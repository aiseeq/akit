//go:build linux

package proc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var ownUnitProcRoot = "/proc"
var ownUnitCgroupRoot = "/sys/fs/cgroup"
var ownUnitEnv = os.Getenv
var ownUnitPidfdOpen = unix.PidfdOpen
var ownUnitSignal = signalProcess
var ownUnitAfterPidfdOpen = func(int) {}

// SignalOwnUnit signals every verified process in this systemd service cgroup except itself.
// It requires a main service process with INVOCATION_ID and systemd as its parent.
func SignalOwnUnit(sig syscall.Signal) (int, error) {
	return signalOwnUnit(ownUnitProcRoot, ownUnitCgroupRoot, os.Getpid(), os.Getppid(), sig)
}

func signalOwnUnit(procRoot, cgRoot string, self, ppid int, sig syscall.Signal) (int, error) {
	if err := validateSignalTarget(self, sig); err != nil {
		return 0, err
	}
	cg, err := ownServiceCgroup(procRoot, self, ppid)
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
		if e != nil || pid <= 1 || pid == self {
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

func ownServiceCgroup(procRoot string, self, ppid int) (string, error) {
	if ownUnitEnv("INVOCATION_ID") == "" {
		return "", errors.New("process has no systemd INVOCATION_ID")
	}
	selfStat, err := ReadStat(procRoot, self)
	if err != nil || selfStat.Ppid != ppid {
		return "", errors.New("process is not the service main process")
	}
	parent, err := ReadStat(procRoot, ppid)
	if err != nil || !serviceManager(ppid, parent) {
		return "", errors.New("process parent is not systemd")
	}
	cgroupBytes, err := os.ReadFile(filepath.Join(procRoot, "self", "cgroup"))
	if err != nil {
		return "", fmt.Errorf("read own cgroup: %w", err)
	}
	cg := ParseCgroup(string(cgroupBytes))
	parts := strings.Split(strings.Trim(cg, "/"), "/")
	if cg == "" || len(parts) == 0 {
		return "", errors.New("invalid own cgroup")
	}
	unit := parts[len(parts)-1]
	if !strings.HasSuffix(unit, ".service") || strings.HasPrefix(unit, "user@") {
		return "", fmt.Errorf("own cgroup leaf %q is not a service", unit)
	}
	return cg, nil
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
		return false, fmt.Errorf("pidfd_open PID %d: %w", pid, err)
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
