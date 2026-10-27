package timearg

import (
	"testing"
	"time"
)

func TestParseTime(t *testing.T) {
	loc := time.FixedZone("UTC+1", 3600)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, loc)
	cases := map[string]time.Time{
		"30m":                  now.Add(-30 * time.Minute),
		"2d":                   now.Add(-48 * time.Hour),
		"10:05":                time.Date(2026, 10, 25, 10, 5, 0, 0, loc),
		"24.10":                time.Date(2026, 10, 24, 0, 0, 0, 0, loc),
		"24.10 23:09":          time.Date(2026, 10, 24, 23, 9, 0, 0, loc),
		"2026-10-20":           time.Date(2026, 10, 20, 0, 0, 0, 0, loc),
		"2026-10-25T09:00:00Z": time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := Parse(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: got %v %v, want %v", in, got, err, want)
		}
	}
	if _, err := Parse("вчера", now); err == nil {
		t.Error("bad time accepted")
	}
}
