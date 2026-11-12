//go:build linux

package proc

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// An orphaned shell is adopted by the user service manager. The manager must
// not become an owner: its other children are not the caller's descendants.
func TestValidateStopManagerIsNeverOwner(t *testing.T) {
	root := t.TempDir()
	for _, p := range []struct {
		pid, parent int
		name        string
	}{
		{1, 0, "systemd"}, {10, 1, "systemd"}, {20, 10, "bash"}, {30, 20, "stop"},
		{40, 10, "terminal"}, {41, 40, "bash"}, {50, 20, "worker"},
	} {
		fakeStat(t, root, p.pid, p.parent, p.name, "S")
	}
	for _, pid := range []int{40, 41} {
		if _, err := validateStop(root, 30, pid, true); err == nil {
			t.Errorf("accepted %d, a child of the service manager", pid)
		}
	}
	if _, err := validateStop(root, 30, 50, true); err != nil {
		t.Errorf("refused own shell's child 50: %v", err)
	}
}

// Only the parent and consecutive shells above it own targets: the non-shell
// process above the shells (a terminal, a multiplexer, an agent) does not, so
// a job of another tab or call is refused.
func TestValidateStopChainEndsAtNonShell(t *testing.T) {
	root := t.TempDir()
	for _, p := range []struct {
		pid, parent int
		name        string
	}{
		{1, 0, "init"}, {10, 1, "tmux"}, {20, 10, "editor"}, {30, 20, "bash"}, {40, 30, "stop"},
		{50, 10, "bash"}, {60, 20, "worker"},
	} {
		fakeStat(t, root, p.pid, p.parent, p.name, "S")
	}
	if _, err := validateStop(root, 40, 50, true); err == nil {
		t.Error("accepted 50 under tmux, above the non-shell editor")
	}
	if _, err := validateStop(root, 40, 60, true); err == nil {
		t.Error("accepted 60 under the non-shell editor above the shell")
	}
	fakeStat(t, root, 70, 30, "worker", "S")
	if _, err := validateStop(root, 40, 70, true); err != nil {
		t.Errorf("refused 70 under the shell: %v", err)
	}
}

// A shell running stop as its last command replaces itself with stop, so the
// parent seen is the program that started the shell (an agent, a terminal).
// A non-shell parent is never an owner: its other jobs are refused, and only
// stop's own children remain reachable.
func TestValidateStopNonShellParentIsNotOwner(t *testing.T) {
	root := t.TempDir()
	for _, p := range []struct {
		pid, parent int
		name        string
	}{
		{1, 0, "init"}, {10, 1, "agent"}, {20, 10, "bash"}, {30, 20, "sleep"},
		{40, 10, "stop"}, {50, 10, "server"}, {60, 40, "sleep"},
	} {
		fakeStat(t, root, p.pid, p.parent, p.name, "S")
	}
	for _, pid := range []int{20, 30, 50} {
		if _, err := validateStop(root, 40, pid, true); err == nil {
			t.Errorf("accepted %d, a job of the non-shell parent", pid)
		}
	}
	if _, err := validateStop(root, 40, 60, true); err != nil {
		t.Errorf("refused stop's own child 60: %v", err)
	}
}

// The library entry accepts only a direct child and a bounded grace.
func TestStopTreeLibraryLimits(t *testing.T) {
	root := t.TempDir()
	fakeStat(t, root, 1, 0, "init", "S")
	fakeStat(t, root, 10, 1, "caller", "S")
	fakeStat(t, root, 20, 10, "child", "S")
	fakeStat(t, root, 30, 20, "grandchild", "S")
	never := func() error { t.Fatal("request called"); return nil }
	if _, err := stopTree(root, 10, 30, 0, never, 0); err == nil || !strings.Contains(err.Error(), "direct child") {
		t.Errorf("library accepted a grandchild: %v", err)
	}
	for _, g := range []time.Duration{-time.Nanosecond, 24*time.Hour + time.Nanosecond} {
		t.Run(strconv.FormatInt(int64(g), 10), func(t *testing.T) {
			if _, err := stopTree(root, 10, 20, g, never, 0); err == nil || !strings.Contains(err.Error(), "grace") {
				t.Errorf("accepted grace %v: %v", g, err)
			}
		})
	}
}

// A script started by its path with a #!/bin/sh line is named after the script,
// so a shell is told by the program it runs: the script's shell owns its jobs,
// a renamed non-shell does not, and an unreadable exe ends the chain.
func TestValidateStopShellByProgram(t *testing.T) {
	root := t.TempDir()
	for _, p := range []struct {
		pid, parent int
		name        string
	}{
		{1, 0, "init"}, {10, 1, "agent"}, {20, 10, "bash"}, {30, 20, "run-jobs"},
		{40, 30, "stop"}, {50, 20, "sleep"}, {60, 30, "sleep"},
	} {
		fakeStat(t, root, p.pid, p.parent, p.name, "S")
	}
	fakeExe(t, root, 30, "/usr/bin/bash")
	for _, pid := range []int{50, 60} {
		if _, err := validateStop(root, 40, pid, true); err != nil {
			t.Errorf("refused %d under the script's shell chain: %v", pid, err)
		}
	}
	fakeStat(t, root, 70, 10, "bash", "S")
	fakeExe(t, root, 70, "/usr/bin/python3")
	fakeStat(t, root, 80, 70, "stop", "S")
	fakeStat(t, root, 90, 70, "sleep", "S")
	if _, err := validateStop(root, 80, 90, true); err == nil {
		t.Error("accepted a job of a non-shell renamed bash")
	}
	if err := os.Remove(root + "/30/exe"); err != nil {
		t.Fatal(err)
	}
	if _, err := validateStop(root, 40, 50, true); err == nil {
		t.Error("accepted 50 above a parent with an unreadable exe")
	}
}
