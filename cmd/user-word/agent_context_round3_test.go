package main

import (
	"strings"
	"testing"
)

// A question with an omission whose first part also occurs earlier: the excerpt
// stands where all parts meet, not at the first occurrence of the first part.
func TestBriefQuestionPartsMeetForExcerpt(t *testing.T) {
	e := testEnv(t)
	reply := "Удалить файлы.\n" + strings.Repeat("Длинный отчёт без вопросов, строка за строкой. ", 100) +
		"\nУдалить старую ветку feature-x и почистить кэш сборки? Жду решения.\n" + strings.Repeat("Хвост отчёта. ", 40)
	writeTranscript(t, e.home, "-home-x-proj", "a7a7b8b8-0000",
		agent("2026-10-31T09:59:00Z", reply),
		human("2026-10-31T10:00:00Z", "да"))
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10 на вопрос «Удалить … кэш сборки?» ответил «да».\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if code != 0 {
		t.Errorf("код %d:\n%s", code, out)
	}
	mustLack(t, "вопрос", out, "not found in agent messages")
	mustHave(t, "кусок на вопросе", agentBlock(t, out), "почистить кэш сборки?")
}

// A question is compared as user quotes are: punctuation inside words does not
// matter ("feature x" finds "feature-x").
func TestBriefQuestionPunctuationAsQuotes(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "c9c9d0d0-0000",
		agent("2026-10-31T09:59:00Z", "Удалить ветку feature-x?"),
		human("2026-10-31T10:00:00Z", "да"))
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10 на вопрос «Удалить ветку feature x?» ответил «да».\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if code != 0 {
		t.Errorf("код %d:\n%s", code, out)
	}
	mustLack(t, "вопрос", out, "not found in agent messages")
	mustHave(t, "вопрос в блоке", agentBlock(t, out), "Удалить ветку feature-x?")
}

// An empty line in the transcript does not end the backward read: the question
// in an earlier agent message of the same turn is still found.
func TestBriefQuestionAcrossEmptyTranscriptLine(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "e1e1f2f2-0000",
		human("2026-10-31T09:50:00Z", "начинай"),
		agent("2026-10-31T09:58:00Z", "Удалить ветку feature-x?"),
		"",
		agent("2026-10-31T09:59:00Z", "Жду ответа."),
		human("2026-10-31T10:00:00Z", "да"))
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10 на вопрос «Удалить ветку feature-x?» ответил «да».\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if code != 0 {
		t.Errorf("код %d:\n%s", code, out)
	}
	mustLack(t, "вопрос", out, "not found in agent messages")
	mustHave(t, "вопрос в блоке", agentBlock(t, out), "Удалить ветку feature-x?")
}
