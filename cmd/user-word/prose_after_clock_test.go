package main

import (
	"strings"
	"testing"
)

// Prose right after the marker clock is text, not a zone: the line is searched
// in the default zone. Words that name a zone stay an error (see TestR6 and
// TestBriefMarkerZoneTextAndYear).
func TestBriefProseAfterMarkerClock(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-pc", "pc000000-0000",
		human("2026-10-31T10:00:00Z", "первая строка"),
		human("2026-10-31T10:00:30Z", "вторая строка"))
	for _, marker := range []string{
		"31.10 10:00 о DeepSeek",
		"31.10 10:00 на «другое»",
		"31.10 10:00:30 (через коллегу, транскрипт abc)",
		"31.10 10:00 про лимит",
		"31.10 10:00 via colleague",
		"31.10 10:00 about limits",
	} {
		quote := "первая строка"
		if strings.Contains(marker, "10:00:30") {
			quote = "вторая строка"
		}
		_, out := briefRun(t, e, writeBrief(t, t.TempDir(), "Пользователь "+marker+": «"+quote+"»\n"), "--until", "2026-10-31 11:00")
		mustHave(t, marker, out, quote, "1 found, 0 ambiguous, 0 not found")
		if strings.Contains(out, "zone") || strings.Contains(out, "not found:") {
			t.Errorf("%s: prose after the clock read as a zone or not found:\n%s", marker, out)
		}
	}
	for _, marker := range []string{"31.10 10:00 MSK", "31.10 10:00 EST", "31.10 10:00 мск", "31.10 10:00 (MSK)", "31.10 10:00 по Москве"} {
		_, out := briefRun(t, e, writeBrief(t, t.TempDir(), "Пользователь "+marker+": «первая строка»\n"), "--until", "2026-10-31 11:00")
		mustHave(t, marker, out, "zone", "1 not found")
	}
}

// A marker day without any transcript record says so instead of "no matching user line".
func TestBriefMarkerDayWithoutTranscripts(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-pc", "pc000001-0000", human("2026-10-31T10:00:00Z", "первая строка"))
	_, out := briefRun(t, e, writeBrief(t, t.TempDir(), "Пользователь 28.09: «давняя реплика»\n"), "--until", "2026-10-31 11:00")
	mustHave(t, "день без транскриптов", out, "1 not found", "no transcript")
	if strings.Contains(out, "no matching user line") {
		t.Errorf("day without transcripts reported as a missing line:\n%s", out)
	}
}
