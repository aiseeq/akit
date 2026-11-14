//go:build linux

package proc

import (
	"context"
	"os"
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

// EINVAL from the probe comes from the kernel and says nothing about Wait.
func TestCmdHelpersKeepSignalingWhenProbeSaysInvalid(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	oldProbe := probeCmdProcess
	t.Cleanup(func() { probeCmdProcess = oldProbe })
	probeCmdProcess = func(*os.Process) error { return syscall.EINVAL }
	if err := SignalCmd(cmd, syscall.SIGKILL); err != nil {
		t.Fatalf("SignalCmd with a Unix EINVAL probe = %v, want the signal delivered", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("Wait succeeded; want the child killed")
	}
}
