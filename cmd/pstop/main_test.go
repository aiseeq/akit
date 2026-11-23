package main

import (
	"bytes"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
	"golang.org/x/sys/unix"
)

func TestRunOwnUnitFlagRoutesToOwnUnitAction(t *testing.T) {
	oldOwn, oldShell := stopOwnUnitAction, stopFromShellAction
	defer func() { stopOwnUnitAction, stopFromShellAction = oldOwn, oldShell }()
	calls := 0
	stopOwnUnitAction = func(pid int, grace time.Duration, sig syscall.Signal) (proc.StopResult, error) {
		calls++
		if pid != 77 || grace != 1500*time.Millisecond || sig != syscall.SIGKILL {
			t.Fatalf("own-unit args = (%d, %s, %s)", pid, grace, unix.SignalName(unix.Signal(sig)))
		}
		return proc.StopResult{AlreadyExited: true}, nil
	}
	stopFromShellAction = func(int, time.Duration, unix.Signal) (proc.StopResult, error) {
		t.Fatal("--own-unit routed through StopFromShell")
		return proc.StopResult{}, nil
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"--own-unit", "--grace", "1500ms", "--signal", "KILL", "77"}, &stdout, &stderr)
	if code != 0 || calls != 1 || stdout.String() != "pstop: 77 already exited\n" || stderr.Len() != 0 {
		t.Fatalf("run = code %d, calls %d, stdout %q, stderr %q", code, calls, stdout.String(), stderr.String())
	}
}

func TestParseSignal(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  unix.Signal
	}{{"TERM", unix.SIGTERM}, {"sigterm", unix.SIGTERM}, {"9", unix.SIGKILL}} {
		got, err := parseSignal(tc.input)
		if err != nil || got != tc.want {
			t.Errorf("%s: %v %v", tc.input, got, err)
		}
	}
	for _, s := range []string{"0", "-1", "65", "NOTASIGNAL"} {
		if _, err := parseSignal(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}

func TestKilledLine(t *testing.T) {
	for killed, want := range map[int]string{
		1: "pstop: 7 killed 1 process after 2s grace",
		3: "pstop: 7 killed 3 processes after 2s grace",
	} {
		if got := killedLine(7, killed, 2*time.Second); got != want {
			t.Errorf("killed %d: %q, want %q", killed, got, want)
		}
	}
}
