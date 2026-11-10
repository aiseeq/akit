package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// StopChild on a real child: a child that exits on the request needs no
// SIGKILL; one that ignores it is killed after the grace.
func TestStopChildLive(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		killed bool
	}{
		{"exits on request", "echo > \"$1\"; exec sleep 30", false},
		{"ignores request", "trap '' TERM; echo > \"$1\"; while :; do sleep 0.05; done", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "ready")
			cmd := exec.Command("sh", "-c", tc.script, "sh", ready)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waitReady(t, ready)
			started := time.Now()
			result, err := StopChild(cmd.Process.Pid, 300*time.Millisecond, func() error {
				return cmd.Process.Signal(syscall.SIGTERM)
			})
			_ = cmd.Wait()
			if err != nil {
				t.Fatal(err)
			}
			if got := result.Killed > 0; got != tc.killed {
				t.Errorf("killed %d, want killed=%v", result.Killed, tc.killed)
			}
			if tc.killed && time.Since(started) < 300*time.Millisecond {
				t.Errorf("killed before the grace: %v", time.Since(started))
			}
		})
	}
}

// waitReady waits until the child has written its ready file: its trap is set.
func waitReady(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not get ready: %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
