package main

import (
	"strings"
	"testing"
)

// A date with impossible hours gives no deadline, and the warning says so
// instead of claiming there is no time at the start.
func TestWaitWarnsOnImpossibleHoursAfterDate(t *testing.T) {
	for _, tc := range []struct{ until, want string }{
		{"12.11, 25:00 build done", "date whose time is impossible"},
		{"12.11 9:5 build done", "date whose time is impossible"},
		{"2026-11-12 25:00 build done", "date whose time is impossible"},
		{"build done", "has no time at the start"},
	} {
		var errout strings.Builder
		o := &options{cmd: "wait", until: tc.until, by: "someone"}
		if _, err := transitionBody("# t\n\nСтатус: открыт\n", o, "ждёт", &errout); err != nil {
			t.Fatalf("%q: %v", tc.until, err)
		}
		if !strings.Contains(errout.String(), tc.want) {
			t.Errorf("%q: warning %q, want %q", tc.until, errout.String(), tc.want)
		}
	}
}
