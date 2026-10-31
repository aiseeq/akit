package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// user-word --brief: сценарии, где проверка брифа могла бы выдать не то слово пользователя, на синтетике.

func writeBrief(t *testing.T, dir, text string) string {
	t.Helper()
	p := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func briefRun(t *testing.T, e env, args ...string) (int, string) {
	t.Helper()
	code, out, errOut := runUW(t, e, append([]string{"--brief"}, args...)...)
	return code, out + errOut
}

func mustHave(t *testing.T, name, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("%s: нет %q в выводе:\n%s", name, w, out)
		}
	}
}

func mustLack(t *testing.T, name, out string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(out, b) {
			t.Errorf("%s: лишнее %q в выводе:\n%s", name, b, out)
		}
	}
}

// №2: бриф ссылается на половину реплики — проверяющему нужна вся.
func TestAcceptBriefLinkWholeLine(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-x-proj", "aaaa1111-0000",
		agent("2026-10-31T09:59:00Z", "Что делать с упавшим тестом?"),
		human("2026-10-31T10:00:00Z", "Исправь тест, прод не трогай"))
	b := writeBrief(t, t.TempDir(), "# Задача\nСлово: `user-word "+p+" 'Исправь тест'`.\nИсправь тест и код прода.\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if code != 0 {
		t.Errorf("код %d, ждали 0:\n%s", code, out)
	}
	mustHave(t, "вся реплика", out, "Исправь тест, прод не трогай", "Что делать с упавшим тестом?", "brief: 1 user words: 1 found, 0 ambiguous, 0 not found")
}

// №4, №5: отмена через 20 минут и уточнение через 50 — до времени брифа, а не 30 минут; позже брифа — нет.
func TestAcceptBriefLaterLinesUntilBrief(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-x-proj", "bbbb2222-0000",
		human("2026-10-31T10:00:00Z", "Можешь удалить таблицу"),
		agent("2026-10-31T10:05:00Z", "Удаляю."),
		human("2026-10-31T10:20:00Z", "Отмена, таблицу не удаляй"),
		human("2026-10-31T10:50:00Z", "Только патч-версии"),
		human("2026-10-31T12:30:00Z", "Совсем другое дело"))
	b := writeBrief(t, t.TempDir(), "`user-word "+p+" 'удалить таблицу'` — удали таблицу.\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if code != 0 {
		t.Errorf("код %d:\n%s", code, out)
	}
	mustHave(t, "до брифа", out, "Можешь удалить таблицу", "Отмена, таблицу не удаляй", "Только патч-версии")
	mustLack(t, "после брифа", out, "Совсем другое дело")
}

// По умолчанию время брифа — mtime файла.
func TestAcceptBriefUntilDefaultsToMtime(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-x-proj", "cccc3333-0000",
		human("2026-10-31T10:00:00Z", "Можешь удалить таблицу"),
		human("2026-10-31T10:20:00Z", "Отмена, таблицу не удаляй"),
		human("2026-10-31T10:50:00Z", "Только патч-версии"))
	b := writeBrief(t, t.TempDir(), "`user-word "+p+" 'удалить таблицу'`\n")
	at := time.Date(2026, 10, 31, 10, 30, 0, 0, time.UTC)
	if err := os.Chtimes(b, at, at); err != nil {
		t.Fatal(err)
	}
	code, out := briefRun(t, e, b)
	if code != 0 {
		t.Errorf("код %d:\n%s", code, out)
	}
	mustHave(t, "mtime", out, "Отмена, таблицу не удаляй")
	mustLack(t, "mtime", out, "Только патч-версии")
}

// №3: цитата без ссылки ищется в день пометки; та же фраза в другой день — не она.
func TestAcceptBriefQuoteByMarkerDay(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-a", "dddd4444-0000", human("2026-10-30T09:00:00Z", "Удали старые логи, это стенд"))
	writeTranscript(t, e.home, "-home-x-b", "eeee5555-0000", human("2026-10-31T14:00:00Z", "Удали старые логи, кроме продакшена"))
	b := writeBrief(t, t.TempDir(), "Почисти логи (пользователь 31.10: «Удали старые логи»).\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 15:00")
	if code != 0 {
		t.Errorf("код %d:\n%s", code, out)
	}
	mustHave(t, "день пометки", out, "Удали старые логи, кроме продакшена", "1 found")
	mustLack(t, "другой день", out, "это стенд", "ambiguous:")
}

// Пометка со временем, поясом и вставкой до двоеточия.
func TestAcceptBriefQuoteMarkerWithTime(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-b", "ffff6666-0000", human("2026-10-31T14:19:00Z", "Удали старые логи, кроме продакшена"))
	b := writeBrief(t, t.TempDir(), "Правило (пользователь 31.10 14:19 CET через коллегу (tl ffff6666:12): «удали старые логи»).\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 15:00")
	if code != 0 {
		t.Errorf("код %d:\n%s", code, out)
	}
	mustHave(t, "время в пометке", out, "Удали старые логи, кроме продакшена")
}

// №3: две разные реплики в тот же день — обе с пометкой ambiguous, не первая молча.
func TestAcceptBriefQuoteAmbiguous(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-a", "abab7777-0000", human("2026-10-31T09:00:00Z", "Удали старые логи, это стенд"))
	writeTranscript(t, e.home, "-home-x-b", "cdcd8888-0000", human("2026-10-31T14:00:00Z", "Удали старые логи, кроме продакшена"))
	b := writeBrief(t, t.TempDir(), "(пользователь 31.10: «Удали старые логи»)\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 15:00")
	if code != 0 {
		t.Errorf("код %d, ждали 0 (неоднозначность — не «не найдено»):\n%s", code, out)
	}
	mustHave(t, "неоднозначно", out, "ambiguous:", "это стенд", "кроме продакшена", "0 found, 1 ambiguous, 0 not found")
}

// №6: «…» в цитате — куски по порядку в одной реплике; «…» в пути транскрипта — по префиксу id; ~ — домашняя папка.
func TestAcceptBriefEllipsis(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "abcd1234-5678",
		human("2026-10-31T10:00:00Z", "Исправь, пожалуйста, этот тест в пакете"),
		human("2026-10-31T10:05:00Z", "И README поправь"))
	b := writeBrief(t, t.TempDir(), "`user-word ~/.claude/projects/-home-x-proj/abcd1234-…jsonl 'README'`\n(пользователь 31.10: «Исправь… тест»)\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if code != 0 {
		t.Errorf("код %d:\n%s", code, out)
	}
	mustHave(t, "многоточие", out, "И README поправь", "Исправь, пожалуйста, этот тест в пакете", "2 user words: 2 found")
}

// №6: неточная цитата — «не найдено» и код 1; тяжесть решает проверка.
func TestAcceptBriefNotFound(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "beef0000-0000", human("2026-10-31T10:00:00Z", "Исправь, пожалуйста, тест"))
	b := writeBrief(t, t.TempDir(), "(пользователь 31.10: «Исправь тест»)\n`user-word "+filepath.Join(e.home, ".claude", "projects", "-home-x-proj", "beef0000-0000.jsonl")+" 'пожалуйста'`\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if code != exitNone {
		t.Errorf("код %d, ждали %d:\n%s", code, exitNone, out)
	}
	mustHave(t, "не найдено", out, "not found: ", "Исправь тест", "2 user words: 1 found, 0 ambiguous, 1 not found")
}

// Ссылка на транскрипт, которого нет, — «не найдено», не ошибка вызова.
func TestAcceptBriefMissingTranscript(t *testing.T) {
	e := testEnv(t)
	b := writeBrief(t, t.TempDir(), "`user-word /nonexistent/zzzz.jsonl 'что-то'`\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if code != exitNone {
		t.Errorf("код %d, ждали %d:\n%s", code, exitNone, out)
	}
	mustHave(t, "нет транскрипта", out, "not found: ", "0 found, 0 ambiguous, 1 not found")
}

// Бриф без слов пользователя: код 0 и пометка; слова коллеги и пересказ без даты — не слова пользователя.
func TestAcceptBriefNoUserWords(t *testing.T) {
	e := testEnv(t)
	b := writeBrief(t, t.TempDir(), "Сделай X (коллега 24.10: «сначала тесты»). Пользователь просил быстрее.\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if code != 0 {
		t.Errorf("код %d:\n%s", code, out)
	}
	mustHave(t, "без слов", out, "no user words")
}

// Нет файла брифа — ошибка вызова.
func TestAcceptBriefMissingFile(t *testing.T) {
	e := testEnv(t)
	code, out := briefRun(t, e, filepath.Join(t.TempDir(), "nope.md"))
	if code != exitUsage {
		t.Errorf("код %d, ждали %d:\n%s", code, exitUsage, out)
	}
}

func TestAcceptBriefArgs(t *testing.T) {
	if !strings.Contains(usage, "--brief") {
		t.Error("usage без --brief")
	}
	for _, args := range [][]string{{"--brief"}, {"--brief", "b.md", "--until"}, {"--brief", "b.md", "--until", "31.10 11:00"}, {"--brief", "b.md", "--nope", "1"}} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%q) — ждали ошибку", args)
		}
	}
}

// Образец вызова в тексте брифа (`user-word <jsonl> '<кусок>'`) — не ссылка: путь ссылки — абсолютный или от ~ и
// кончается на .jsonl.
func TestAcceptBriefPlaceholderNotLink(t *testing.T) {
	e := testEnv(t)
	b := writeBrief(t, t.TempDir(), "Формат: `user-word <jsonl> '<кусок>'` и `user-word --brief FILE`.\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if code != 0 {
		t.Errorf("код %d, ждали 0:\n%s", code, out)
	}
	mustHave(t, "образец", out, "no user words")
	mustLack(t, "образец", out, "not found:")
}

// Пометка с часами (и секундами) сужает поиск до минут вокруг них, а не до часа: короткое «да» в тот же час — другие ответы.
func TestAcceptBriefQuoteClockNarrows(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-a", "acac9999-0000", human("2026-10-31T15:07:51Z", "да"))
	writeTranscript(t, e.home, "-home-x-b", "bcbc9999-0000",
		human("2026-10-31T14:40:00Z", "да, но позже"),
		human("2026-10-31T15:30:00Z", "да, отменяю"))
	b := writeBrief(t, t.TempDir(), "Согласие (пользователя 31.10 15:07:51 CET: «да»).\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 17:00")
	if code != 0 {
		t.Errorf("код %d:\n%s", code, out)
	}
	mustHave(t, "минута пометки", out, "1 found, 0 ambiguous")
	mustLack(t, "другие минуты", out, "да, но позже", "да, отменяю", "ambiguous:")
}

// Два одинаковых ответа в разное время дня без часов в пометке — неоднозначность, а не одна находка.
func TestBriefSameTextTwiceIsAmbiguous(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-a", "a1a10000-0000", human("2026-10-31T10:00:00Z", "да"))
	writeTranscript(t, e.home, "-home-x-b", "b2b20000-0000", human("2026-10-31T16:00:00Z", "да"))
	b := writeBrief(t, t.TempDir(), "(пользователь 31.10: «да»)\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 17:00")
	mustHave(t, "два да", out, "ambiguous: 2 lines", "0 found, 1 ambiguous")
}

// Цитата другой пометки на той же строке — не слово пользователя; пометка без цитаты видна в итоге.
func TestBriefQuoteOfAnotherMarker(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-a", "c3c30000-0000", human("2026-10-31T10:00:00Z", "Ускорь сборку"))
	b := writeBrief(t, t.TempDir(), "Сделать (пользователь 31.10 просил ускорить; коллега 31.10: «Ускорь сборку»).\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 17:00")
	if code != exitNone {
		t.Errorf("код %d, ждали %d:\n%s", code, exitNone, out)
	}
	mustHave(t, "чужая цитата", out, "no «quote» after a colon", "0 found, 0 ambiguous, 1 not found")
}

// Пометка с прописной, без двоеточия, ссылка не по образцу, 29.02 невисокосного года, пустая цитата — не молчание.
func TestBriefMalformedWordsReported(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-a", "d4d40000-0000", human("2026-10-31T10:00:00Z", "Ускорь сборку"))
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10: «Ускорь сборку».\n"+
		"Пользователь 31.10 сказал «Ускорь».\n"+
		"`user-word ./x.jsonl 'кусок'`\n"+
		"(пользователь 29.02: «Ускорь»)\n"+
		"(пользователь 31.10: «…»)\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 17:00")
	if code != exitNone {
		t.Errorf("код %d, ждали %d:\n%s", code, exitNone, out)
	}
	mustHave(t, "всё в итоге", out, "5 user words: 1 found, 0 ambiguous, 4 not found",
		"no «quote» after a colon", "link not in the form", "no such day in 2026", "empty quote")
}

// Ссылка в двойных кавычках — ссылка; несколько реплик с куском — последняя, о прежних сказано.
func TestBriefLinkDoubleQuotesAndEarlierLines(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-x-a", "e5e50000-0000",
		human("2026-10-31T10:00:00Z", "Ускорь сборку"),
		human("2026-10-31T11:30:00Z", "Ускорь сборку ещё"))
	b := writeBrief(t, t.TempDir(), "`user-word "+p+" \"Ускорь сборку\"`\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 17:00")
	mustHave(t, "двойные кавычки", out, "1 found", "note: the last of 2 user lines", "Ускорь сборку ещё")
}

// Цитата ищется целыми словами: «да» не находит «Удали».
func TestBriefQuoteWholeWords(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-a", "f6f60000-0000", human("2026-10-31T10:00:00Z", "Удали логи"))
	b := writeBrief(t, t.TempDir(), "(пользователь 31.10: «да»)\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 17:00")
	if code != exitNone {
		t.Errorf("код %d, ждали %d:\n%s", code, exitNone, out)
	}
	mustHave(t, "целые слова", out, "1 not found")
}
