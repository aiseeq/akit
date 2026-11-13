//go:build linux

package proc

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
)

// Cancel runs in os/exec's context goroutine while Wait records ProcessState;
// helpers called from Cancel must not read that field. Run with -race.
func TestCmdHelpersCalledFromCancelDoNotRaceWithWait(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel func(*exec.Cmd) error
	}{
		{"SignalCmd", func(cmd *exec.Cmd) error { return SignalCmd(cmd, syscall.SIGKILL) }},
		{"SignalCmdWithResult", func(cmd *exec.Cmd) error { _, err := SignalCmdWithResult(cmd, syscall.SIGKILL); return err }},
		{"StopCmd", func(cmd *exec.Cmd) error { _, err := StopCmd(cmd, 0, syscall.SIGKILL); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cmd := exec.CommandContext(ctx, "sleep", "30")
			cmd.Cancel = func() error { return tc.cancel(cmd) }
			if err := cmd.Start(); err != nil {
				cancel()
				t.Fatal(err)
			}
			cancel()
			if err := cmd.Wait(); err == nil {
				t.Fatal("Wait succeeded; want the child stopped by Cancel")
			}
		})
	}
}
