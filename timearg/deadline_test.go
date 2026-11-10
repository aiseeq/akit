package timearg

import (
	"testing"
	"time"
)

func TestDeadlineQuirks(t *testing.T) {
	loc := time.FixedZone("CET", 3600)
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, loc)
	// A day with impossible hours is no deadline, not that day at midnight.
	for _, wait := range []string{"12.11 25:00 x", "2026-11-12 25:00 x", "12.11 9:61: x"} {
		if at, ok := Deadline(wait, now); ok {
			t.Errorf("Deadline(%q) = %v, want no deadline", wait, at)
		}
	}
	// A one-digit day or month reads as the two-digit one.
	for wait, want := range map[string]time.Time{
		"1.1 x":        time.Date(2027, 1, 1, 0, 0, 0, 0, loc),
		"5.12 9:30: x": time.Date(2026, 12, 5, 9, 30, 0, 0, loc),
	} {
		if at, ok := Deadline(wait, now); !ok || !at.Equal(want) {
			t.Errorf("Deadline(%q) = %v, %v; want %v", wait, at, ok, want)
		}
	}
	// A DayMonth built by the caller works without ParseDayMonth.
	for d, want := range map[DayMonth]time.Time{
		{Date: "31.10"}:                   time.Date(2026, 10, 31, 0, 0, 0, 0, loc),
		{Date: "31.10", Clock: "12:00"}:   time.Date(2026, 10, 31, 12, 0, 0, 0, loc),
		{Date: "31.10", Clock: "9:30:15"}: time.Date(2026, 10, 31, 9, 30, 15, 0, loc),
	} {
		if at, ok := d.At(2026, loc); !ok || !at.Equal(want) {
			t.Errorf("%+v.At = %v, %v; want %v", d, at, ok, want)
		}
	}
}
