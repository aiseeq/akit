package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func workRecord(t *testing.T, kind string, at time.Time, text, origin string) string {
	t.Helper()
	record := map[string]any{"type": "user", "timestamp": at.Format(time.RFC3339Nano)}
	if origin != "" {
		record["origin"] = map[string]string{"kind": origin}
	}
	var message map[string]any
	switch kind {
	case "user":
		message = map[string]any{"role": "user", "content": text}
	case "agent":
		record["type"] = "assistant"
		message = map[string]any{"role": "assistant", "id": at.Format(time.RFC3339Nano), "content": []any{map[string]any{"type": "text", "text": text}}}
	case "cmd":
		record["type"] = "assistant"
		message = map[string]any{"role": "assistant", "id": at.Format(time.RFC3339Nano), "content": []any{map[string]any{
			"type": "tool_use", "id": "tool-id", "name": "Task", "input": map[string]string{"command": text},
		}}}
	case "out":
		message = map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tool-id", "content": text}}}
	default:
		t.Fatalf("unknown synthetic role %q", kind)
	}
	record["message"] = message
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func workRows(t *testing.T, start, end time.Time, step time.Duration, build func(time.Time) string) []string {
	t.Helper()
	var rows []string
	for at := start; !at.After(end); at = at.Add(step) {
		rows = append(rows, build(at))
	}
	return rows
}

func workHome(t *testing.T) (env, string) {
	t.Helper()
	e := fakeHome(t)
	e.now = time.Date(2026, 11, 10, 14, 0, 0, 0, time.FixedZone("synthetic", 3600))
	dir := filepath.Join(e.home, ".claude", "projects", "-synthetic-worktime")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return e, dir
}

func writeWorkFile(t *testing.T, path string, rows []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

const workParentID = "90000000-0000-4000-8000-000000000009"

func workParentPath(dir string) string { return filepath.Join(dir, workParentID+".jsonl") }

func workChildPath(dir string) string {
	return filepath.Join(dir, workParentID, "subagents", "agent-a1b2c3d4e5f6.jsonl")
}

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

func TestActivityGapBoundaryAndMinimumDuration(t *testing.T) {
	base := time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC)
	joined := activitySegments([]time.Time{base, base.Add(15 * time.Minute)}, 15*time.Minute)
	if len(joined) != 1 || joined[0].end.Sub(joined[0].start) != 15*time.Minute {
		t.Fatalf("events exactly at gap boundary = %+v", joined)
	}
	short := activitySegments([]time.Time{base, base.Add(30 * time.Second)}, 15*time.Minute)
	if len(short) != 1 || short[0].end.Sub(short[0].start) != time.Minute {
		t.Fatalf("sub-minute activity = %+v", short)
	}
	if got := jiraDuration(time.Hour); got != "1h" {
		t.Fatalf("whole-hour Jira duration = %q", got)
	}
}

func TestUnionIntervalsDoesNotAddOverlap(t *testing.T) {
	base := time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC)
	got := unionIntervals([]interval{{base, base.Add(time.Hour)}, {base.Add(30 * time.Minute), base.Add(90 * time.Minute)},
		{base.Add(35 * time.Minute), base.Add(45 * time.Minute)}, {base.Add(90 * time.Minute), base.Add(2 * time.Hour)}})
	if len(got) != 1 || got[0].end.Sub(got[0].start) != 2*time.Hour {
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
	if got := clipIntervals([]interval{{base.Add(-time.Hour), base}},
		interval{base.Add(time.Minute), base.Add(30 * time.Minute)}); len(got) != 0 {
		t.Fatalf("disjoint interval was retained: %+v", got)
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
		if minute == 0 || minute == 60 {
			key += " KEY-123"
		}
		a = append(a, row(base.Add(time.Duration(minute)*time.Minute), key))
	}
	for minute := 30; minute <= 90; minute += 10 {
		key := "parallel work"
		if minute == 30 || minute == 90 {
			key += " KEY-123"
		}
		b = append(b, row(base.Add(time.Duration(minute)*time.Minute), key))
	}
	other = append(other, row(base.Add(22*time.Hour), "unrelated session activity"), row(base.Add(27*time.Hour), "more unrelated activity"))
	write("10000000-0000-4000-8000-000000000001", a)
	write("20000000-0000-4000-8000-000000000002", b)
	write("30000000-0000-4000-8000-000000000003", other)
	code, out, errOut := runTL(t, e, "--all", "--worktime", "--grep", "KEY-123", "--since", "2026-10-25T09:00:00Z")
	if code != 0 || errOut != "" || !strings.Contains(out, "total\t1h 30m") {
		t.Fatalf("code %d, stderr %q, output:\n%s", code, errOut, out)
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
	content := row(start, "KEY-123 start") + "\n" + row(start.Add(10*time.Minute), "KEY-123 activity") + "\n"
	path := filepath.Join(dir, "40000000-0000-4000-8000-000000000004.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runTL(t, e, "--all", "--worktime", "--grep", "KEY-123", "--since", "2026-04-05T00:00:00+02:00", "--until", "2026-04-07T00:00:00+02:00")
	if code != 0 || !strings.Contains(out, "2026-04-05\t15m") || !strings.Contains(out, "2026-04-06\t15m") {
		t.Fatalf("code %d, output:\n%s", code, out)
	}
}

func TestWorktimeKeyWindowStartsAtFirstMention(t *testing.T) {
	e, dir := workHome(t)
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, time.FixedZone("synthetic", 3600))
	rows := workRows(t, start, start.Add(5*time.Hour), 5*time.Minute, func(at time.Time) string {
		if at.Equal(start.Add(time.Hour)) {
			return workRecord(t, "user", at, "KEY-123", "human")
		}
		return workRecord(t, "agent", at, "other task", "")
	})
	writeWorkFile(t, workParentPath(dir), rows)
	code, out, stderr := runTL(t, e, "--all", "--worktime", "--grep", `\bKEY-123\b`)
	if code != 0 || stderr != "" || !strings.Contains(out, "2026-11-10\t4h\n") {
		t.Fatalf("code %d, stderr %q, output:\n%s", code, stderr, out)
	}
}

func TestWorktimeKeyRoles(t *testing.T) {
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.FixedZone("synthetic", 3600))
	t.Run("peer", func(t *testing.T) {
		e, dir := workHome(t)
		rows := []string{workRecord(t, "user", start, "KEY-123", "peer")}
		rows = append(rows, workRows(t, start.Add(5*time.Minute), start.Add(30*time.Minute), 5*time.Minute,
			func(at time.Time) string { return workRecord(t, "agent", at, "work", "") })...)
		writeWorkFile(t, workParentPath(dir), rows)
		code, out, stderr := runTL(t, e, "--all", "--worktime", "--grep", "KEY-123")
		if code != 0 || stderr != "" || !strings.Contains(out, "2026-11-10\t30m\n") {
			t.Fatalf("code %d, out %q, err %q", code, out, stderr)
		}
	})
	t.Run("prompt in subagent brief", func(t *testing.T) {
		e, dir := workHome(t)
		parent := workRows(t, start, start.Add(time.Hour), 5*time.Minute,
			func(at time.Time) string { return workRecord(t, "agent", at, "parent", "") })
		writeWorkFile(t, workParentPath(dir), parent)
		rows := []string{workRecord(t, "user", start.Add(10*time.Minute), "KEY-123 brief", "")}
		rows = append(rows, workRows(t, start.Add(15*time.Minute), start.Add(40*time.Minute), 5*time.Minute,
			func(at time.Time) string { return workRecord(t, "agent", at, "child work", "") })...)
		writeWorkFile(t, workChildPath(dir), rows)
		code, out, stderr := runTL(t, e, "--all", "--worktime", "--grep", "KEY-123")
		if code != 0 || stderr != "" || !strings.Contains(out, "2026-11-10\t30m\n") {
			t.Fatalf("code %d, out %q, err %q", code, out, stderr)
		}
	})
	t.Run("tool output is not a match", func(t *testing.T) {
		e, dir := workHome(t)
		rows := workRows(t, start.Add(-2*time.Hour), start.Add(2*time.Hour), 5*time.Minute,
			func(at time.Time) string { return workRecord(t, "agent", at, "unrelated", "") })
		rows = append(rows, workRecord(t, "cmd", start.Add(2*time.Hour+time.Minute), "ls files", ""),
			workRecord(t, "out", start.Add(2*time.Hour+2*time.Minute), "KEY-123.txt", ""))
		writeWorkFile(t, workParentPath(dir), rows)
		code, out, stderr := runTL(t, e, "--all", "--worktime", "--grep", "KEY-123")
		if code != exitNone || !strings.Contains(out, "no sessions with matching key") || stderr != "" {
			t.Fatalf("code %d, out %q, err %q", code, out, stderr)
		}
	})
}

func TestWorktimeParentWaitsForChild(t *testing.T) {
	e, dir := workHome(t)
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.FixedZone("synthetic", 3600))
	parent := []string{workRecord(t, "user", start, "KEY-123", "human"),
		workRecord(t, "cmd", start.Add(time.Minute), "Task", ""),
		workRecord(t, "out", start.Add(41*time.Minute), "done", ""),
		workRecord(t, "agent", start.Add(42*time.Minute), "finished", "")}
	writeWorkFile(t, workParentPath(dir), parent)
	child := workRows(t, start.Add(time.Minute), start.Add(55*time.Minute), 5*time.Minute,
		func(at time.Time) string { return workRecord(t, "agent", at, "child work", "") })
	writeWorkFile(t, workChildPath(dir), child)
	code, out, stderr := runTL(t, e, "--all", "--worktime", "--grep", "KEY-123")
	if code != 0 || stderr != "" || !strings.Contains(out, "2026-11-10\t45m\n") {
		t.Fatalf("code %d, output %q, stderr %q", code, out, stderr)
	}
}

func TestWorktimeKeyBeforeWindowStillCountsCurrentActivity(t *testing.T) {
	e, dir := workHome(t)
	prev := time.Date(2026, 11, 9, 22, 0, 0, 0, time.FixedZone("synthetic", 3600))
	today := time.Date(2026, 11, 10, 9, 0, 0, 0, time.FixedZone("synthetic", 3600))
	rows := []string{workRecord(t, "user", prev, "KEY-123", "human")}
	rows = append(rows, workRows(t, today, today.Add(2*time.Hour), 5*time.Minute,
		func(at time.Time) string { return workRecord(t, "agent", at, "continuing", "") })...)
	writeWorkFile(t, workParentPath(dir), rows)
	code, out, stderr := runTL(t, e, "--all", "--worktime", "--grep", `\bKEY-123\b`, "--since", "2026-11-10T10:00:00+01:00")
	if code != 0 || stderr != "" || !strings.Contains(out, "2026-11-10\t1h\n") {
		t.Fatalf("code %d, output %q, stderr %q", code, out, stderr)
	}
}

func TestWorktimeClipsActivityAtUntil(t *testing.T) {
	e, dir := workHome(t)
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, time.FixedZone("synthetic", 3600))
	rows := workRows(t, start, start.Add(2*time.Hour), 5*time.Minute, func(at time.Time) string {
		if at.Equal(start) {
			return workRecord(t, "user", at, "KEY-123", "human")
		}
		return workRecord(t, "agent", at, "work", "")
	})
	writeWorkFile(t, workParentPath(dir), rows)
	code, out, stderr := runTL(t, e, "--all", "--worktime", "--grep", "KEY-123", "--until", "2026-11-10T10:00:00+01:00")
	if code != 0 || stderr != "" || !strings.Contains(out, "2026-11-10\t1h\n") {
		t.Fatalf("code %d, output %q, stderr %q", code, out, stderr)
	}
}

func TestWorktimeDayRowsAreRoundedAndSummed(t *testing.T) {
	e, dir := workHome(t)
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, time.FixedZone("synthetic", 3600))
	rows := []string{workRecord(t, "user", start, "KEY-123", "human"),
		workRecord(t, "agent", start.Add(2*time.Hour), "work", ""),
		workRecord(t, "agent", start.Add(4*time.Hour), "work", ""),
		workRecord(t, "agent", start.Add(14*time.Hour+55*time.Minute), "work", ""),
		workRecord(t, "agent", start.Add(15*time.Hour+5*time.Minute), "work", "")}
	writeWorkFile(t, workParentPath(dir), rows)
	code, out, stderr := runTL(t, e, "--all", "--worktime", "--grep", "KEY-123")
	want := "2026-11-10\t15m\n2026-11-11\t15m\ntotal\t30m\n"
	if code != 0 || stderr != "" || out != want {
		t.Fatalf("code %d, output %q, stderr %q; want %q", code, out, stderr, want)
	}
}

func TestWorktimeMissingParentWarnsAndKeepsChild(t *testing.T) {
	e, dir := workHome(t)
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.FixedZone("synthetic", 3600))
	rows := workRows(t, start, start.Add(30*time.Minute), 5*time.Minute,
		func(at time.Time) string { return workRecord(t, "agent", at, "KEY-123 child", "") })
	writeWorkFile(t, workChildPath(dir), rows)
	code, out, stderr := runTL(t, e, "--all", "--worktime", "--grep", "KEY-123")
	if code != 0 || !strings.Contains(out, "2026-11-10\t30m\n") || !strings.Contains(stderr, "warning: parent transcript") || strings.Contains(out, "warning:") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, stderr)
	}
}

func TestWorktimeRejectsOtherOutputFlagsAndUnboundGap(t *testing.T) {
	now := time.Now()
	for _, args := range [][]string{
		{"--worktime", "--grep", "KEY"},
		{"--all", "--worktime"},
		{"--all", "--gap", "15m"},
		{"--all", "--worktime", "--grep", "KEY", "--gap", "bad"},
		{"--all", "--worktime", "--grep", "KEY", "--gap", "0s"},
		{"--all", "--worktime", "--grep", "KEY", "--raw", "x"},
		{"--all", "--worktime", "--grep", "KEY", "--max", "1"},
		{"--all", "--worktime", "--grep", "KEY", "--width", "1"},
		{"--all", "--worktime", "--grep", "KEY", "--role", "all"},
		{"--all", "--worktime", "--grep", "KEY", "-C", "0"},
		{"--all", "--worktime", "--grep", "KEY", "--usage"},
		{"--all", "--worktime", "--grep", "KEY", "--wait", "x"},
	} {
		if _, err := parseArgs(args, now); err == nil {
			t.Errorf("parseArgs(%q) accepted", args)
		}
	}
	if _, err := parseArgs([]string{"--all", "--worktime", "--grep", "KEY", "--gap", "15m"}, now); err != nil {
		t.Fatalf("explicit default gap rejected with --worktime: %v", err)
	}
}

func TestWorktimeComboErrorsAreSpecific(t *testing.T) {
	for _, tc := range []struct {
		opts options
		want string
	}{
		{options{worktime: true, all: true, grep: "KEY", usage: true}, "--worktime and --usage"},
		{options{worktime: true, all: true, grep: "KEY", waitProg: "cmd"}, "--worktime and --wait"},
	} {
		err := checkWorktimeCombos(tc.opts)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("checkWorktimeCombos(%+v) = %v, want error containing %q", tc.opts, err, tc.want)
		}
	}
}

func TestWorktimeGapParsingReportsInvalidDuration(t *testing.T) {
	var opts options
	err := setFlag(&opts, "--gap", "invalid", time.Now())
	if err == nil || !strings.Contains(err.Error(), "--gap:") {
		t.Fatalf("setFlag invalid gap = %v, want a parse error for --gap", err)
	}
}
