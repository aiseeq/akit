package proc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func fakeStat(t *testing.T, root string, pid, parent int, name, state string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("%d (%s) %s %d 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 42\n", pid, name, state, parent)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	fakeExe(t, root, pid, "/usr/bin/"+name)
}

// fakeExe points the fake process's exe link at a program.
func fakeExe(t *testing.T, root string, pid int, exe string) {
	t.Helper()
	link := filepath.Join(root, strconv.Itoa(pid), "exe")
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
}

func TestValidateStop(t *testing.T) {
	root := t.TempDir()
	for _, p := range []struct {
		pid, parent int
		name, state string
	}{
		{1, 0, "init", "S"}, {10, 1, "systemd", "S"}, {20, 10, "bash", "S"}, {30, 20, "bash", "S"},
		{40, 30, "stop", "S"}, {50, 20, "worker", "S"}, {51, 50, "worker", "Z"}, {60, 1, "worker", "S"}, {70, 20, "systemd", "S"},
	} {
		fakeStat(t, root, p.pid, p.parent, p.name, p.state)
	}
	for _, pid := range []int{-50, -1, 0, 1, 10, 20, 30, 40, 60, 70} {
		t.Run(strconv.Itoa(pid), func(t *testing.T) {
			if _, err := validateStop(root, 40, pid, true); err == nil {
				t.Fatalf("accepted %d", pid)
			}
		})
	}
	for _, pid := range []int{50, 51} {
		if _, err := validateStop(root, 40, pid, true); err != nil {
			t.Fatalf("pid %d: %v", pid, err)
		}
	}
	if _, err := validateStop(root, 40, 50, false); err == nil {
		t.Fatal("accepted unrelated child")
	}
}

func TestServiceManagerNeverOwnerOrTarget(t *testing.T) {
	for _, manager := range []string{"systemd", "SYSTEMD"} {
		t.Run(manager, func(t *testing.T) {
			root := t.TempDir()
			for _, p := range []struct {
				pid, parent int
				name        string
			}{
				{1, 0, "init"}, {10, 1, manager}, {20, 10, "bash"},
				{30, 20, "stop"}, {40, 10, "worker"}, {50, 20, "worker"},
			} {
				fakeStat(t, root, p.pid, p.parent, p.name, "S")
			}
			if _, err := validateStop(root, 30, 40, true); err == nil {
				t.Error("manager became shell ancestor owner")
			}
			if _, err := validateStop(root, 20, 40, true); err == nil {
				t.Error("manager became direct parent owner")
			}
			if _, err := validateStop(root, 30, 50, true); err != nil {
				t.Errorf("shell child rejected: %v", err)
			}
			fakeStat(t, root, 60, 20, manager, "S")
			if _, err := validateStop(root, 30, 60, true); err == nil {
				t.Error("manager target accepted")
			}
		})
	}
}

func TestZombieReturnsImmediately(t *testing.T) {
	root := t.TempDir()
	fakeStat(t, root, 1, 0, "init", "S")
	fakeStat(t, root, 10, 1, "parent", "S")
	fakeStat(t, root, 20, 10, "child", "Z")
	result, err := stopTree(root, 10, 20, 0, func() error { t.Fatal("request called"); return nil }, 0)
	if err != nil || !result.AlreadyExited {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

// A signal that fails for one process does not keep the request or SIGKILL
// from the others, and the error names the failed PID.
func TestStopContinuesAfterSignalFailure(t *testing.T) {
	// The child starts a grandchild that writes a marker on TERM, unless the
	// child ignores TERM itself: then the grandchild inherits the ignore.
	const script = `[ "$3" = ignore ] && trap '' TERM
sh -c 'trap "echo > \"$1\"; exit 0" TERM; echo $$ > "$2.tmp"; mv "$2.tmp" "$2"; while :; do sleep 0.05; done' sh "$1" "$2" &
while :; do sleep 0.05; done`
	for _, tc := range []struct {
		name    string
		failSig unix.Signal
		mode    string
		failure string
		marker  bool
	}{
		{"request fails", unix.SIGTERM, "", "request", true},
		{"SIGKILL fails", unix.SIGKILL, "ignore", "SIGKILL", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			marker, pidFile := filepath.Join(dir, "marker"), filepath.Join(dir, "pid")
			cmd := exec.Command("sh", "-c", script, "sh", marker, pidFile, tc.mode)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			child := cmd.Process.Pid
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			waitReady(t, pidFile)
			b, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			grandchild, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatal(err)
			}
			real := signalProcess
			t.Cleanup(func() { signalProcess = real })
			signalProcess = func(pid int, p *trackedProcess, sig unix.Signal) error {
				if pid == child && sig == tc.failSig {
					return unix.EPERM
				}
				return real(pid, p, sig)
			}
			result, err := stopTree("/proc", os.Getpid(), child, 300*time.Millisecond, nil, unix.SIGTERM)
			want := fmt.Sprintf("PID %d: %s", child, tc.failure)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error %v, want it to name %q", err, want)
			}
			if result.Killed == 0 {
				t.Errorf("no process got SIGKILL: %+v", result)
			}
			if _, err := os.Stat(marker); (err == nil) != tc.marker {
				t.Errorf("grandchild marker present=%v, want %v", err == nil, tc.marker)
			}
			if s, err := ReadStat("/proc", grandchild); err == nil && s.State != "Z" {
				t.Errorf("grandchild %d still alive: %s", grandchild, s.State)
			}
		})
	}
}
