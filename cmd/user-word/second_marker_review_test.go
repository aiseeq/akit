package main

import "testing"

// A date after «;» continues the user's list only inside the brackets opened by
// a user marker: another speaker's list and a plain list of dates stay out.
func TestBriefSemicolonDateOutsideUserBrackets(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-sr", "sr000000-0000",
		human("2026-11-01T23:34:00Z", "одни сутки от одного сна до другого"),
		human("2026-11-05T11:23:00Z", "2 блока. По блоку за день"))
	for _, tc := range []struct{ name, brief, want string }{
		{"another speaker's list", "Сроки (лид 01.11 10:00: «первое»; 05.11 11:23: «второе»).\n", "no user words in "},
		{"plain dates", "Прогоны 01.11; 05.11 проверено, 06.11; 07.11 — без замечаний.\n", "no user words in "},
		{"after the user's brackets close", "(пользователь 01.11 23:34: «одни сутки от одного сна до другого»). Дальше: лид; 05.11 11:23: «чужое».\n", "1 user words: 1 found, 0 ambiguous, 0 not found"},
		{"user list without brackets", "Пользователь 01.11 23:34: «одни сутки от одного сна до другого»; 05.11 11:23: «2 блока. По блоку за день».\n", "2 user words: 2 found, 0 ambiguous, 0 not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, out := briefRun(t, e, writeBrief(t, t.TempDir(), tc.brief), "--until", "2026-11-08 19:00")
			mustHave(t, tc.name, out, tc.want)
		})
	}
}

// A marker with only „…“ quotes says what it saw and what it expects.
func TestBriefLowQuotesCause(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-lc", "lc000000-0000", human("2026-11-09T15:01:00Z", "да"))
	_, out := briefRun(t, e, writeBrief(t, t.TempDir(), "Пользователь 09.11: „да“.\n"), "--until", "2026-11-10 10:00")
	mustHave(t, "„…“", out, "saw „…“ but expected «…»", "1 not found")
}
