package main

import (
	"testing"
	"time"
)

// An old line without a year keeps the UTC written after its clock.
func TestWaitDueOldLineWithoutYearKeepsUTC(t *testing.T) {
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.FixedZone("CET", 3600))
	for _, wait := range []string{"10.11 12:00 UTC x", "10.11 12:00 UTC", "10.11 12:00 utc: x"} {
		got, ok := waitDue(wait, now)
		if want := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC); !ok || !got.Equal(want) {
			t.Errorf("waitDue(%q) = %v, %v; want %v", wait, got, ok, want)
		}
	}
}
