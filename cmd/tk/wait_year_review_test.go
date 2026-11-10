package main

import (
	"testing"
	"time"
)

// A wait for today stays today, even when its clock has passed; a full date or a
// glued word is not rewritten as a day and month.
func TestWaitWriteTodayAndFullDates(t *testing.T) {
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local)
	for _, c := range []struct{ wait, want string }{
		{"10.11: x", "2026-11-10: x"},
		{"10.11 08:00: x", "2026-11-10 08:00: x"},
		{"09.11: x", "2027-11-09: x"},
		{"10.11.2026 x", "10.11.2026 x"},
		{"10.11.2026: x", "10.11.2026: x"},
		{"10.11x", "10.11x"},
		{"31.11 x", "31.11 x"},
	} {
		if got := datedWait(c.wait, now); got != c.want {
			t.Errorf("datedWait(%q) = %q, want %q", c.wait, got, c.want)
		}
	}
}
