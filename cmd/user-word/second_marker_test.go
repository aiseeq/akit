package main

import (
	"strings"
	"testing"
)

// A marker that continues the list after «;» in the same brackets (a date and
// time with no speaker word) is the user's word too: it is found or reported,
// never dropped from the summary.
func TestBriefSecondMarkerInBrackets(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-sm", "sm000000-0000",
		human("2026-11-01T23:34:00Z", "одни сутки от одного сна до другого"),
		human("2026-11-05T11:23:00Z", "2 блока. По блоку за день"))

	t.Run("both quotes are found", func(t *testing.T) {
		brief := "1. По блоку на день (пользователь 01.11 23:34: «одни сутки от одного сна до другого»; 05.11 11:23 о пропущенной ночи: «2 блока. По блоку за день»).\n"
		_, out := briefRun(t, e, writeBrief(t, t.TempDir(), brief), "--until", "2026-11-08 19:00")
		mustHave(t, "два маркера", out, "2 user words: 2 found, 0 ambiguous, 0 not found")
	})
	t.Run("a second quote without a line is not found, not dropped", func(t *testing.T) {
		brief := "1. По блоку на день (пользователь 01.11 23:34: «одни сутки от одного сна до другого»; 05.11 11:23: «такой реплики нет»).\n"
		_, out := briefRun(t, e, writeBrief(t, t.TempDir(), brief), "--until", "2026-11-08 19:00")
		mustHave(t, "второй без реплики", out, "2 user words: 1 found, 0 ambiguous, 1 not found")
	})
	t.Run("a named speaker after «;» is not the user", func(t *testing.T) {
		brief := "1. Сроки (пользователь 01.11 23:34: «одни сутки от одного сна до другого»; лид 05.11 11:23: «чужие слова»).\n"
		_, out := briefRun(t, e, writeBrief(t, t.TempDir(), brief), "--until", "2026-11-08 19:00")
		mustHave(t, "чужой маркер", out, "1 user words: 1 found, 0 ambiguous, 0 not found")
		mustLack(t, "чужой маркер", out, "чужие слова")
	})
}

// A user marker followed by the quote without a colon is still a marker.
func TestBriefMarkerWithoutColon(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-nc", "nc000000-0000",
		human("2026-11-09T14:34:00Z", "чат"),
		agent("2026-11-09T15:00:00Z", "Записываю правки в 18 страниц?"),
		human("2026-11-09T15:01:00Z", "да"))
	for _, brief := range []string{
		"По слову пользователя 09.11 14:34 «чат» — канал.\n",
		"Пользователь 09.11 14:34 «чат».\n",
	} {
		_, out := briefRun(t, e, writeBrief(t, t.TempDir(), brief), "--until", "2026-11-10 10:00")
		mustHave(t, brief, out, "1 user words: 1 found, 0 ambiguous, 0 not found")
	}
	t.Run("answer to a quoted question", func(t *testing.T) {
		brief := "Пользователь 09.11 на вопрос «Записываю правки в 18 страниц?» ответил «да».\n"
		_, out := briefRun(t, e, writeBrief(t, t.TempDir(), brief), "--until", "2026-11-10 10:00")
		mustHave(t, "ответ на вопрос", out, "1 user words: 1 found", "Записываю правки в 18 страниц?")
	})
}

// Quotes in „…“ are not read as user words; when a marker has only such quotes,
// the not found line names both: „…“ seen, «…» expected.
func TestBriefLowQuotesSayExpected(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-lq", "lq000000-0000", human("2026-11-09T15:01:00Z", "да"))
	brief := "Пользователь 09.11 на вопрос „Записываю правки?“ ответил „да“.\n"
	_, out := briefRun(t, e, writeBrief(t, t.TempDir(), brief), "--until", "2026-11-10 10:00")
	if !strings.Contains(out, "1 not found") || !strings.Contains(out, "„") || !strings.Contains(out, "«") {
		t.Errorf("no not found line naming «…»:\n%s", out)
	}
}
