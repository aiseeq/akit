package main

import "testing"

func TestBriefQuestionInUnorderedTranscript(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "unordered-question",
		agent("2026-10-31T09:58:00Z", "Удалить ветку feature-x?"),
		agent("2026-10-31T09:59:00Z", "Жду ответа."),
		human("2026-10-31T10:00:00Z", "да"),
		agent("2026-10-31T09:50:00Z", "Поздняя запись с более ранним временем."))
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10 на вопрос «Удалить ветку feature-x?» ответил «да».\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if code != 0 {
		t.Fatalf("код %d:\n%s", code, out)
	}
	mustHave(t, "вопрос в неупорядоченном транскрипте", agentBlock(t, out), "Удалить ветку feature-x?")
}
