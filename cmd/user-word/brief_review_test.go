package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// diffBrief runs --brief through files and through the fake database and reports both outputs.
func diffBrief(t *testing.T, e env, args ...string) {
	t.Helper()
	t.Setenv("TL_BACKEND", "files")
	codeF, outF, errF := runUW(t, e, args...)
	t.Setenv("TL_BACKEND", "")
	fakeCorpusDB(t, e)
	codeD, outD, errD := runUW(t, e, args...)
	if codeF != codeD || outF != outD {
		t.Errorf("DIFF files code %d / db code %d\n--- files:\n%s%s--- db:\n%s%s", codeF, codeD, outF, errF, outD, errD)
	}
}

func TestReviewShortWordAfterNewline(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "s1",
		agent("2026-10-31T09:58:00Z", "Собрать?"),
		human("2026-10-31T10:00:00Z", "ок\nда"))
	b := writeBrief(t, t.TempDir(), "Сборка (пользователь 31.10: «да»).\n")
	diffBrief(t, e, "--brief", b, "--until", "2026-10-31 11:00")
}

func TestReviewAppendedAfterLoad(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-x-proj", "s3",
		agent("2026-10-31T09:58:00Z", "Что с кэшем?"),
		human("2026-10-31T10:00:00Z", "оставь кэш сборки"))
	b := writeBrief(t, t.TempDir(), "Кэш (пользователь 31.10: «оставь кэш сборки»), ветка (пользователь 31.10: «удали ветку тоже»).\n")
	t.Setenv("TL_BACKEND", "")
	fakeCorpusDB(t, e) // database loaded before the tail below
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(human("2026-10-31T10:05:00Z", "удали ветку тоже") + "\n" + human("2026-10-31T10:06:00Z", "и собери релиз") + "\n")
	f.Close()
	codeD, outD, _ := runUW(t, e, "--brief", b, "--until", "2026-10-31 11:00")
	t.Setenv("TL_BACKEND", "files")
	codeF, outF, _ := runUW(t, e, "--brief", b, "--until", "2026-10-31 11:00")
	if codeF != codeD || outF != outD {
		t.Errorf("DIFF files code %d / db code %d\n--- files:\n%s--- db:\n%s", codeF, codeD, outF, outD)
	}
}

func TestReviewEventsLagBehindCompleteLines(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-x-proj", "lagging-events",
		human("2026-10-31T09:00:00Z", "оставь кэш старое"),
		human("2026-10-31T10:00:00Z", "оставь кэш сборки"))
	b := writeBrief(t, t.TempDir(), "Кэш (пользователь 31.10: «оставь кэш сборки»).\n")
	t.Setenv("TL_BACKEND", "")
	fakeCorpusDBAdjust(t, e, func(c *fakeCorpus) { c.evLines[p]-- })
	codeD, outD, _ := runUW(t, e, "--brief", b, "--until", "2026-10-31 11:00")
	t.Setenv("TL_BACKEND", "files")
	codeF, outF, _ := runUW(t, e, "--brief", b, "--until", "2026-10-31 11:00")
	if codeF != codeD || outF != outD {
		t.Errorf("DIFF files code %d / db code %d\n--- files:\n%s--- db:\n%s", codeF, codeD, outF, outD)
	}
}

func TestReviewBriefEventsPastEvLinesAreFiltered(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-x-proj", "stale-index",
		human("2026-10-31T10:00:00Z", "оставь кэш не сборки"))
	b := writeBrief(t, t.TempDir(), "Кэш (пользователь 31.10: «оставь кэш сборки»).\n")
	t.Setenv("TL_BACKEND", "")
	fakeCorpusDBAdjust(t, e, func(c *fakeCorpus) {
		id := c.events[0].fileID
		c.events = append(c.events, fakeEvent{fileID: id, path: p, line: 2, ts: c.events[0].ts.Add(time.Minute), role: "user", text: "оставь кэш сборки"})
	})
	codeD, outD, _ := runUW(t, e, "--brief", b, "--until", "2026-10-31 11:00")
	t.Setenv("TL_BACKEND", "files")
	codeF, outF, _ := runUW(t, e, "--brief", b, "--until", "2026-10-31 11:00")
	if codeF != codeD || outF != outD {
		t.Errorf("DIFF files code %d / db code %d\n--- files:\n%s--- db:\n%s", codeF, codeD, outF, outD)
	}
}

func TestReviewDeletedTranscript(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-x-proj", "s4",
		agent("2026-10-31T09:58:00Z", "Что с кэшем?"),
		human("2026-10-31T10:00:00Z", "оставь кэш сборки"))
	writeTranscript(t, e.home, "-home-x-proj", "s4b", human("2026-10-31T10:30:00Z", "что-то другое"))
	b := writeBrief(t, t.TempDir(), "Кэш (пользователь 31.10: «оставь кэш сборки»).\nПользователь 31.10 на вопрос «Почистить кэш?» ответил «оставь кэш сборки».\n")
	t.Setenv("TL_BACKEND", "")
	fakeCorpusDB(t, e)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	codeD, outD, errD := runUW(t, e, "--brief", b, "--until", "2026-10-31 11:00")
	t.Setenv("TL_BACKEND", "files")
	codeF, outF, _ := runUW(t, e, "--brief", b, "--until", "2026-10-31 11:00")
	if codeF != codeD || outF != outD {
		t.Errorf("DIFF files code %d / db code %d\n--- files:\n%s--- db:\n%s%s", codeF, codeD, outF, outD, errD)
	}
}

func TestReviewAskCallWithText(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "s5",
		agent("2026-10-31T09:50:00Z", "Ранний ответ агента."),
		`{"type":"assistant","timestamp":"2026-10-31T10:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"Текст перед вопросом"},{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[]}}]}}`,
		answer("2026-10-31T10:01:00Z", "q1", "Какой режим?", "Запусти мягкий режим"),
		human("2026-10-31T10:05:00Z", "и проверь логи"))
	b := writeBrief(t, t.TempDir(), "Режим (пользователь 31.10: «Запусти мягкий режим»).\n")
	diffBrief(t, e, "--brief", b, "--until", "2026-10-31 11:00")
}

func TestReviewAskLaterAndQuoteAsk(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "s6",
		agent("2026-10-31T09:58:00Z", "Что с кэшем?"),
		human("2026-10-31T10:00:00Z", "оставь кэш сборки"),
		ask("2026-10-31T10:02:00Z", "q1"),
		answer("2026-10-31T10:03:00Z", "q1", "Какой режим?", "Северный"),
		agent("2026-10-31T10:04:00Z", "Ок."),
		human("2026-10-31T10:05:00Z", "и проверь логи"))
	b := writeBrief(t, t.TempDir(), "Кэш (пользователь 31.10 10:00: «оставь кэш сборки»), режим (пользователь 31.10: «Северный»).\n")
	diffBrief(t, e, "--brief", b, "--until", "2026-10-31 11:00")
}

func TestReviewQueuedCommand(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "s7",
		agent("2026-10-31T09:58:00Z", "Работаю."),
		`{"type":"attachment","timestamp":"2026-10-31T10:00:00Z","attachment":{"type":"queued_command","prompt":"оставь кэш сборки","origin":{"kind":"human"}}}`,
		human("2026-10-31T10:05:00Z", "и проверь логи"))
	b := writeBrief(t, t.TempDir(), "Кэш (пользователь 31.10: «оставь кэш сборки»).\n")
	diffBrief(t, e, "--brief", b, "--until", "2026-10-31 11:00")
}

func TestReviewManyLater(t *testing.T) {
	e := testEnv(t)
	lines := []string{agent("2026-10-31T09:58:00Z", "Что с кэшем?"), human("2026-10-31T10:00:00Z", "оставь кэш сборки")}
	lines = append(lines, human("2026-10-31T10:00:00Z", "одновременно с цитатой"))
	for i := range 30 {
		ts := "2026-10-31T10:" + string(rune('1'+i/10)) + string(rune('0'+i%10)) + ":00Z"
		lines = append(lines, human(ts, "дальше "+strings.Repeat("x", i+1)))
	}
	lines = append(lines, human("2026-11-01T09:00:00Z", "на следующий день"), human("2026-11-01T13:00:00Z", "после границы until"))
	writeTranscript(t, e.home, "-home-x-proj", "s8", lines...)
	b := writeBrief(t, t.TempDir(), "Кэш (пользователь 31.10: «оставь кэш сборки»).\n")
	diffBrief(t, e, "--brief", b, "--until", "2026-11-01 12:00")
}

func TestReviewSubMillisecond(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "s9a",
		agent("2026-10-31T09:58:00Z", "Что с кэшем?"),
		human("2026-10-31T10:00:00.123456Z", "оставь кэш сборки"))
	writeTranscript(t, e.home, "-home-x-proj", "s9b",
		human("2026-10-31T10:00:00.123456Z", "оставь кэш сборки"), `{"type":"user","timestamp":`)
	b := writeBrief(t, t.TempDir(), "Кэш (пользователь 31.10: «оставь кэш сборки»).\n")
	diffBrief(t, e, "--brief", b, "--until", "2026-10-31 11:00")
}

func TestReviewUnorderedAgent(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "s10",
		human("2026-10-31T08:00:00Z", "начало"),
		agent("2026-10-30T20:00:00Z", "Запись с ранним временем после начала"),
		human("2026-10-31T10:00:00Z", "оставь кэш сборки"))
	b := writeBrief(t, t.TempDir(), "Кэш (пользователь 31.10: «оставь кэш сборки»).\n")
	diffBrief(t, e, "--brief", b, "--until", "2026-10-31 11:00")
}

func TestReviewCopyOneBad(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "c1",
		agent("2026-10-31T09:58:00.000Z", "Что с кэшем?"),
		human("2026-10-31T10:00:00.123Z", "оставь кэш сборки"))
	writeTranscript(t, e.home, "-home-x-proj", "c2",
		agent("2026-10-31T09:58:00.000Z", "Что с кэшем?"),
		human("2026-10-31T10:00:00.123Z", "оставь кэш сборки"), `{"type":"user","timestamp":`)
	b := writeBrief(t, t.TempDir(), "Кэш (пользователь 31.10: «оставь кэш сборки»).\n")
	diffBrief(t, e, "--brief", b, "--until", "2026-10-31 11:00")
}
