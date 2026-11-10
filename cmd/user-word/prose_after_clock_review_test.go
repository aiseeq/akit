package main

import (
	"strings"
	"testing"
	"time"
)

// Upper-case prose after the clock is text unless it names a known zone; a
// parenthesized «по Москве» is a zone like the bare one.
func TestBriefUpperProseAndParenthesizedZone(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-pc", "pr000000-0000", human("2026-10-31T10:00:00Z", "первая строка"))
	for _, marker := range []string{"31.10 10:00 PR review", "31.10 10:00 API", "31.10 10:00 CI:"} {
		_, out := briefRun(t, e, writeBrief(t, t.TempDir(), "Пользователь "+marker+": «первая строка»\n"), "--until", "2026-10-31 11:00")
		mustHave(t, marker, out, "1 found, 0 ambiguous, 0 not found")
	}
	for _, marker := range []string{"31.10 10:00 (по Москве)", "31.10 10:00 MSK", "31.10 10:00 EST"} {
		_, out := briefRun(t, e, writeBrief(t, t.TempDir(), "Пользователь "+marker+": «первая строка»\n"), "--until", "2026-10-31 11:00")
		mustHave(t, marker, out, "zone", "1 not found")
	}
}

// Records out of chronological order still count for the marker day.
func TestBriefRecordDaysOutOfOrderTranscript(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-pc", "oo000000-0000",
		human("2026-11-03T12:00:00Z", "поздняя"),
		human("2026-11-03T09:00:00Z", "ранняя"),
		human("2026-11-03T11:00:00Z", "средняя"))
	_, out := briefRun(t, e, writeBrief(t, t.TempDir(), "Пользователь 03.11: «нет такой»\n"), "--until", "2026-11-04 11:00")
	mustHave(t, "непоследовательный транскрипт", out, "no matching user line", "1 not found")
	if strings.Contains(out, "no transcript") {
		t.Errorf("records of the day not seen:\n%s", out)
	}
}

// The marker day is taken in the marker zone: a UTC marker sees a record late in that UTC day.
func TestBriefRecordDayInMarkerZone(t *testing.T) {
	e := testEnv(t)
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	e.loc = loc
	writeTranscript(t, e.home, "-home-pc", "tz000000-0000", human("2026-10-30T23:30:00Z", "ночная"))
	_, out := briefRun(t, e, writeBrief(t, t.TempDir(), "Пользователь 30.10 UTC: «нет такой»\n"), "--until", "2026-11-01 11:00")
	mustHave(t, "день в поясе маркера", out, "1 not found")
	if strings.Contains(out, "no transcript") {
		t.Errorf("UTC day with a record reported as empty:\n%s", out)
	}
}
