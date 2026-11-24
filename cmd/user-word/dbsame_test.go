package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aiseeq/akit/transcript"
)

// sameThroughDatabase — команда через файлы и через базу (фейк из тех же транскриптов) даёт тот же stdout и код,
// и ответ через базу дала база: в stderr строка свежести и нет отката на файлы. Возвращает stdout через файлы.
func sameThroughDatabase(t *testing.T, e env, args ...string) string {
	t.Helper()
	t.Setenv("TL_BACKEND", "files")
	codeFiles, outFiles, _ := runUW(t, e, args...)
	fakeCorpusDB(t, e)
	codeDB, outDB, errDB := runUW(t, e, args...)
	if codeDB != codeFiles || outDB != outFiles {
		t.Errorf("%q: files code %d, database code %d\n--- files:\n%s--- database:\n%s", args, codeFiles, codeDB, outFiles, outDB)
	}
	if !strings.Contains(errDB, "данные актуальны на") || strings.Contains(errDB, "reading files instead") {
		t.Errorf("%q: the database did not answer: %q", args, errDB)
	}
	return outFiles
}

// humanWithImage — реплика человека с картинкой: текст и одна картинка в content.
func humanWithImage(ts, text string) string {
	return fmt.Sprintf(`{"type":"user","origin":{"kind":"human"},"timestamp":"%s","message":{"role":"user","content":[{"type":"text","text":%q},`+
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}}`, ts, text)
}

// Транскрипт, где записи идут не по времени (поздняя запись с ранним временем): --brief читает его целиком через
// отбор сырых строк, который запоминает ответ агента перед цитатой; через базу контекст цитаты тот же.
func TestDatabaseBriefUnorderedTranscript(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "unordered-db",
		agent("2026-10-31T09:58:00Z", "Удалить ветку feature-x?"),
		human("2026-10-31T10:00:00Z", "да, удали ветку"),
		agent("2026-10-31T10:01:00Z", "Удалил."),
		human("2026-10-31T10:05:00Z", "теперь собери релиз"),
		agent("2026-10-31T09:50:00Z", "Поздняя запись с более ранним временем. "+strings.Repeat("длинный текст ", 300)),
		agent("2026-10-31T10:06:00Z", "Собираю."))
	b := writeBrief(t, t.TempDir(), "Ветка (пользователь 31.10: «да, удали ветку»).\n")
	out := sameThroughDatabase(t, e, "--brief", b, "--until", "2026-10-31 11:00")
	mustHave(t, "контекст цитаты", out, "Удалить ветку feature-x?", "теперь собери релиз")
}

// Короткое слово: файлы отбирают сырые строки, где оно стоит целым словом (в JSON перенос строки - «\n», буква
// перед словом), а реплика, где оно лишь внутри другого слова, находкой не становится.
func TestDatabaseSearchShortWord(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "short-word",
		human("2026-10-24T09:00:00.000Z", "когда соберёшь"),
		human("2026-10-24T09:01:00.000Z", "да"),
		human("2026-10-24T09:02:00.000Z", "Да, конечно"),
		human("2026-10-24T09:03:00.000Z", "ок\nда"),
		ask("2026-10-24T09:04:00.000Z", "q1"),
		answer("2026-10-24T09:05:00.000Z", "q1", "Собрать?", "когда-нибудь"))
	out := sameThroughDatabase(t, e, "--search", "да")
	if !strings.HasPrefix(out, "=== ") || strings.Count(out, "=== ") != 2 {
		t.Errorf("files: two hits expected:\n%s", out)
	}
	sameThroughDatabase(t, e, "--search", "да", "--since", "2026-10-24")
}

// Реплика с картинкой: база хранит текст для показа с «[image]», user-word показывает слова, как из файла.
func TestDatabaseRepliesWithImages(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-x-proj", "with-image",
		agent("2026-10-24T09:00:00.000Z", "Что дальше?"),
		humanWithImage("2026-10-24T09:01:00.000Z", "глянь приоритет на снимке"),
		human("2026-10-24T09:02:00.000Z", "приоритет выше"))
	sameThroughDatabase(t, e, "--search", "приоритет")
	sameThroughDatabase(t, e, "--search", "на снимке")
	out := sameThroughDatabase(t, e, p, "глянь приоритет", "30")
	mustLack(t, "картинка", out, "[image]")
}

// Окно вопроса с несколькими ответами: ищется любой ответ, не только первый.
func TestDatabaseQuestionWithSeveralAnswers(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-x-proj", "answers",
		ask("2026-10-24T09:00:00.000Z", "q1"),
		`{"type":"user","timestamp":"2026-10-24T09:01:00.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"q1","content":"User answered"}]},`+
			`"toolUseResult":{"answers":{"Что с кэшем?":"Оставить","Что с веткой?":"Убрать (Рекомендую)"}}}`)
	out := sameThroughDatabase(t, e, p, "Оставить")
	mustHave(t, "второй ответ", out, "Оставить ← Что с кэшем?")
	sameThroughDatabase(t, e, "--search", "Убрать (Рекомендую)")
	sameThroughDatabase(t, e, "--search", "Оставить")
}

// withoutImages снимает с текста для показа ровно отметки картинок: слова события те же, что до Shown.
func TestWithoutImagesUndoesShown(t *testing.T) {
	for _, e := range []transcript.Event{{Text: "глянь"}, {Text: "глянь", Images: 1}, {Text: "две\n[image]", Images: 2}, {Images: 3}, {Text: "[image]"}, {Text: "слово\n[image]", Images: 1}} {
		if got := withoutImages(e.Shown(), e.Images); got != e.Text {
			t.Errorf("withoutImages(%q, %d) = %q, want %q", e.Shown(), e.Images, got, e.Text)
		}
	}
}
