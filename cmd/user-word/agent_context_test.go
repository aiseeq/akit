package main

import (
	"strings"
	"testing"
)

// A long agent reply with the question in its middle: the brief names the
// question, so the block shows it, not only the reply's end.
func TestBriefShowsQuotedQuestionInLongAgentReply(t *testing.T) {
	e := testEnv(t)
	reply := "Разбор окончен.\n" + strings.Repeat("Строка отчёта о проверке без вопросов. ", 20) +
		"\nУдалить старую ветку feature-x?\n" + strings.Repeat("Ещё подробности замера, длинный хвост. ", 80)
	writeTranscript(t, e.home, "-home-x-proj", "abab1212-0000",
		agent("2026-10-31T09:59:00Z", reply),
		human("2026-10-31T10:00:00Z", "да"))
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10 на вопрос «Удалить старую ветку feature-x?» ответил «да».\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if code != 0 {
		t.Errorf("код %d:\n%s", code, out)
	}
	mustHave(t, "вопрос из середины", agentBlock(t, out), "Удалить старую ветку feature-x?")
	mustHave(t, "найдено", out, "1 found")
}

// Two agent messages before the user's line: the brief quotes the question of
// the first one, so the block shows that message, not the later neighbour.
func TestBriefShowsAgentMessageWithQuotedQuestion(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "cdcd3434-0000",
		agent("2026-10-31T09:58:00Z", "Переносить репо в общее пространство?"),
		agent("2026-10-31T09:59:30Z", "Пока жду, проверяю статус сборки: зелёная."),
		human("2026-10-31T10:00:00Z", "да"))
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10 на вопрос «Переносить репо в общее пространство?» ответил «да».\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if code != 0 {
		t.Errorf("код %d:\n%s", code, out)
	}
	block := agentBlock(t, out)
	mustHave(t, "сообщение с вопросом", block, "Переносить репо в общее пространство?")
	mustLack(t, "соседнее сообщение", block, "проверяю статус сборки")
	mustHave(t, "найдено", out, "1 found")
}

// A plain answer without the quoted question: a long reply cut to its end still
// shows the question lines from the cut part.
func TestBriefShowsQuestionsCutFromLongAgentReply(t *testing.T) {
	e := testEnv(t)
	reply := "Итог.\nОставить кэш включённым?\n" + strings.Repeat("Подробный отчёт без вопросов, длинный текст. ", 80)
	writeTranscript(t, e.home, "-home-x-proj", "efef5656-0000",
		agent("2026-10-31T09:59:00Z", reply),
		human("2026-10-31T10:00:00Z", "да, оставь"))
	b := writeBrief(t, t.TempDir(), "Кэш (пользователь 31.10: «да, оставь»).\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if code != 0 {
		t.Errorf("код %d:\n%s", code, out)
	}
	mustHave(t, "вопрос из отрезанной части", agentBlock(t, out), "Оставить кэш включённым?")
	mustHave(t, "найдено", out, "1 found")
}

// agentBlock is the agent part of the context: from the first «--- agent» header
// to the user's line, without the brief line that quotes the question itself.
func agentBlock(t *testing.T, out string) string {
	t.Helper()
	start := strings.Index(out, "--- agent")
	end := strings.Index(out, "--- user ")
	if start < 0 || end < start {
		t.Fatalf("нет блока агента перед строкой пользователя:\n%s", out)
	}
	return out[start:end]
}
