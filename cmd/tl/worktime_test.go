package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestActivitySegmentsSplitAtLongGaps(t *testing.T) {
	base := time.Date(2026, 4, 5, 10, 0, 0, 0, time.Local)
	got := activitySegments([]time.Time{base, base.Add(40 * time.Minute)}, 15*time.Minute)
	if len(got) != 2 || got[0].end.Sub(got[0].start) != time.Minute || got[1].end.Sub(got[1].start) != time.Minute {
		t.Fatalf("segments = %+v", got)
	}
	if got := jiraDuration(2 * time.Minute); got != "15m" {
		t.Fatalf("rounded duration = %q", got)
	}
}

func TestUnionIntervalsDoesNotAddOverlap(t *testing.T) {
	base := time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC)
	got := unionIntervals([]interval{{base, base.Add(time.Hour)}, {base.Add(30 * time.Minute), base.Add(90 * time.Minute)}})
	if len(got) != 1 || got[0].end.Sub(got[0].start) != 90*time.Minute {
		t.Fatalf("union = %+v", got)
	}
}

func TestActivityIsClippedToParentWindow(t *testing.T) {
	base := time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC)
	got := clipIntervals([]interval{{base.Add(-time.Hour), base.Add(time.Hour)}},
		interval{base, base.Add(30 * time.Minute)})
	if len(got) != 1 || !got[0].start.Equal(base) || got[0].end.Sub(got[0].start) != 30*time.Minute {
		t.Fatalf("clipped intervals = %+v", got)
	}
}

func TestWorktimeUnionsMatchingSessionsAndIgnoresOthers(t *testing.T) {
	e := fakeHome(t)
	dir := filepath.Join(e.home, ".claude", "projects", "-synthetic-work")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(id string, rows []string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	row := func(at time.Time, key string) string {
		return `{"type":"user","origin":{"kind":"human"},"timestamp":"` + at.Format(time.RFC3339) + `","message":{"role":"user","content":"` + key + `"}}`
	}
	base := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	var a, b, other []string
	for minute := 0; minute <= 60; minute += 10 {
		key := "ongoing work"
		if minute == 0 {
			key += " KEY-123"
		}
		a = append(a, row(base.Add(time.Duration(minute)*time.Minute), key))
	}
	for minute := 30; minute <= 90; minute += 10 {
		key := "parallel work"
		if minute == 30 {
			key += " KEY-123"
		}
		b = append(b, row(base.Add(time.Duration(minute)*time.Minute), key))
	}
	other = append(other, row(base, "unrelated session activity"), row(base.Add(time.Hour), "more unrelated activity"))
	write("10000000-0000-4000-8000-000000000001", a)
	write("20000000-0000-4000-8000-000000000002", b)
	write("30000000-0000-4000-8000-000000000003", other)
	code, out, errOut := runTL(t, e, "--all", "--worktime", "--grep", "KEY-123", "--since", "2026-10-25T09:00:00Z")
	if code != 0 || errOut != "" || !strings.Contains(out, "total\t1h 30m") {
		t.Fatalf("code %d, stderr %q, output:\n%s", code, errOut, out)
	}
}

func TestWorktimeSplitsAcrossLocalMidnight(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("test", 0)
	t.Cleanup(func() { time.Local = old })
	start := time.Date(2026, 4, 5, 23, 55, 0, 0, time.Local)
	spans := unionIntervals(activitySegments([]time.Time{start, start.Add(10 * time.Minute)}, 15*time.Minute))
	days := map[string]time.Duration{}
	for _, span := range spans {
		for at := span.start; at.Before(span.end); {
			local := at.In(time.Local)
			midnight := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, time.Local)
			end := span.end
			if midnight.Before(end) {
				end = midnight
			}
			days[local.Format(time.DateOnly)] += end.Sub(at)
			at = end
		}
	}
	if days["2026-04-05"] != 5*time.Minute || days["2026-04-06"] != 5*time.Minute {
		t.Fatalf("local day split = %v", days)
	}
}

func TestWorktimeNoMatchingSessionReturnsOne(t *testing.T) {
	e := fakeHome(t)
	code, out, _ := runTL(t, e, "--all", "--worktime", "--grep", "absent-key")
	if code != exitNone || !strings.Contains(out, "no sessions with matching key") {
		t.Fatalf("code %d, output %q", code, out)
	}
}

func TestWorktimeReportsEachLocalDayAcrossMidnight(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("synthetic", 2*60*60)
	t.Cleanup(func() { time.Local = old })
	e := fakeHome(t)
	dir := filepath.Join(e.home, ".claude", "projects", "-synthetic-midnight")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	row := func(at time.Time, text string) string {
		return `{"type":"user","origin":{"kind":"human"},"timestamp":"` + at.Format(time.RFC3339) + `","message":{"role":"user","content":"` + text + `"}}`
	}
	start := time.Date(2026, 4, 5, 23, 55, 0, 0, time.Local)
	content := row(start, "KEY-123 start") + "\n" + row(start.Add(10*time.Minute), "activity") + "\n"
	path := filepath.Join(dir, "40000000-0000-4000-8000-000000000004.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runTL(t, e, "--all", "--worktime", "--grep", "KEY-123", "--since", "2026-04-05T00:00:00+02:00", "--until", "2026-04-07T00:00:00+02:00")
	if code != 0 || !strings.Contains(out, "2026-04-05\t15m") || !strings.Contains(out, "2026-04-06\t15m") {
		t.Fatalf("code %d, output:\n%s", code, out)
	}
}
