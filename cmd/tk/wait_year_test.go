package main

import (
	"testing"
	"time"
)

// A new wait written as «DD.MM» is stored with the nearest future year.
func TestWaitWriteStoresYear(t *testing.T) {
	for _, c := range []struct {
		wait string
		now  time.Time
		want string
	}{
		{"05.01 10:00: x", time.Date(2026, 12, 20, 9, 0, 0, 0, time.Local), "2027-01-05 10:00: x"},
		{"10.11 12:00: x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-10 12:00: x"},
		{"10.11: x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-10: x"},
		{"12:00 x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-09 12:00 x"},
		{"после ответа", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "после ответа"},
	} {
		if got := datedWait(c.wait, c.now); got != c.want {
			t.Errorf("datedWait(%q) at %v = %q, want %q", c.wait, c.now, got, c.want)
		}
	}
}

// An old «Ждёт:» line without a year keeps the date its author meant across New Year.
func TestWaitDueOldLineWithoutYear(t *testing.T) {
	for _, c := range []struct {
		wait     string
		now, due time.Time
	}{
		{"31.12 23:59: x", time.Date(2027, 1, 2, 9, 0, 0, 0, time.Local), time.Date(2026, 12, 31, 23, 59, 0, 0, time.Local)},
		{"10.01 12:00: x", time.Date(2026, 12, 20, 9, 0, 0, 0, time.Local), time.Date(2027, 1, 10, 12, 0, 0, 0, time.Local)},
		{"31.10 12:00 после замера", time.Date(2026, 10, 31, 15, 0, 0, 0, time.Local), time.Date(2026, 10, 31, 12, 0, 0, 0, time.Local)},
	} {
		got, ok := waitDue(c.wait, c.now)
		if !ok || !got.Equal(c.due) {
			t.Errorf("waitDue(%q) at %v = %v, %v; want %v", c.wait, c.now, got, ok, c.due)
		}
	}
}
