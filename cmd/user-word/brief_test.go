package main

import (
	"fmt"
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
	writeTranscript(t, e.home, "-home-x-b", "ffff6666-0000", human("2026-10-31T13:19:00Z", "Удали старые логи, кроме продакшена"))
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
	writeTranscript(t, e.home, "-home-x-a", "acac9999-0000", human("2026-10-31T14:07:51Z", "да"))
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

func TestR3QuoteFirstAfterColonAndParagraphContinuation(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-r3", "10000000-0000", human("2026-10-31T10:00:00Z", "Ускорь сборку"))
	b := writeBrief(t, t.TempDir(), "пользователь 31.10 про «сборку»:\n«Ускорь сборку»\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 17:00")
	mustHave(t, "первая цитата после двоеточия и перенос", out, "1 found", "Ускорь сборку")
}

func TestR3ClockColonAndContextQuote(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-r3", "11000000-0000", human("2026-10-22T13:43:00Z", "Проверил, вопроса нет"))
	b := writeBrief(t, t.TempDir(), "Пользователь 22.10 ~15:43 CEST о вопросе «включать ли кэш»: «Проверил, вопроса нет»\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 17:00")
	mustHave(t, "двоеточие времени не считается разделителем пометки", out, "1 found", "Проверил, вопроса нет")
}

func TestR3SeveralQuotesAndSquareEllipsis(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-r3", "20000000-0000",
		human("2026-10-31T10:00:00Z", "Сделай быструю сборку проекта"),
		human("2026-10-31T10:05:00Z", "Исправь документацию"),
		human("2026-10-31T10:10:00Z", "Ускорь компиляцию"))
	b := writeBrief(t, t.TempDir(), "пользователь 31.10: «Сделай […] проекта», «Исправь документацию», «Ускорь компиляцию».\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 17:00")
	mustHave(t, "три цитаты", out, "3 user words: 3 found, 0 ambiguous, 0 not found")
}

func TestR3BracketedQuoteAnnotationIsOmission(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-r3", "21000000-0000", human("2026-10-31T10:00:00Z", "Посчитай оба способа"))
	b := writeBrief(t, t.TempDir(), "пользователь 31.10: «Посчитай оба способа [числа в отчётах]»\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 17:00")
	mustHave(t, "пояснение в скобках пропускается внутри цитаты", out, "1 found", "Посчитай оба способа")
}

func TestR3QuotePunctuationMayVary(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-r3", "22000000-0000", human("2026-10-31T10:00:00Z", "Проверьте вопроса-то нет"))
	b := writeBrief(t, t.TempDir(), "пользователь 31.10: «вопроса то нет»\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 17:00")
	mustHave(t, "дефис в реплике соответствует пробелу в цитате", out, "1 found", "вопроса то нет")
}

func TestR3QueuedUserAttachmentIsScanned(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-r3", "23000000-0000",
		`{"type":"attachment","timestamp":"2026-10-31T10:00:00Z","attachment":{"type":"queued_command","origin":{"kind":"human"},"prompt":"Проверь расчёт"}}`)
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10: «Проверь расчёт»\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 17:00")
	mustHave(t, "реплика пользователя из очереди", out, "1 found", "Проверь расчёт")
}

func TestR3MarkerFormsAndApproximateClock(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-r3", "30000000-0000", human("2026-11-01T15:55:00Z", "Собери проект"))
	b := writeBrief(t, t.TempDir(), "ПОЛЬЗОВАТЕЛЯ, 1.11 ~16:00 UTC: «Собери проект».\n")
	_, out := briefRun(t, e, b, "--until", "2026-11-01 17:00")
	mustHave(t, "форма пометки и окно около 15 минут", out, "1 found", "Собери проект")
}

func TestR3ExactClockWindowAndZones(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-r3", "40000000-0000",
		human("2026-10-31T14:00:00Z", "Точное окно"),
		human("2026-10-31T14:03:00Z", "Точное окно"),
		human("2026-10-31T13:00:00Z", "CEST время"),
		human("2026-10-31T15:00:00Z", "UTC время"))
	b := writeBrief(t, t.TempDir(),
		"Пользователь 31.10 15:00 CET: «Точное окно».\n"+
			"Пользователь 31.10 15:00 CEST: «CEST время».\n"+
			"Пользователь 31.10 15:00 UTC: «UTC время».\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 17:00")
	mustHave(t, "точное окно и пояса", out, "3 user words: 3 found, 0 ambiguous, 0 not found", "Точное окно", "CEST время", "UTC время")
	mustLack(t, "за пределом точного окна", out, "ambiguous:")
}

func TestR3ExactClockWindowIncludesTwoMinuteEdges(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-r3", "41000000-0000",
		human("2026-10-31T13:58:00Z", "Край окна"),
		human("2026-10-31T14:02:00Z", "Край окна"),
		human("2026-10-31T14:02:01Z", "Край окна"))
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10 15:00 CET: «Край окна»\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 17:00")
	mustHave(t, "точное окно включает обе границы ±2 минуты", out, "1 user words: 0 found, 1 ambiguous, 0 not found", "13:58", "14:02")
	// The matching window excludes 14:02:01 as a quote hit; later context now continues to --until.
}

func TestR3LinkUntilAndTranscriptMtime(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-r3", "50000000-0000",
		human("2026-10-31T10:00:00Z", "Ссылка есть"),
		human("2026-10-31T12:00:00Z", "Позже брифа"))
	b := writeBrief(t, t.TempDir(), "`user-word "+p+" 'Ссылка есть'`\n")
	until := time.Date(2026, 10, 31, 11, 0, 0, 0, time.UTC)
	if err := os.Chtimes(p, until.Add(-time.Hour), until.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	mustHave(t, "реплика после брифа отсечена", out, "1 found")
	if err := os.Chtimes(p, until.Add(time.Hour), until.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, out = briefRun(t, e, b, "--until", "2026-10-31 11:00")
	mustHave(t, "ранняя реплика в изменённом файле", out, "1 found")
}

func TestR3ChangedTranscriptDoesNotConfirmLaterReply(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-r3", "51000000-0000",
		human("2026-10-31T12:00:00Z", "Фрагмент только после брифа"))
	b := writeBrief(t, t.TempDir(), "`user-word "+p+" 'Фрагмент только после брифа'`\n")
	until := time.Date(2026, 10, 31, 11, 0, 0, 0, time.UTC)
	if err := os.Chtimes(p, until.Add(time.Hour), until.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	mustHave(t, "поздняя реплика в изменённом транскрипте", out, "1 not found", "fragment not found in transcript")
}

func TestBriefOtherBracketTextIsLiteral(t *testing.T) {
	if hasFragment("the feature", "the [hidden] feature") {
		t.Fatal("ordinary bracket text must not act as an omission")
	}
}

func TestR3ShellEscapedApostrophe(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-r3", "60000000-0000", human("2026-10-31T10:00:00Z", "don't stop"))
	b := writeBrief(t, t.TempDir(), "`user-word "+p+" 'don'\\''t stop'`\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	mustHave(t, "shell escaped apostrophe", out, "1 found", "don't stop")
}

func TestR4QuoteShowsLinkedTranscriptContext(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-r4", "70000000-0000",
		agent("2026-10-31T09:59:00Z", "Предложу вариант исправления."),
		human("2026-10-31T10:00:00Z", "Ускорь сборку проекта"),
		human("2026-10-31T10:01:00Z", "И проверь документацию"))
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10: «Ускорь сборку»\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	mustHave(t, "цитата показывает контекст найденной реплики", out, "Предложу вариант исправления", "Ускорь сборку проекта", "И проверь документацию")
}

func TestR4QuoteFindsAnswerInQuestionDialog(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-r4", "71000000-0000",
		ask("2026-10-31T10:00:00Z", "question-1"),
		answer("2026-10-31T10:01:00Z", "question-1", "Какой режим?", "Запусти мягкий режим"))
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10: «Запусти мягкий режим»\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	mustHave(t, "ответ в окне вопроса считается словами пользователя", out, "1 found", "Запусти мягкий режим")
}

func TestR4MarkerZoneControlsDayAndWindowAcrossMidnight(t *testing.T) {
	t.Run("UTC marker day in local zone", func(t *testing.T) {
		e := testEnv(t)
		e.loc = time.FixedZone("CET", 3600)
		writeTranscript(t, e.home, "-home-r4", "72000000-0000", human("2026-10-30T23:30:00Z", "Ночная дата"))
		b := writeBrief(t, t.TempDir(), "Пользователь 30.10 23:30 UTC: «Ночная дата»\n")
		_, out := briefRun(t, e, b, "--until", "2026-10-31 02:00")
		mustHave(t, "день определяется в поясе пометки", out, "1 found", "Ночная дата")
	})
	t.Run("timed window crosses midnight", func(t *testing.T) {
		e := testEnv(t)
		writeTranscript(t, e.home, "-home-r4", "73000000-0000", human("2026-10-30T22:59:00Z", "На границе полуночи"))
		b := writeBrief(t, t.TempDir(), "Пользователь 31.10 00:01 CET: «На границе полуночи»\n")
		_, out := briefRun(t, e, b, "--until", "2026-10-31 02:00")
		mustHave(t, "окно с временем и поясом пересекает полночь", out, "1 found", "На границе полуночи")
	})
}

func TestR4EmptyLinkFragmentIsNotFound(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-r4", "74000000-0000", human("2026-10-31T10:00:00Z", "Любая реплика"))
	b := writeBrief(t, t.TempDir(), "`user-word "+p+" ''`\n")
	code, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if code != exitNone {
		t.Errorf("пустая ссылка должна быть not found, код %d:\n%s", code, out)
	}
	mustHave(t, "пустой фрагмент ссылки", out, "1 not found")
}

func TestR4MalformedTranscriptLineIsReported(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-r4", "75000000-0000",
		human("2026-10-31T10:00:00Z", "Полезный фрагмент"), "{broken json")
	b := writeBrief(t, t.TempDir(), "`user-word "+p+" 'Полезный фрагмент'`\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	mustHave(t, "повреждённая строка транскрипта замечена", out, "malformed", "line 2")
}

func TestR4BracketedTranscriptTextIsLiteral(t *testing.T) {
	got := briefTextFragment("сделай X [пояснение] Y")
	if strings.Contains(got, "…") || !strings.Contains(got, "пояснение") {
		t.Fatalf("bracketed transcript words must remain literal, got %q", got)
	}
}

func TestR5QuoteContextContinuesUntilBriefTime(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-r5", "76000000-0000",
		human("2026-10-31T10:00:00Z", "Начальная цитата"),
		human("2026-10-31T10:05:00Z", "Продолжение после окна пометки"))
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10 10:00 UTC: «Начальная цитата»\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	mustHave(t, "контекст цитаты продолжается до времени брифа", out, "1 found", "Продолжение после окна пометки")
}

func TestR5PrefilterMatchesCaseFoldedQuote(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-r5", "77000000-0000", human("2026-10-31T10:00:00Z", "Залей на GitHub"))
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10: «залей на github»\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	mustHave(t, "отсев соблюдает сравнение без регистра", out, "1 found", "Залей на GitHub")
}

func TestR5SpacedJSONSyntaxFallsBackToFullParse(t *testing.T) {
	e := testEnv(t)
	raw := strings.ReplaceAll(human("2026-10-31T10:00:00Z", "Найди запись с пробелами"), `":"`, `": "`)
	writeTranscript(t, e.home, "-home-r5", "78000000-0000", raw)
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10: «Найди запись с пробелами»\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	mustHave(t, "JSON с пробелами разбирается", out, "1 found", "Найди запись с пробелами")
}

func TestR5QuoteSearchReportsMalformedTranscriptLines(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-r5", "79000000-0000",
		human("2026-10-31T10:00:00Z", "Цитата перед повреждённой строкой"), "{broken json")
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10: «Цитата перед повреждённой строкой»\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	mustHave(t, "путь поиска цитаты сообщает о повреждённой строке", out, "malformed", "line 2")
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

// Пометка со временем: отказ называет окно, а не весь день — в тот день такие слова могли быть.
func TestBriefTimedQuoteNotFoundNamesWindow(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-a", "w1a10000-0000", human("2026-10-31T15:00:00Z", "да"))
	_, out := briefRun(t, e, writeBrief(t, t.TempDir(), "Ок (пользователь 31.10 10:00 UTC: «да»).\n"), "--until", "2026-10-31 17:00")
	mustHave(t, "окно", out, "no matching user line within 2m0s of marker time", "1 not found")
}

func TestR6MarkerZonesCaseAndUnknownZone(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-r6", "zone0000-0000",
		human("2026-10-31T10:00:00Z", "UTC нижний регистр"),
		human("2026-10-31T09:00:00Z", "Местное время CET"),
	)
	b := writeBrief(t, t.TempDir(),
		"Пользователь 31.10 10:00 utc: «UTC нижний регистр»\n"+
			"Пользователь 31.10 10:00 MSK: «Неизвестный пояс»\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	mustHave(t, "utc без регистра найден в UTC", out, "1 found", "UTC нижний регистр")
	mustHave(t, "неизвестный пояс отвергнут с причиной", out, "zone MSK", "1 not found")
	if strings.Contains(out, "Местное время CET") {
		t.Errorf("MSK ошибочно трактован как местное время:\n%s", out)
	}
}

// Текст после часов пометки — только известный пояс: остальное не местное время, а not found с причиной; год
// в дате пометки учитывается.
func TestBriefMarkerZoneTextAndYear(t *testing.T) {
	e := testEnv(t)
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	e.loc = loc
	writeTranscript(t, e.home, "-home-x-a", "zt000000-0000",
		human("2026-10-20T08:00:00Z", "первая строка"),
		human("2026-10-31T10:12:00Z", "вторая строка"))
	for _, c := range []struct {
		marker, quote, want string
	}{
		{"20.10 10:00 мск", "первая строка", "zone мск"},
		{"20.10 11:00 мск", "первая строка", "zone мск"},
		{"20.10 10:00 (MSK)", "первая строка", "zone (MSK)"},
		{"20.10 10:00 по Москве", "первая строка", "zone по "},
		{"20.10 10:00, через коллегу", "первая строка", "1 found"},
		{"20.10 08:00 UTC+3", "первая строка", "1 not found"},
		{"20.10 11:00 UTC+3", "первая строка", "1 found"},
		{"20.10 10:00 GMT+2", "первая строка", "1 found"},
		{"20.10 06:00 utc-2", "первая строка", "1 found"},
		{"20.10 09:00 (CET)", "первая строка", "1 found"},
		{"31.10 10:12Z", "вторая строка", "1 found"},
		{"31.10 11:12Z", "вторая строка", "1 not found"},
		{"31.10.2025", "вторая строка", "1 not found"},
		{"31.10.2026", "вторая строка", "1 found"},
	} {
		_, out := briefRun(t, e, writeBrief(t, t.TempDir(), "Итог (пользователь "+c.marker+": «"+c.quote+"»).\n"), "--until", "2026-10-31 17:00")
		mustHave(t, c.marker, out, c.want)
		if strings.Contains(c.want, "zone") {
			mustHave(t, c.marker, out, "1 not found")
		}
	}
}

func TestRound7LaterUserLinesAfterNotifications(t *testing.T) {
	e := testEnv(t)
	records := []string{human("2026-10-31T10:00:00Z", "Исходная цитата")}
	for i := range 25 {
		records = append(records, fmt.Sprintf(`{"type":"attachment","timestamp":"2026-10-31T10:%02d:00Z","attachment":{"type":"hook_success","stdout":"уведомление %d"}}`, i+1, i))
	}
	records = append(records, human("2026-10-31T10:30:00Z", "Поздняя реплика после уведомлений"))
	writeTranscript(t, e.home, "-home-r7", "later000-0000", records...)
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10: «Исходная цитата»\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	mustHave(t, "поздняя реплика после уведомлений", out, "Поздняя реплика после уведомлений")
}

func TestRound7AgentBeforeSecondQuote(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-r7", "agents00-0000",
		agent("2026-10-31T09:59:00Z", "Ответ агента перед первой цитатой"),
		human("2026-10-31T10:00:00Z", "Первая цитата в разговоре"),
		agent("2026-10-31T10:09:00Z", "Ответ агента перед второй цитатой"),
		human("2026-10-31T10:10:00Z", "Вторая цитата в разговоре"))
	b := writeBrief(t, t.TempDir(),
		"Пользователь 31.10 10:00 UTC: «Первая цитата в разговоре»\n"+
			"Пользователь 31.10 10:10 UTC: «Вторая цитата в разговоре»\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	if !strings.Contains(out, "Ответ агента перед первой цитатой") || !strings.Contains(out, "Ответ агента перед второй цитатой") {
		t.Errorf("перед каждой цитатой нужен свой ответ агента:\n%s", out)
	}
	if strings.Count(out, "Ответ агента перед первой цитатой") != 1 {
		t.Errorf("ответ первой цитаты повторён для второй:\n%s", out)
	}
}

func TestRound7MarkerZoneForms(t *testing.T) {
	e := testEnv(t)
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	e.loc = loc
	writeTranscript(t, e.home, "-home-r7", "zones000-0000",
		human("2026-10-31T00:30:00Z", "Зона перед часами в UTC"),
		human("2026-10-31T09:00:00Z", "UTC в скобках перед часами"),
		human("2026-10-31T08:00:00Z", "Пунктуация после CET"),
		human("2026-10-31T08:00:00Z", "Пунктуация после скобок CET"),
		human("2026-10-31T08:00:00Z", "Пунктуация после UTC offset"),
		human("2026-10-30T14:00:00Z", "Дата без часов в UTC"),
		human("2025-10-31T08:00:00Z", "Двузначный год"))
	for _, c := range []struct{ marker, quote string }{
		{"31.10 UTC 00:30", "Зона перед часами в UTC"},
		{"31.10 (UTC) 09:00", "UTC в скобках перед часами"},
		{"31.10 09:00 CET,", "Пунктуация после CET"},
		{"31.10 (CET), 09:00", "Пунктуация после скобок CET"},
		{"31.10 09:00 UTC+1,", "Пунктуация после UTC offset"},
		{"30.10 UTC", "Дата без часов в UTC"},
		{"31.10.25", "Двузначный год"},
	} {
		_, out := briefRun(t, e, writeBrief(t, t.TempDir(), "Пользователь "+c.marker+": «"+c.quote+"»\n"), "--until", "2026-10-31 17:00")
		mustHave(t, c.marker, out, "1 found", c.quote)
	}
}
