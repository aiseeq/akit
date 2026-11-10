package main

import (
	"testing"

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
