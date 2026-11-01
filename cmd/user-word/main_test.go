package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
	var first, latest time.Time
	for _, line := range lines {
		const key = `"timestamp":"`
		if at := strings.Index(line, key); at >= 0 {
			start := at + len(key)
			if end := strings.IndexByte(line[start:], '"'); end >= 0 {
				if stamp, err := time.Parse(time.RFC3339Nano, line[start:start+end]); err == nil {
					if first.IsZero() || stamp.Before(first) {
						first = stamp
					}
					if stamp.After(latest) {
						latest = stamp
					}
				}
			}
		}
	}
	if !first.IsZero() {
		mtime := first
		if first.In(time.Local).Format(time.DateOnly) != latest.In(time.Local).Format(time.DateOnly) {
			mtime = latest
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
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

// Поздние реплики до времени брифа — не больше briefLater, о скрытых сказано; в обычном режиме предела по числу нет.
func TestBriefLaterLinesCapped(t *testing.T) {
	e := testEnv(t)
	recs := []string{human("2026-10-31T10:00:00Z", "Можешь удалить таблицу")}
	for i := range briefLater + 2 {
		recs = append(recs, human(fmt.Sprintf("2026-10-31T10:%02d:00Z", i+1), fmt.Sprintf("потом %d", i)))
	}
	p := writeTranscript(t, e.home, "-home-x-proj", "cap00000-0000", recs...)
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("`user-word "+p+" 'удалить таблицу'`\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, out, _ := runUW(t, e, "--brief", brief, "--until", "2026-10-31 11:00")
	if !strings.Contains(out, "--- 2 more user lines not shown") || strings.Contains(out, fmt.Sprintf("потом %d\n", briefLater)) {
		t.Errorf("brief: ждали %d показанных и 2 скрытых:\n%s", briefLater, out)
	}
	_, out, _ = runUW(t, e, p, "удалить таблицу", "30")
	if !strings.Contains(out, fmt.Sprintf("потом %d\n", briefLater+1)) || strings.Contains(out, "not shown") {
		t.Errorf("обычный режим урезал поздние реплики:\n%s", out)
	}
}

func TestR6AskQuestionIsNotUserWords(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-r6", "ask00000-0000",
		ask("2026-10-31T10:00:00Z", "t1"),
		answer("2026-10-31T10:01:00Z", "t1", "Перенести выкатку на понедельник?", "Нет"),
	)
	if code, out, errOut := runUW(t, e, p, "Перенести выкатку на понедельник"); code != exitNone {
		t.Errorf("вопрос не должен считаться ответом: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, out, errOut := runUW(t, e, "--search", "Перенести выкатку на понедельник"); code != exitNone {
		t.Errorf("--search не должен считать вопрос словами: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	brief := writeBrief(t, t.TempDir(), "Пользователь 31.10: «Перенести выкатку на понедельник»\n`user-word "+p+" 'Перенести выкатку на понедельник'`\n")
	code, out := briefRun(t, e, brief, "--until", "2026-10-31 11:00")
	if code != exitNone || !strings.Contains(out, "2 user words: 0 found, 0 ambiguous, 2 not found") {
		t.Errorf("цитата и ссылка не должны считать вопрос словами: code %d\n%s", code, out)
	}
}

func TestR6MalformedTranscriptWarningWhenNotFound(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-r6", "bad00000-0000", `{"text":"absent fragment", broken`)
	if code, out, errOut := runUW(t, e, p, "absent fragment"); code != exitNone || !strings.Contains(out+errOut, "malformed") {
		t.Errorf("direct not found должен предупредить о битой строке: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, out, errOut := runUW(t, e, "--search", "absent fragment"); code != exitNone || !strings.Contains(out+errOut, "malformed") {
		t.Errorf("search not found должен предупредить о битой строке: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	brief := writeBrief(t, t.TempDir(), "`user-word "+p+" 'absent fragment'`\n")
	code, out := briefRun(t, e, brief, "--until", "2026-10-31 11:00")
	if code != exitNone || !strings.Contains(out, "malformed") {
		t.Errorf("brief not found должен предупредить о битой строке: code %d\n%s", code, out)
	}
}

func TestR6SearchDoesNotRereadOnMiss(t *testing.T) {
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-r6", "read0001-0000", human("2026-10-31T10:00:00Z", "другие слова"))
	reads := 0
	opts, err := parseArgs([]string{"--search", "zz no such fragment q9"})
	if err != nil {
		t.Fatal(err)
	}
	_, warnings, err := findHitsUsing(opts, e, func(path string, keep func([]byte) bool) ([]line, error) {
		reads++
		return readLines(path, keep)
	})
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Errorf("поиск перечитал транскрипт: readLines вызван %d раз", reads)
	}
	if len(warnings) != 0 {
		t.Errorf("неожиданное предупреждение, получили %d", len(warnings))
	}
}

func naiveRawPrefilter(raw []byte, fragment string) bool {
	best, bestWord := prefilterBest(fragment)
	if best == "" {
		return true
	}
	if len([]rune(best)) == 1 && len([]rune(bestWord)) <= 3 {
		for _, variant := range wordVariants([]rune(bestWord)) {
			term := jsonEscaped(variant)
			for from := 0; from <= len(raw)-len(term); {
				at := bytes.Index(raw[from:], term)
				if at < 0 {
					break
				}
				at += from
				if rawWordEdge(raw[:at], true) && rawWordEdge(raw[at+len(term):], false) {
					return true
				}
				from = at + len(term)
			}
		}
		return false
	}
	term := foldRawBytes(jsonEscaped(best))
	return bytes.Contains(foldRawBytes(raw), term)
}

func foldRawBytes(raw []byte) []byte {
	var folded []byte
	for len(raw) > 0 {
		r, size := utf8.DecodeRune(raw)
		folded = utf8.AppendRune(folded, prefilterRune(r))
		raw = raw[size:]
	}
	return folded
}

func TestPrefilterAnyMatchesNaiveByteSearch(t *testing.T) {
	rng := rand.New(rand.NewSource(8))
	fragments := [][]string{
		{"ab", "abc", "bc", "a"}, // перекрытие, вложение и короткие фрагменты
		{"северный ветер", "котёл и 界", "граница"},
		{"слово внутри длинного предложения"},
		{}, // пустой список
	}
	for _, fragment := range []string{"abcde", "котёл", "граница"} {
		best, _ := prefilterBest(fragment)
		raw := jsonEscaped(best)
		if got, want := prefilterAny([]string{fragment})(raw), naiveRawPrefilter(raw, fragment); got != want || !got {
			t.Fatalf("fragment at raw boundary %q: got %v, want %v", fragment, got, want)
		}
	}
	alphabet := []string{"a", "b", "c", "x", " ", "\\", "\"", "ё", "Е", "кот", "界", "🌿"}
	for i := range 10000 {
		group := fragments[i%len(fragments)]
		raw := make([]byte, 0, 128)
		count := rng.Intn(40)
		for range count {
			raw = append(raw, alphabet[rng.Intn(len(alphabet))]...)
		}
		if len(group) > 0 && i%2 == 0 {
			insert := []byte(group[rng.Intn(len(group))])
			at := rng.Intn(len(raw) + 1)
			raw = append(append(append([]byte(nil), raw[:at]...), insert...), raw[at:]...)
		}
		got := prefilterAny(group)(raw)
		want := false
		for _, fragment := range group {
			if naiveRawPrefilter(raw, fragment) {
				want = true
				break
			}
		}
		if got != want {
			t.Fatalf("case %d: prefilterAny(%q) = %v, naive bytes.Contains oracle = %v for %q", i, group, got, want, raw)
		}
	}
}

func TestSearchMultilineAskAnswerSeparatesQuestion(t *testing.T) {
	e := testEnv(t)
	p := writeTranscript(t, e.home, "-home-r6", "ask00001-0000",
		ask("2026-10-31T10:00:00Z", "t1"),
		answer("2026-10-31T10:01:00Z", "t1", "Удалить ветки?\nвсе архивные сразу", "Нет ← сначала спроси"),
	)
	if code, out, errOut := runUW(t, e, p, "все архивные сразу"); code != exitNone || strings.Contains(out, "Нет ← сначала спроси") {
		t.Errorf("строка вопроса не должна находиться: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, out, errOut := runUW(t, e, p, "Нет ← сначала спроси"); code != 0 || !strings.Contains(out, "Нет ← сначала спроси") {
		t.Errorf("ответ целиком должен находиться: code %d, stdout %q, stderr %q", code, out, errOut)
	}
}
