package main

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

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
		1: "stop: 7 killed 1 process after 2s grace",
		3: "stop: 7 killed 3 processes after 2s grace",
	} {
		if got := killedLine(7, killed, 2*time.Second); got != want {
			t.Errorf("killed %d: %q, want %q", killed, got, want)
		}
	}
}
