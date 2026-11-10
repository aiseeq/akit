package main

import (
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/timearg"
)

// A date with impossible hours gives no deadline, and the warning says so
// instead of claiming there is no time at the start.
func TestWaitWarnsOnImpossibleHoursAfterDate(t *testing.T) {
	for _, tc := range []struct{ until, want string }{
		{"12.11, 25:00 build done", "date and time that do not exist"},
		{"12.11 9:5 build done", "date and time that do not exist"},
		{"2026-11-12 25:00 build done", "date and time that do not exist"},
		{"2026-11-12; 25:00 build done", "date and time that do not exist"},
		{"31.11 9:00 build done", "date and time that do not exist"},
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

// One wait gives one deadline: the text as written and the text tk wait
// stores after dating it agree for every separator after the date.
func TestDatedWaitKeepsDeadline(t *testing.T) {
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.FixedZone("CET", 3600))
	for _, wait := range []string{"12.11, 9:00 x", "12.11: 9:00 x", "12.11; 9:00 x", "12.11,9:00 x", "12.11:9:00 x", "12.11; 25:00 x", "12.11,25:00 x", "12.11; текст"} {
		before, okBefore := timearg.Deadline(wait, now)
		after, okAfter := timearg.Deadline(datedWait(wait, now), now)
		if okBefore != okAfter || !before.Equal(after) {
			t.Errorf("%q: deadline %v %v before dating, %v %v after (%q)", wait, before, okBefore, after, okAfter, datedWait(wait, now))
		}
	}
}
