package proc

import (
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

// Only consecutive shells extend ownership: a non-shell ancestor above the
// parent ends the chain.
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
	if _, err := validateStop(root, 40, 60, true); err != nil {
		t.Errorf("refused 60 under the shell's parent: %v", err)
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
