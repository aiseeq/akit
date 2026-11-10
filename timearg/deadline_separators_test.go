package timearg

import (
	"testing"
	"time"
)

// Hours after a date are read through a comma, a colon or a semicolon and a
// space too, and impossible
// or short hours there give no deadline instead of the date's midnight.
func TestDeadlineSeparatorsAndShortHours(t *testing.T) {
	loc := time.FixedZone("CET", 3600)
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, loc)
	for wait, want := range map[string]time.Time{
		"12.11, 9:00 x":     time.Date(2026, 11, 12, 9, 0, 0, 0, loc),
		"12.11: 9:00 x":     time.Date(2026, 11, 12, 9, 0, 0, 0, loc),
		"12.11; 9:00 x":     time.Date(2026, 11, 12, 9, 0, 0, 0, loc),
		"12.11,9:00 x":      time.Date(2026, 11, 12, 9, 0, 0, 0, loc),
		"12.11:9:00 x":      time.Date(2026, 11, 12, 9, 0, 0, 0, loc),
		"12.11;9:00 x":      time.Date(2026, 11, 12, 9, 0, 0, 0, loc),
		"2026-11-12,9:00 x": time.Date(2026, 11, 12, 9, 0, 0, 0, loc),
		"2026-11-12:9:00 x": time.Date(2026, 11, 12, 9, 0, 0, 0, loc),
		"2026-11-12;9:00 x": time.Date(2026, 11, 12, 9, 0, 0, 0, loc),

		"12.11; текст":       time.Date(2026, 11, 12, 0, 0, 0, 0, loc),
		"12.11;собрать":      time.Date(2026, 11, 12, 0, 0, 0, 0, loc),
		"2026-11-12;собрать": time.Date(2026, 11, 12, 0, 0, 0, 0, loc),
		"12.11, 9:00":        time.Date(2026, 11, 12, 9, 0, 0, 0, loc),
		"12.11, текст":       time.Date(2026, 11, 12, 0, 0, 0, 0, loc),
		"12.11: 3 файла":     time.Date(2026, 11, 12, 0, 0, 0, 0, loc),
	} {
		if at, ok := Deadline(wait, now); !ok || !at.Equal(want) {
			t.Errorf("Deadline(%q) = %v, %v; want %v", wait, at, ok, want)
		}
	}
	for _, wait := range []string{"12.11, 25:00 x", "12.11: 25:00 x", "12.11 9:5 x", "12.11, 9:5", "12.11; 25:00 x", "12.11;25:00 x", "2026-11-12;25:00 x"} {
		if at, ok := Deadline(wait, now); ok {
			t.Errorf("Deadline(%q) = %v; want no deadline", wait, at)
		}
	}
}

func TestDeadlineUTCAfterDateClock(t *testing.T) {
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.FixedZone("CET", 3600))
	for _, wait := range []string{"12.11;9:00 UTC x", "12.11;9:00Z x", "2026-11-12;9:00 UTC x", "2026-11-12;9:00Z x", "12.11 9:00: UTC x", "12.11 9:00, Z x", "2026-11-12 09:00: UTC x"} {
		at, ok := Deadline(wait, now)
		want := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
		if !ok || !at.Equal(want) || at.Location() != time.UTC {
			t.Errorf("Deadline(%q) = %v, %v; want %v UTC", wait, at, ok, want)
		}
	}
	// A zone after the separator of a date without hours is midnight UTC.
	for _, wait := range []string{"12.11; Z", "12.11, UTC x", "2026-11-12; UTC"} {
		at, ok := Deadline(wait, now)
		want := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
		if !ok || !at.Equal(want) || at.Location() != time.UTC {
			t.Errorf("Deadline(%q) = %v, %v; want %v UTC", wait, at, ok, want)
		}
	}
}
