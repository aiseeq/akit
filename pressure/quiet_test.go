package pressure

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuietPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	if p, err := QuietPath(); err != nil || p != "/state/akit/quiet-until" {
		t.Errorf("QuietPath = %q, %v", p, err)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/u")
	if p, err := QuietPath(); err != nil || p != "/home/u/.local/state/akit/quiet-until" {
		t.Errorf("QuietPath без XDG = %q, %v", p, err)
	}
}

func TestQuietFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "akit", "quiet-until")
	if until, err := ReadQuiet(path); err != nil || !until.IsZero() {
		t.Errorf("нет файла = %v, %v; ждали нулевое время", until, err)
	}
	want := time.Date(2026, 10, 29, 14, 45, 0, 0, time.FixedZone("CET", 3600))
	if err := SetQuiet(path, QuietNote{Until: want}); err != nil {
		t.Fatal(err)
	}
	if until, err := ReadQuiet(path); err != nil || !until.Equal(want) {
		t.Errorf("ReadQuiet = %v, %v; ждали %v", until, err, want)
	}
	if data, _ := os.ReadFile(path); string(data) != "2026-10-29T14:45:00+01:00\n" {
		t.Errorf("файл = %q; ждали RFC3339 с поясом", data)
	}
	if err := ClearQuiet(path); err != nil {
		t.Fatal(err)
	}
	if err := ClearQuiet(path); err != nil {
		t.Errorf("снять снятый: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("после ClearQuiet файл есть: %v", err)
	}
	if err := os.WriteFile(path, []byte("14:45\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadQuiet(path); err == nil {
		t.Error("битый файл — ждали ошибку")
	}
}

func TestQuietNote(t *testing.T) {
	m, now := fakeProc(t, map[string]string{"stat": "cpu0 1\ncpu1 1\nprocs_running 1\n"})
	until := now.Add(time.Hour + 500*time.Millisecond) // доли секунды файл срока не хранит
	if err := SetQuiet(m.quiet, QuietNote{Until: until, By: "planner", Why: "call"}); err != nil {
		t.Fatal(err)
	}
	v, err := m.Check(context.Background(), DefaultLimits, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if v.Load.QuietBy != "planner" || v.Load.QuietWhy != "call" {
		t.Fatalf("пометка = %q, %q; ждали planner, call", v.Load.QuietBy, v.Load.QuietWhy)
	}
	if why := v.Why(); why != "quiet mode until "+quietClock(v.Load.QuietUntil, time.Now())+" (planner: call)" {
		t.Errorf("Why = %q", why)
	}
	// Срок переставил писатель без пометки: старая пометка к новому сроку не относится.
	if err := os.WriteFile(m.quiet, []byte(now.Add(2*time.Hour).Format(time.RFC3339)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, err = m.Check(context.Background(), DefaultLimits, "", true); err != nil || v.Load.QuietBy != "" || v.Load.QuietWhy != "" || v.Measure != Quiet {
		t.Errorf("чужой срок = %+v, %v; ждали тихий режим без пометки", v.Load, err)
	}
	for by, want := range map[[2]string]string{{"planner", ""}: " (planner)", {"", "call"}: " (call)", {"", ""}: ""} {
		if got := quietNoteText(by[0], by[1]); got != want {
			t.Errorf("quietNoteText(%q, %q) = %q; ждали %q", by[0], by[1], got, want)
		}
	}
	if err := SetQuiet(m.quiet, QuietNote{Until: until}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(QuietNotePath(m.quiet)); !os.IsNotExist(err) {
		t.Errorf("срок без пометки оставил старую: %v", err)
	}
	if err := SetQuiet(m.quiet, QuietNote{Until: until, Why: "call"}); err != nil {
		t.Fatal(err)
	}
	if err := ClearQuiet(m.quiet); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(QuietNotePath(m.quiet)); !os.IsNotExist(err) {
		t.Errorf("ClearQuiet оставил пометку: %v", err)
	}
	if err := SetQuiet(m.quiet, QuietNote{Until: until}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(QuietNotePath(m.quiet), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Check(context.Background(), DefaultLimits, "", true); err == nil {
		t.Error("битая пометка — ждали ошибку замера")
	}
}

func TestVerdictWithoutQuiet(t *testing.T) {
	quiet := Rule(Load{Runnable: 1, Threads: 2, QuietUntil: time.Now().Add(time.Hour), QuietBy: "planner"}, DefaultLimits, nil)
	if quiet.Measure != Quiet {
		t.Fatalf("Measure = %q; ждали quiet", quiet.Measure)
	}
	if v := quiet.WithoutQuiet(DefaultLimits); v.Busy() || !v.Load.QuietUntil.IsZero() || v.Load.QuietBy != "" {
		t.Errorf("без тишины, runnable 1 из 2 = %+v; ждали свободна", v)
	}
	busy := Rule(Load{Runnable: 2, Threads: 2, QuietUntil: time.Now().Add(time.Hour)}, DefaultLimits, nil)
	if v := busy.WithoutQuiet(DefaultLimits); v.Measure != CPU {
		t.Errorf("без тишины, runnable 2 из 2 = %q; ждали cpu", v.Measure)
	}
}

func TestCheckQuiet(t *testing.T) {
	m, now := fakeProc(t, map[string]string{"stat": "cpu0 1\ncpu1 1\nprocs_running 1\n"})
	until := now.Add(time.Hour)
	if err := SetQuiet(m.quiet, QuietNote{Until: until}); err != nil {
		t.Fatal(err)
	}
	v, err := m.Check(context.Background(), DefaultLimits, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Busy() || v.Measure != Quiet || !v.Load.QuietUntil.Equal(until) || v.Load.Threads != 2 {
		t.Errorf("тихий режим = %+v; ждали занята по quiet с замерами", v)
	}
	if why := v.Why(); why != "quiet mode until "+quietClock(until, time.Now()) {
		t.Errorf("Why = %q", why)
	}
	*now = until
	if v, err = m.Check(context.Background(), DefaultLimits, "", true); err != nil || v.Busy() || !v.Load.QuietUntil.IsZero() {
		t.Errorf("истёкший срок = %+v, %v; ждали свободна", v, err)
	}
	if err := os.WriteFile(m.quiet, []byte("soon\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Check(context.Background(), DefaultLimits, "", true); err == nil {
		t.Error("битый файл тихого режима — ждали ошибку замера")
	}
}

func TestQuietClock(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.Local)
	if s := quietClock(now.Add(2*time.Hour+45*time.Minute), now); s != "14:45" {
		t.Errorf("сегодня = %q", s)
	}
	if s := quietClock(now.Add(24*time.Hour), now); s != "2026-10-30 12:00" {
		t.Errorf("завтра = %q", s)
	}
}
