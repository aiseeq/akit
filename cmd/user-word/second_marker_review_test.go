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

// Defects of the first version, each a false user word, a lost one or noise.
func TestBriefSecondMarkerReviewDefects(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-sd", "sd000000-0000",
		human("2026-11-01T23:34:00Z", "одни сутки от одного сна до другого"),
		human("2026-11-09T14:34:00Z", "чат"),
		agent("2026-11-09T15:00:00Z", "Записываю правки в 18 страниц к 14:00?"),
		human("2026-11-09T15:01:00Z", "да"),
		agent("2026-11-09T15:05:00Z", "Удаляю старые?"),
		human("2026-11-09T15:06:00Z", "нет"),
		human("2026-11-05T11:23:00Z", "чужое"))
	for _, tc := range []struct{ name, brief, want, lack string }{
		// D1: only «пользователь» and «пользователя» start a marker without a colon.
		{"dative is not a marker", "Агент отправил пользователю 09.11 «чат» на согласование.\n", "", "1 found"},
		{"instrumental is not a marker", "Пользователем 09.11 «чат» не упоминался.\n", "", "1 found"},
		// D2: a second question and answer in the same row is a second word.
		{"two answers in a row", "Пользователь 09.11 на вопрос «Записываю правки в 18 страниц к 14:00?» ответил «да», на вопрос «Удаляю старые?» ответил «нет».\n", "2 user words: 2 found, 0 ambiguous, 0 not found", ""},
		// D3: the clock in the quoted question is not the marker time.
		{"clock inside the question", "Пользователь 09.11 на вопрос «Записываю правки в 18 страниц к 14:00?»: «да».\n", "1 user words: 1 found, 0 ambiguous, 0 not found", ""},
		// D4: a speaker after the continuation date is not the user.
		{"speaker after the continuation date", "(пользователь 01.11 23:34: «одни сутки от одного сна до другого»; 05.11 лид: «чужое»)\n", "1 user words: 1 found, 0 ambiguous, 0 not found", ""},
		// D5: „…“ of another sentence are not this marker's.
		{"low quotes of another sentence", "Пользователь 09.11 просил. Лид 10.11: „x“.\n", "", "saw „…“"},
		// D6: a continuation date without a quote is just a date.
		{"continuation date without a quote", "(пользователь 01.11 23:34: «одни сутки от одного сна до другого»; 05.11 проверено на прогоне)\n", "1 user words: 1 found, 0 ambiguous, 0 not found", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, out := briefRun(t, e, writeBrief(t, t.TempDir(), tc.brief), "--until", "2026-11-10 10:00")
			if tc.want != "" {
				mustHave(t, tc.name, out, tc.want)
			}
			if tc.lack != "" {
				mustLack(t, tc.name, out, tc.lack)
			}
		})
	}
}

// Another speaker's answer after the user's one in the same sentence is not the user's.
func TestBriefSecondAnswerOfAnotherSpeaker(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-sa", "sa000000-0000",
		agent("2026-11-09T15:00:00Z", "Записываю правки?"),
		human("2026-11-09T15:01:00Z", "да"),
		human("2026-11-09T15:06:00Z", "нет"))
	for _, tc := range []struct{ brief, want string }{
		{"Пользователь 09.11 на вопрос «Записываю правки?» ответил «да», лид на вопрос «Удаляю?» ответил «нет».\n", "1 user words: 1 found"},
		{"Пользователь 09.11 на вопрос «Записываю правки?» ответил «да», а на вопрос «Удаляю?» ответил «нет».\n", "2 user words: 2 found"},
	} {
		_, out := briefRun(t, e, writeBrief(t, t.TempDir(), tc.brief), "--until", "2026-11-10 10:00")
		mustHave(t, tc.brief, out, tc.want)
	}
}
