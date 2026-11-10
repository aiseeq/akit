package proc

import (
	"os/exec"
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
		{"exits on request", "exec sleep 30", false},
		{"ignores request", "trap '' TERM; while :; do sleep 0.05; done", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", tc.script)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
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
