package timearg

import (
	"strings"
	"testing"
	"time"
)

func TestDeadlineQuirks(t *testing.T) {
	loc := time.FixedZone("CET", 3600)
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, loc)
	// A day with impossible hours is no deadline, not that day at midnight.
	for _, wait := range []string{
		"12.11 25:00 x", "2026-11-12 25:00 x", "12.11 9:61: x",
		"12.11 06:4x", "2026-11-12 06:4х",
		"12.11 06:4x: итог", "12.11 06:4x, итог", "12.11 06:4x; итог",
		"2026-11-12 06:4x: итог", "12.11 06:4xМСК",
	} {
		if at, ok := Deadline(wait, now); ok {
			t.Errorf("Deadline(%q) = %v, want no deadline", wait, at)
		}
	}
	for _, value := range []string{"06:4x", "06:4х", "06:4xМСК", "06:4x:", "06:4x,"} {
		if got, ok := GuessedTime(value); !ok || !strings.HasPrefix(value, got) {
			t.Errorf("GuessedTime(%q) = %q, %v", value, got, ok)
		}
	}
	if _, ok := GuessedTime("06:41"); ok {
		t.Error("exact clock detected as guessed")
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
