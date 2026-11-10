package main

import "testing"

// Records only on the days around the marker day do not count for it.
func TestBriefRecordDayBounds(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-pc", "bd000000-0000",
		human("2026-11-02T12:00:00Z", "накануне"),
		human("2026-11-04T12:00:00Z", "назавтра"))
	_, out := briefRun(t, e, writeBrief(t, t.TempDir(), "Пользователь 03.11: «нет такой»\n"), "--until", "2026-11-05 11:00")
	mustHave(t, "соседние дни", out, "no transcript records on marker day", "1 not found")
}
