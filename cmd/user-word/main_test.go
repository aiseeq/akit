package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// human, agent, ask, answer — строки транскрипта Claude Code: реплика человека, ответ агента, окно вопроса и ответ в нём.
func human(ts, text string) string {
	return fmt.Sprintf(`{"type":"user","origin":{"kind":"human"},"timestamp":"%s","message":{"role":"user","content":%q}}`, ts, text)
}

func agent(ts, text string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":"%s","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, ts, text)
}

func ask(ts, id string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":"%s","message":{"role":"assistant","content":[{"type":"tool_use","id":"%s","name":"AskUserQuestion","input":{"questions":[]}}]}}`, ts, id)
}

func answer(ts, id, q, a string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":"%s","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"%s","content":"User answered"}]},"toolUseResult":{"answers":{%q:%q}}}`, ts, id, q, a)
}

func writeTranscript(t *testing.T, home, project, name string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name+".jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func runUW(t *testing.T, e env, args ...string) (int, string, string) {
	t.Helper()
	opts, err := parseArgs(args)
	if err != nil {
		t.Fatalf("parseArgs %q: %v", args, err)
	}
	var out, errOut bytes.Buffer
	code := run(opts, e, &out, &errOut)
	return code, out.String(), errOut.String()
}

func testEnv(t *testing.T) env {
	return env{home: t.TempDir(), loc: time.UTC}
}

func TestParseArgsErrors(t *testing.T) {
	for _, args := range [][]string{
		{}, {"x.jsonl"}, {"x.jsonl", ""}, {"x.jsonl", "a", "-1"}, {"x.jsonl", "a", "1", "2"},
		{"--search"}, {"--search", "a", "--since"}, {"--search", "a", "--since", "25.10"}, {"--search", "a", "--frob", "x"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
}

func TestShowLastHitWithContext(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-x-proj", "s1",
		human("2026-10-25T10:00:00.000Z", "деплой в прод: да"),
		agent("2026-10-25T10:05:00.000Z", "Выкатить сегодня оба релиза?"),
		human("2026-10-25T10:06:00.000Z", "деплой в прод: да, оба"),
		agent("2026-10-25T10:07:00.000Z", "Выкатываю."),
		human("2026-10-25T10:20:00.000Z", "но один сегодняшний деплой"),
		human("2026-10-25T11:00:00.000Z", "это уже про другое"),
	)
	code, out, errOut := runUW(t, e, p, "деплой в прод")
	if code != 0 {
		t.Fatalf("code %d, err %q", code, errOut)
	}
	for _, want := range []string{
		"--- agent before the line (25.10 10:05:00), end:\nВыкатить сегодня оба релиза?",
		"--- user 25.10 10:06:00:\nдеплой в прод: да, оба",
		"--- user later, 25.10 10:20:00 (may be about something else):\nно один сегодняшний деплой",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("нет %q в\n%s", want, out)
		}
	}
	if strings.Contains(out, "это уже про другое") || strings.Contains(out, "10:00:00") {
		t.Errorf("лишнее: позже окна или раннее совпадение\n%s", out)
	}
	code, out, _ = runUW(t, e, p, "деплой в прод", "5")
	if code != 0 || !strings.Contains(out, "--- no other user lines within 5 min") {
		t.Errorf("окно 5 мин: code %d\n%s", code, out)
	}
	if code, _, errOut = runUW(t, e, p, "нет такого"); code != exitNone || !strings.Contains(errOut, "no user line") {
		t.Errorf("нет реплики: code %d, %q", code, errOut)
	}
	if code, _, _ = runUW(t, e, filepath.Join(e.home, "nope.jsonl"), "a"); code != exitUsage {
		t.Errorf("нет файла: code %d", code)
	}
}

// Агентский текст с куском — не реплика пользователя; ответ в окне вопроса — реплика.
func TestAskAnswerAndAgentText(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-x-proj", "s1",
		agent("2026-10-25T10:00:00.000Z", "вариант sub (Recommended)"),
		ask("2026-10-25T10:00:01.000Z", "t1"),
		answer("2026-10-25T10:01:00.000Z", "t1", "Какое имя?", "sub (Recommended)"),
	)
	code, out, errOut := runUW(t, e, p, "sub (Recommended)")
	if code != 0 {
		t.Fatalf("code %d, err %q", code, errOut)
	}
	if !strings.Contains(out, "--- user 25.10 10:01:00, answer in a question dialog:\nsub (Recommended) ← Какое имя?") {
		t.Errorf("ответ в окне вопроса:\n%s", out)
	}
}

func TestSearch(t *testing.T) {
	e := testEnv(t)
	lines := []string{
		agent("2026-10-24T09:00:00.000Z", "Собрать в akit?"),
		human("2026-10-24T09:01:00.000Z", `собрать в "akit" <всё>`),
		ask("2026-10-24T09:02:00.000Z", "t1"),
		answer("2026-10-24T09:03:00.000Z", "t1", "Куда?", `в "akit" <всё> тоже`),
	}
	writeTranscript(t, e.home, "-home-x-proj", "s1", lines...)
	// продолжение сессии повторяет записи — совпадение одно
	writeTranscript(t, e.home, "-home-x-proj", "s2", lines...)
	writeTranscript(t, e.home, "-home-x-other", "s3", human("2026-10-25T09:00:00.000Z", `и тут "akit" <всё>`))

	code, out, errOut := runUW(t, e, "--search", `"akit" <всё>`, "--project", "/home/x/proj")
	if code != 0 {
		t.Fatalf("code %d, err %q", code, errOut)
	}
	if n := strings.Count(out, "=== "); n != 2 {
		t.Errorf("ждали 2 совпадения (реплика и ответ в окне вопроса), %d:\n%s", n, out)
	}
	for _, want := range []string{"-home-x-proj/s1.jsonl · 2026-10-24 09:01:00", "--- agent before the line (24.10 09:00:00)", "answer in a question dialog"} {
		if !strings.Contains(out, want) {
			t.Errorf("нет %q в\n%s", want, out)
		}
	}
	if strings.Contains(out, "x-other") {
		t.Errorf("--project не отсёк другой проект:\n%s", out)
	}

	if code, _, _ = runUW(t, e, "--search", `"akit" <всё>`, "--since", "2026-10-25"); code != 0 {
		t.Errorf("--since: code %d", code)
	}
	_, out, _ = runUW(t, e, "--search", `"akit" <всё>`, "--since", "2026-10-25")
	if strings.Contains(out, "x-proj") || !strings.Contains(out, "x-other") {
		t.Errorf("--since отсекает по дате реплики:\n%s", out)
	}
	if code, _, _ = runUW(t, e, "--search", "нигде"); code != exitNone {
		t.Errorf("нет совпадений: code %d", code)
	}
}

func TestSearchManyHitsList(t *testing.T) {
	e := testEnv(t)
	var lines []string
	for i := range 7 {
		lines = append(lines, human(fmt.Sprintf("2026-10-25T10:0%d:00.000Z", i), fmt.Sprintf("%s решение %d %s", strings.Repeat("до ", 30), i, strings.Repeat("после ", 30))))
	}
	writeTranscript(t, e.home, "-home-x-proj", "s1", lines...)
	code, out, _ := runUW(t, e, "--search", "решение")
	if code != 0 || !strings.HasPrefix(out, "7 hits, by time:\n2026-10-25 10:00  ") {
		t.Fatalf("code %d:\n%s", code, out)
	}
	if !strings.Contains(out, "решение 0 после") || strings.Contains(out, "=== ") {
		t.Errorf("список с куском:\n%s", out)
	}
}

// Кусок, процитированный с другим регистром, через е вместо ё или с переносом строки, находит реплику: при точной
// сверке цитата не находилась в реплике, где одно слово начиналось с прописной.
func TestFragmentFolded(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-x-proj", "s1",
		human("2026-10-26T10:00:00.000Z", "ну ёлки! Перенести тех. Задачи в спринт\nи ещё в бэклог?"),
	)
	for _, frag := range []string{"Перенести тех. задачи в спринт", "ЕЛКИ", "в спринт и еще в", "в  спринт"} {
		if code, out, errOut := runUW(t, e, p, frag); code != 0 || !strings.Contains(out, "Задачи в спринт") {
			t.Errorf("%q: code %d, err %q\n%s", frag, code, errOut, out)
		}
	}
	if code, out, errOut := runUW(t, e, "--search", "тех. задачи В спринт и ЕЩЕ"); code != 0 || !strings.Contains(out, "=== ") {
		t.Errorf("--search: code %d, err %q\n%s", code, errOut, out)
	}
	if got := around("ну ёлки! Перенести тех. Задачи в спринт\nи ещё", "спринт и еще"); got != "ну ёлки! Перенести тех. Задачи в спринт и ещё" {
		t.Errorf("around: %q", got)
	}
	if code, _, errOut := runUW(t, e, p, "нет такого"); code != exitNone || !strings.Contains(errOut, "user-word --search") {
		t.Errorf("промах в файле подсказывает поиск по всем: code %d, %q", code, errOut)
	}
}
