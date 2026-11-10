package timearg

import (
	"testing"
	"time"
)

// Hours after a date are read through a comma or a colon too, and impossible
// or short hours there give no deadline instead of the date's midnight.
func TestDeadlineSeparatorsAndShortHours(t *testing.T) {
	loc := time.FixedZone("CET", 3600)
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, loc)
	for wait, want := range map[string]time.Time{
		"12.11, 9:00 x":  time.Date(2026, 11, 12, 9, 0, 0, 0, loc),
		"12.11: 9:00 x":  time.Date(2026, 11, 12, 9, 0, 0, 0, loc),
		"12.11, 9:00":    time.Date(2026, 11, 12, 9, 0, 0, 0, loc),
		"12.11, текст":   time.Date(2026, 11, 12, 0, 0, 0, 0, loc),
		"12.11: 3 файла": time.Date(2026, 11, 12, 0, 0, 0, 0, loc),
	} {
		if at, ok := Deadline(wait, now); !ok || !at.Equal(want) {
			t.Errorf("Deadline(%q) = %v, %v; want %v", wait, at, ok, want)
		}
	}
	for _, wait := range []string{"12.11, 25:00 x", "12.11: 25:00 x", "12.11 9:5 x", "12.11, 9:5"} {
		if at, ok := Deadline(wait, now); ok {
			t.Errorf("Deadline(%q) = %v; want no deadline", wait, at)
		}
	}
}
