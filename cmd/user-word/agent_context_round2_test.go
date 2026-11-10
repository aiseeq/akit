package main

import (
	"strings"
	"testing"
)

// Two answers in one sentence: each word gets its own question.
func TestBriefSecondAnswerGetsItsOwnQuestion(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "a1a1b2b2-0000",
		agent("2026-10-31T09:58:00Z", "Удалить ветку feature-x?"),
		human("2026-10-31T09:59:00Z", "да"),
		agent("2026-10-31T10:00:00Z", "Перезапустить сервис?"),
		human("2026-10-31T10:01:00Z", "нет"))
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10 на вопрос «Удалить ветку feature-x?» ответил «да», а на вопрос «Перезапустить сервис?» ответил «нет».\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if code != 0 {
		t.Errorf("код %d:\n%s", code, out)
	}
	mustLack(t, "чужой вопрос", out, "not found in agent messages")
	second := out[strings.Index(out, "== 2."):]
	mustHave(t, "свой вопрос", agentBlock(t, second), "Перезапустить сервис?")
}

// A question quoted with an omission: the excerpt still holds its parts.
func TestBriefQuestionWithEllipsisShowsItsPlace(t *testing.T) {
	for _, quote := range []string{"Оставить … кэш сборки?", "Оставить […] кэш сборки?"} {
		e := testEnv(t)
		reply := "Начало.\n" + strings.Repeat("Длинный отчёт без вопросов, строка за строкой. ", 30) +
			"\nОставить общий кэш сборки? Жду решения.\n" + strings.Repeat("Хвост отчёта, ещё подробности замера. ", 60)
		writeTranscript(t, e.home, "-home-x-proj", "c3c3d4d4-0000",
			agent("2026-10-31T09:59:00Z", reply),
			human("2026-10-31T10:00:00Z", "да"))
		b := writeBrief(t, t.TempDir(), "Пользователь 31.10 на вопрос «"+quote+"» ответил «да».\n")
		code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
		if code != 0 {
			t.Errorf("%s: код %d:\n%s", quote, code, out)
		}
		mustLack(t, quote, out, "not found in agent messages")
		mustHave(t, quote, agentBlock(t, out), "Оставить общий кэш сборки?")
	}
}

// With the quoted question shown, questions cut off after the excerpt are listed too.
func TestBriefQuestionsCutAfterExcerpt(t *testing.T) {
	e := testEnv(t)
	reply := "Удалить ветку feature-x?\n" + strings.Repeat("Подробный отчёт без вопросов, длинный текст. ", 80) + "\nА ещё перезапустить сервис?"
	writeTranscript(t, e.home, "-home-x-proj", "e5e5f6f6-0000",
		agent("2026-10-31T09:59:00Z", reply),
		human("2026-10-31T10:00:00Z", "да"))
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10 на вопрос «Удалить ветку feature-x?» ответил «да».\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if code != 0 {
		t.Errorf("код %d:\n%s", code, out)
	}
	mustHave(t, "вопрос после куска", agentBlock(t, out), "Удалить ветку feature-x?", "А ещё перезапустить сервис?")
}
