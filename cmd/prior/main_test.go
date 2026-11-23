package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runPrior(t *testing.T, home string, words ...string) string {
	t.Helper()
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	q, err := newQuery(words)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	run(q, home, &b)
	return b.String()
}

// sectionOf — строки раздела, чей заголовок начинается с title.
func sectionOf(out, title string) []string {
	var lines []string
	in := false
	for l := range strings.Lines(out) {
		l = strings.TrimSuffix(l, "\n")
		if strings.HasPrefix(l, "== ") {
			in = strings.HasPrefix(l, "== "+title)
			continue
		}
		if in && l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestParseArgs(t *testing.T) {
	words, err := parseArgs([]string{"-i", "компак", "compact"})
	if err != nil || len(words) != 2 {
		t.Errorf("-i пропускается: %q %v", words, err)
	}
	for _, args := range [][]string{{}, {"-i"}, {"-x", "слово"}} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("%q принято", args)
		}
	}
	if _, err := newQuery([]string{"("}); err == nil {
		t.Error("кривое регулярное выражение принято")
	}
}

func TestSources(t *testing.T) {
	home := t.TempDir()
	c := filepath.Join(home, ".claude")
	write(t, filepath.Join(c, "decisions.md"), "# Решения\n- 01.02 akit — набор утилит\n- другое\n")
	write(t, filepath.Join(c, "improve/state/backlog.md"), "- ждёт: замер akit\n")
	write(t, filepath.Join(c, "improve/state/declined.md"), "- нет\n")
	write(t, filepath.Join(c, "improve/runs/2026-10-24/a.md"), "x\nAKIT старый\n")
	write(t, filepath.Join(c, "improve/runs/2026-10-25/b.md"), "akit свежий\nakit ещё\n")
	write(t, filepath.Join(c, "user-replies.md"), "# Журнал\n\n### 2026-10-25T10:00 · ~/git/akit\nв ответ на: Собрать?\nсобрать в akit\n\n"+
		"### 2026-10-25T11:00 · ~/git/akit\nв ответ на: про akit?\nда\n")
	out := runPrior(t, home, "akit")

	if got := sectionOf(out, "журнал решений"); len(got) != 1 || got[0] != "2:- 01.02 akit — набор утилит" {
		t.Errorf("решения: %q", got)
	}
	if got := sectionOf(out, "пробы"); len(got) != 1 || got[0] != "backlog.md:1:- ждёт: замер akit" {
		t.Errorf("пробы: %q", got)
	}
	if got := sectionOf(out, "отчёты"); len(got) != 2 || got[0] != "2026-10-25/b.md: akit свежий" || got[1] != "2026-10-24/a.md: AKIT старый" {
		t.Errorf("отчёты: %q", got)
	}
	want := []string{

		"### 2026-10-25T10:00 · ~/git/akit", "  в ответ на: Собрать?", "  собрать в akit",
		"### 2026-10-25T11:00 · ~/git/akit", "  в ответ на: про akit?", "  да",
	}
	if got := sectionOf(out, "реплики"); !strings.HasPrefix(got[0], "(daydigest не дописал журнал") || strings.Join(got[1:], "\n") != strings.Join(want, "\n") {
		t.Errorf("реплики:\n%s", strings.Join(got, "\n"))
	}
	if got := sectionOf(out, "механизмы"); len(got) != 2 || !strings.HasPrefix(got[0], "(карточки не сверены") || got[1] != "(нет карточек)" {
		t.Errorf("механизмы без inventory и карточек: %q", got)
	}
	nothing := runPrior(t, home, "нигде")
	if got := sectionOf(nothing, "журнал решений"); len(got) != 0 {
		t.Errorf("пустой раздел напечатан отдельно: %q", got)
	}
	if last := lastLine(nothing); !strings.HasPrefix(last, "== пусто: ") || !strings.Contains(last, "журнал решений") {
		t.Errorf("последняя строка без перечня пустых разделов: %q", last)
	}
}

func lastLine(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	return lines[len(lines)-1]
}

// Пустые разделы и разделы с одними пометками идут в порядке источников раньше раздела с находкой; вывод
// начинается с находки, пометки — после неё, пустые — одной строкой в конце.
func TestHitsBeforeEmptySections(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude/user-replies.md"), "### 2026-10-25T10:00 · ~/git/x\n  находка про ключслово\n")
	out := runPrior(t, home, "ключслово")
	lines := strings.Split(strings.TrimLeft(out, "\n"), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "== реплики пользователя") {
		t.Fatalf("вывод начинается не с раздела находки:\n%s", out)
	}
	if !strings.Contains(strings.Join(lines[:min(4, len(lines))], "\n"), "ключслово") {
		t.Errorf("находки нет в первых строках:\n%s", out)
	}
	last := lastLine(out)
	for _, name := range []string{"журнал решений", "пробы и очереди ночного разбора"} {
		if !strings.Contains(last, name) {
			t.Errorf("в строке пустых нет %q: %q", name, last)
		}
	}
	if got := sectionOf(out, "журнал решений"); len(got) != 0 {
		t.Errorf("пустой раздел напечатан отдельно: %q", got)
	}
}

func TestRunsSelectionAndTruncation(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude/improve/runs")
	for _, day := range []string{"2026-10-28", "2026-10-27", "2026-10-26"} {
		for _, name := range []string{"a", "b", "c", "d"} {
			write(t, filepath.Join(dir, day, name+".md"), "слово "+name+"\n")
		}
	}

	got := sectionOf(runPrior(t, home, "слово"), "отчёты")
	want := []string{
		"2026-10-28/d.md: слово d", "2026-10-28/c.md: слово c", "2026-10-28/b.md: слово b",
		"2026-10-28/a.md: слово a", "2026-10-27/d.md: слово d", "2026-10-27/c.md: слово c",
		"2026-10-27/b.md: слово b", "2026-10-26/d.md: слово d", "2026-10-26/c.md: слово c",
		"2026-10-26/b.md: слово b",
		"… ещё 2 с совпадением, старейший: 2026-10-26/a.md",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("отчёты:\n%s", strings.Join(got, "\n"))
	}
}

func TestMechanisms(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude/improve/mechanisms")
	card := func(name, status, body string) {
		write(t, filepath.Join(dir, name), "# "+strings.TrimSuffix(name, ".md")+"\n<!-- stamp -->\nСтатус: "+status+"\nЧто: "+body+"\n")
	}
	// имя совпало — выше всех, хотя строк меньше
	card("tool-draft.md", "актуальна", "черновики")
	// два разных слова и ручная часть — её строка первой
	write(t, filepath.Join(dir, "project-monitor.md"), "# monitor\nСтатус: актуальна\nЧто: страж\nчерновик проверка\n"+
		"поле ввода\n## Важно знать\ninputChecks: черновик в поле ввода\n")
	card("cmd-old.md", "УСТАРЕЛА - источник новее", "черновик")
	card("cmd-none.md", "актуальна", "другое")
	out := runPrior(t, home, "draft", "черновик", "поле ввода")
	got := sectionOf(out, "механизмы")[1:] // первая строка — нет inventory
	want := []string{
		"tool-draft.md:1:tool-draft | Что: черновики",
		"project-monitor.md:1:monitor | Что: страж",
		"project-monitor.md:7:inputChecks: черновик в поле ввода",
		"project-monitor.md:4:черновик проверка",
		"project-monitor.md:5:поле ввода",
		"cmd-old.md:1:cmd-old | Что: черновик",
		"  Статус: УСТАРЕЛА - источник новее",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("механизмы:\n%s", strings.Join(got, "\n"))
	}
}

func TestMechanismsLimit(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude/improve/mechanisms")
	for i := range 15 {
		write(t, filepath.Join(dir, fmt.Sprintf("c%02d.md", i)), "# c\nСтатус: актуальна\nЧто: x\nслово 1\nслово 2\nслово 3\nслово 4\n")
	}
	got := sectionOf(runPrior(t, home, "слово"), "механизмы")[1:]
	if len(got) != maxCardLines+1 || got[len(got)-1] != "(ещё 5 карточек - сузьте слова: c10.md, c11.md, c12.md, c13.md, c14.md)" {
		t.Errorf("%d строк, последняя %q", len(got), got[len(got)-1])
	}
}

func TestSaid(t *testing.T) {
	home := t.TempDir()
	write(t, home+"/.claude/said.tsv", "time\tsession\tname\tproject\tscope\tkind\twords\tquote\tref\tsupersedes\n"+
		"2026-10-20T10:00:00+02:00\t1111aaaa-0000\tmain\t~\tglobal\trule\tвопросы минимум\tМеньше вопросов людям\t1111aaaa:10\t\n"+
		"2026-10-25T18:35:00+01:00\t2222bbbb-0000\t\t~\t~/git/demo\tdecision\tперенос demo akit\tДа, переносим\\nи akit\t2222bbbb:20\t\n"+
		"кривая строка\n"+
		"2026-10-26T14:01:33+01:00\t2222bbbb-0000\tmain\t~\tglobal\tdecision\tуказатель said вопросы\tпринимаю рекомендацию\t2222bbbb:30\t\n"+
		"2026-10-26T15:00:00+01:00\t2222bbbb-0000\tmain\t~\tglobal\trule\tвопросы\tвопросы можно\t2222bbbb:40#2\t1111aaaa:10\n")
	got := sectionOf(runPrior(t, home, "вопрос", "указател"), "сказанное")
	want := []string{"(строк не по формату: 1, первая — 4)",
		"2026-10-26T14:01:33+01:00 main decision global, сказано в ~ (tl 2222bbbb:30)", "  принимаю рекомендацию",
		"2026-10-26T15:00:00+01:00 main rule global, сказано в ~ (tl 2222bbbb:40)", "  вопросы можно",
		"2026-10-20T10:00:00+02:00 main rule global, сказано в ~ (tl 1111aaaa:10) ОТМЕНЕНА или изменена записью 2222bbbb:40#2",
		"  Меньше вопросов людям"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("указатель:\n%s\nждали:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	got = sectionOf(runPrior(t, home, "akit"), "сказанное")
	if len(got) != 3 || got[1] != "2026-10-25T18:35:00+01:00 2222bbbb decision ~/git/demo, сказано в ~ (tl 2222bbbb:20)" ||
		got[2] != "  Да, переносим ⏎ и akit" {
		t.Errorf("без имени и с переводом строки: %q", got)
	}
	if got := sectionOf(runPrior(t, t.TempDir(), "x"), "сказанное"); len(got) != 1 || got[0] != "(нет указателя)" {
		t.Errorf("нет файла: %q", got)
	}
	if unescape(`a\\n\tb\x`) != "a\\n\tb\\x" {
		t.Errorf("unescape: %q", unescape(`a\\n\tb\x`))
	}
}

// Topic words given as one quoted argument form a single phrase that matches
// only where the whole phrase stands; prior says so at the top. Short phrases,
// the form the hint itself advises, and alternations of short phrases stay a
// plain search.
func TestPhraseArgumentHint(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude", "decisions.md"), "# журнал\n- старый секрет клиента удалить позже\n")
	out := runPrior(t, home, "старый секрет клиента OAuth удалить")
	if !strings.HasPrefix(out, "prior: «старый секрет клиента OAuth удалить» — один аргумент из 5 слов") {
		t.Errorf("no phrase hint at the top:\n%s", out)
	}
	if !strings.Contains(out, "отдельными аргументами") {
		t.Errorf("hint does not say how to search:\n%s", out)
	}
	for _, args := range [][]string{
		{"окн[оа] сжатия", "секрет"},
		{"старый секрет клиента OAuth", "Demo App Client"},
		{"a b c d|e f", "300 ?тыс", `a\s+b`, "(x|y) z w v"},
	} {
		if out := runPrior(t, home, args...); strings.Contains(out, "одной фразой") {
			t.Errorf("%q flagged as a phrase:\n%s", args, out)
		}
	}
}

func TestRemovedRulesArchive(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, ".claude", "improve", "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := "# Снятые правила\n\nВступление со словом zebra не запись.\nПричина: пример zebra.\n\n" +
		"## rules/a.md:12 — 2026-01-02\nКласс: дубль\nПричина: повторяет правило b\n> Всегда проверять zebra\n> перед коммитом.\n\n" +
		"## rules/b.md:3 — 2026-01-03\nКласс: устарело\nПричина: инструмент удалён\n> Звать giraffe раз в день.\n"
	if err := os.WriteFile(filepath.Join(state, "removed-rules.md"), []byte(archive), 0o644); err != nil {
		t.Fatal(err)
	}
	q, err := newQuery([]string{"zebra"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	run(q, home, &out)
	got := out.String()
	for _, want := range []string{"== снятые правила", "removed-rules.md:6 rules/a.md:12 — 2026-01-02 [дубль]", "причина: повторяет правило b", "правило: Всегда проверять zebra перед коммитом."} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "giraffe") || strings.Contains(got, "Вступление") || strings.Contains(got, "пример zebra") {
		t.Errorf("unmatched entry or preamble shown:\n%s", got)
	}
	// Absent archive: nothing was removed, the section is empty.
	var empty bytes.Buffer
	run(q, t.TempDir(), &empty)
	if !strings.Contains(empty.String(), "пусто:") || !strings.Contains(empty.String(), "снятые правила") || strings.Contains(empty.String(), "== снятые правила") {
		t.Errorf("absent archive:\n%s", empty.String())
	}
}

// Много совпадений: показываются реплики с наибольшим числом слов темы, а не только последние, — старый ответ,
// где сошлись все слова, не вытесняется свежими с одним словом; порядок — как в журнале.
func TestRepliesRankedByTopicWords(t *testing.T) {
	home := t.TempDir()
	var b strings.Builder
	b.WriteString("### 2026-10-01T10:00:00Z · ~/x\nстарый ответ: ключ alpha и beta отдельно\n")
	for i := range 10 {
		fmt.Fprintf(&b, "### 2026-10-%02dT10:00:00Z · ~/x\nсвежая реплика %d про beta\n", 10+i, i)
	}
	write(t, filepath.Join(home, ".claude/user-replies.md"), b.String())
	got := sectionOf(runPrior(t, home, "alpha", "beta"), "реплики")[1:] // первая строка — пометка о daydigest
	if len(got) != 1+2*maxReplies || !strings.HasPrefix(got[0], "(совпадений 11, ниже 8 с наибольшим числом слов темы") {
		t.Fatalf("реплики:\n%s", strings.Join(got, "\n"))
	}
	if got[2] != "  старый ответ: ключ alpha и beta отдельно" || got[len(got)-1] != "  свежая реплика 9 про beta" {
		t.Errorf("не тот отбор или порядок:\n%s", strings.Join(got, "\n"))
	}
	if strings.Contains(strings.Join(got, "\n"), "свежая реплика 2 ") {
		t.Errorf("при равенстве слов вошла не самая свежая:\n%s", strings.Join(got, "\n"))
	}
}

// У реплики из сессии claude — ссылка tl на строку транскрипта: время журнала без хвостовых нулей сверяется с
// миллисекундами транскрипта; нет транскрипта или строки — сказано явно.
func TestRepliesLinkTranscriptLine(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude/projects/-p/1234abcd-0000-0000-0000-000000000001.jsonl"),
		`{"type":"user","timestamp":"2026-10-05T08:07:09.400Z","message":{"role":"user","content":"раньше"}}`+"\n"+
			`{"type":"user","timestamp":"2026-10-05T08:07:09.440Z","message":{"role":"user","content":"ответ про ключслово"}}`+"\n")
	write(t, filepath.Join(home, ".claude/user-replies.md"),
		"### 2026-10-05T08:07:09.44Z (05.10 10:07 CEST) · ~/x · claude 1234abcd-0000-0000-0000-000000000001\n  ответ про ключслово\n"+
			"### 2026-10-20T09:17:00Z (20.10 11:17 CEST) · ~/x · claude 1234abcd-0000-0000-0000-000000000001\n  ещё ключслово\n"+
			"### 2026-10-21T09:00:00Z (21.10 11:00 CEST) · ~/x · claude 99999999-0000-0000-0000-000000000001\n  снова ключслово\n"+
			"### 2026-10-22T09:00:00Z (22.10 11:00 CEST) · ~/x · codex 1234abcd-0000-0000-0000-000000000001\n  в codex ключслово\n"+
			"### 2026-10-23T09:00:00Z (23.10 11:00 CEST) · ~/x · claude 5555aaaa-0000-0000-0000-000000000001\n  двойной ключслово\n")
	for _, p := range []string{"-p", "-q"} {
		write(t, filepath.Join(home, ".claude/projects", p, "5555aaaa-0000-0000-0000-000000000001.jsonl"),
			`{"type":"user","timestamp":"2026-10-23T09:00:00.000Z"}`+"\n")
	}
	got := sectionOf(runPrior(t, home, "ключслово"), "реплики")
	want := []string{
		"### 2026-10-05T08:07:09.44Z (05.10 10:07 CEST) · ~/x · claude 1234abcd-0000-0000-0000-000000000001 · tl 1234abcd:2",
		"### 2026-10-20T09:17:00Z (20.10 11:17 CEST) · ~/x · claude 1234abcd-0000-0000-0000-000000000001 · tl: строки с 2026-10-20T09:17:00.000Z в транскрипте нет",
		"### 2026-10-21T09:00:00Z (21.10 11:00 CEST) · ~/x · claude 99999999-0000-0000-0000-000000000001 · tl: транскриптов сессии 99999999 найдено 0",
		"### 2026-10-22T09:00:00Z (22.10 11:00 CEST) · ~/x · codex 1234abcd-0000-0000-0000-000000000001",
		"### 2026-10-23T09:00:00Z (23.10 11:00 CEST) · ~/x · claude 5555aaaa-0000-0000-0000-000000000001 · tl: транскриптов сессии 5555aaaa найдено 2",
	}
	var heads []string
	for _, l := range got {
		if strings.HasPrefix(l, "### ") {
			heads = append(heads, l)
		}
	}
	if strings.Join(heads, "\n") != strings.Join(want, "\n") {
		t.Errorf("заголовки:\n%s", strings.Join(heads, "\n"))
	}
}

// Строка транскрипта длиннее буфера чтения считается одной строкой.
func TestLineWithLongLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, path, strings.Repeat("x", 3<<20)+"\nкороткая\n"+strings.Repeat("y", 2<<20)+"needle\n")
	if n, err := lineWith(path, []byte("needle")); err != nil || n != 3 {
		t.Errorf("needle: %d %v", n, err)
	}
	if n, err := lineWith(path, []byte("нет такого")); err != nil || n != 0 {
		t.Errorf("нет такого: %d %v", n, err)
	}
}

func TestLineWithTimestampFindsMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chronological.jsonl")
	write(t, path,
		`{"timestamp":"2026-10-01T10:00:00.000Z"}`+"\n"+
			`{"timestamp":"2026-10-02T10:00:00.000Z"}`+"\n"+
			`{"timestamp":"2026-10-03T10:00:00.000Z"}`+"\n"+
			`{"timestamp":"2026-10-04T10:00:00.000Z"}`+"\n")
	needle := []byte(`"timestamp":"2026-10-03T10:00:00.000Z"`)
	if n, err := lineWith(path, needle); err != nil || n != 3 {
		t.Fatalf("chronological lookup: line %d, err %v; want line 3", n, err)
	}
}

func TestLineWithTimestampFindsFirstMatchInUnorderedTranscript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unordered.jsonl")
	write(t, path,
		`{"timestamp":"2026-10-01T10:00:00.000Z"}`+"\n"+
			`{"timestamp":"2026-10-03T10:00:00.000Z"}`+"\n"+
			`{"timestamp":"2026-10-02T10:00:00.000Z"}`+"\n")
	needle := []byte(`"timestamp":"2026-10-02T10:00:00.000Z"`)
	if n, err := lineWith(path, needle); err != nil || n != 3 {
		t.Fatalf("unordered fallback: line %d, err %v; want line 3", n, err)
	}
}

func TestLineWithTimestampFindsFirstMatchDespiteSparseOrderedSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unordered-samples.jsonl")
	days := []int{1, 5, 2, 7, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10}
	var content strings.Builder
	for _, day := range days {
		fmt.Fprintf(&content, `{"timestamp":"2026-01-%02dT00:00:00.000Z"}`+"\n", day)
	}
	write(t, path, content.String())
	needle := []byte(`"timestamp":"2026-01-05T00:00:00.000Z"`)
	if n, err := lineWith(path, needle); err != nil || n != 2 {
		t.Fatalf("unordered lookup: line %d, err %v; want first match at line 2", n, err)
	}
}

func TestLineWithTimestampDoesNotMatchAcrossLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "split-match.jsonl")
	write(t, path, "{\"a\":1}\n{\"timestamp\":\"2026-01-05T00:00:00.000Z\"}\n")
	needle := []byte(`"timestamp":"2026-01-05T00:00:00.000Z"`)
	if n, err := lineWith(path, needle); err != nil || n != 2 {
		t.Fatalf("split lookup: line %d, err %v; want line 2", n, err)
	}
}

func TestTranscriptIndexMultipleReferencesAndAppend(t *testing.T) {
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	path := filepath.Join(t.TempDir(), "append.jsonl")
	write(t, path, `{"timestamp":"2026-01-01T00:00:00.000Z"}`+"\n"+`{"type":"assistant"}`+"\n")
	first := `"timestamp":"2026-01-01T00:00:00.000Z"`
	second := `"timestamp":"2026-01-02T00:00:00.000Z"`
	got, err := transcriptTimestampLines(path, map[string]struct{}{first: {}})
	if err != nil || got[first] != 1 {
		t.Fatalf("initial index = %v, %v", got, err)
	}
	firstInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	indexed, ok, err := loadTranscriptLines(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || indexed.ScannedBytes != firstInfo.Size() || indexed.LineCount != 2 {
		t.Fatalf("timestamp index did not cover the file: cache=%+v size=%d", indexed, firstInfo.Size())
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"timestamp":"2026-01-02T00:00:00.000Z"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	needles := map[string]struct{}{first: {}, second: {}}
	got, err = transcriptTimestampLines(path, needles)
	if err != nil || got[second] != 3 || got[first] != 1 {
		t.Fatalf("appended index = %v, %v; want lines 3 and 1", got, err)
	}
	cachePath := transcriptLinesCachePath(path)
	if err := os.WriteFile(cachePath, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = transcriptTimestampLines(path, needles)
	if err != nil || got[second] != 3 || got[first] != 1 {
		t.Fatalf("rebuilt index = %v, %v; want lines 3 and 1", got, err)
	}
}

func TestTranscriptIndexResumesAtUnterminatedLine(t *testing.T) {
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	path := filepath.Join(t.TempDir(), "partial.jsonl")
	first := `"timestamp":"2026-01-01T00:00:00.000Z"`
	second := `"timestamp":"2026-01-02T00:00:00.000Z"`
	third := `"timestamp":"2026-01-03T00:00:00.000Z"`
	write(t, path, "{"+first+"}\n{"+second+"}")
	got, err := transcriptTimestampLines(path, map[string]struct{}{first: {}, second: {}})
	if err != nil || got[first] != 1 || got[second] != 2 {
		t.Fatalf("partial index = %v, %v; want lines 1 and 2", got, err)
	}
	cache, ok, err := loadTranscriptLines(path)
	if err != nil || !ok || cache.ScannedBytes != int64(len("{"+first+"}\n")) || cache.LineCount != 1 {
		t.Fatalf("partial line cache = %+v, %v; want only the complete first line", cache, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n{" + third + "}\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err = transcriptTimestampLines(path, map[string]struct{}{first: {}, second: {}, third: {}})
	if err != nil || got[first] != 1 || got[second] != 2 || got[third] != 3 {
		t.Fatalf("resumed index = %v, %v; want lines 1, 2, and 3", got, err)
	}
}

func TestTranscriptCachePersistsAppendDuringScan(t *testing.T) {
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	path := filepath.Join(t.TempDir(), "growing.jsonl")
	first := "{\"timestamp\":\"2026-01-01T00:00:00.000Z\"}\n"
	second := "{\"timestamp\":\"2026-01-02T00:00:00.000Z\"}\n"
	third := "{\"timestamp\":\"2026-01-03T00:00:00.000Z\"}\n"
	write(t, path, first)
	secondNeedle := `"timestamp":"2026-01-02T00:00:00.000Z"`
	appended := false
	var offsets []int64
	transcriptScanAfterLineHook = func(gotPath string, offset int64) {
		if gotPath != path {
			return
		}
		offsets = append(offsets, offset)
		if appended {
			return
		}
		appended = true
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(second); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { transcriptScanAfterLineHook = nil })
	got, err := transcriptTimestampLines(path, map[string]struct{}{secondNeedle: {}})
	if err != nil || got[secondNeedle] != 2 || !appended || len(offsets) != 2 ||
		offsets[0] != int64(len(first)) || offsets[1] != int64(len(first+second)) {
		t.Fatalf("scan during append = %v, %v, appended=%v, offsets=%v; want one pass through line 2", got, err, appended, offsets)
	}
	cache, ok, err := loadTranscriptLines(path)
	if err != nil || !ok || cache.ScannedBytes != int64(len(first+second)) || cache.Size != int64(len(first+second)) {
		t.Fatalf("saved growing cache = %+v, %v; want complete appended prefix", cache, err)
	}
	start := cache.ScannedBytes
	if err := os.WriteFile(path, []byte(first+second+third), 0600); err != nil {
		t.Fatal(err)
	}
	var bytesScanned int64
	transcriptScanAfterLineHook = func(gotPath string, offset int64) {
		if gotPath == path {
			bytesScanned += offset - start
			start = offset
		}
	}
	thirdNeedle := `"timestamp":"2026-01-03T00:00:00.000Z"`
	got, err = transcriptTimestampLines(path, map[string]struct{}{thirdNeedle: {}})
	transcriptScanAfterLineHook = nil
	if err != nil || got[thirdNeedle] != 3 || bytesScanned != int64(len(third)) {
		t.Fatalf("tail scan = %v, %v, bytes=%d; want line 3 and %d bytes", got, err, bytesScanned, len(third))
	}
}

func TestTranscriptCachePrefixInvalidation(t *testing.T) {
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	path := filepath.Join(t.TempDir(), "replace.jsonl")
	oldLine := "{\"timestamp\":\"2026-01-01T00:00:00.000Z\"}\n"
	newLine := "{\"timestamp\":\"2026-01-02T00:00:00.000Z\"}\n"
	write(t, path, oldLine)
	oldNeedle := `"timestamp":"2026-01-01T00:00:00.000Z"`
	newNeedle := `"timestamp":"2026-01-02T00:00:00.000Z"`
	got, err := transcriptTimestampLines(path, map[string]struct{}{oldNeedle: {}})
	if err != nil || got[oldNeedle] != 1 {
		t.Fatalf("initial cache = %v, %v; want line 1", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldLine) != len(newLine) {
		t.Fatal("test replacement must preserve file size")
	}
	if err := os.WriteFile(path, []byte(newLine), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	var scanned int64
	transcriptScanAfterLineHook = func(gotPath string, offset int64) {
		if gotPath == path {
			scanned = offset
		}
	}
	got, err = transcriptTimestampLines(path, map[string]struct{}{newNeedle: {}})
	transcriptScanAfterLineHook = nil
	if err != nil || got[newNeedle] != 1 || scanned != int64(len(newLine)) {
		t.Fatalf("same-size prefix replacement = %v, %v, scanned=%d; want rebuild of %d bytes", got, err, scanned, len(newLine))
	}
	changedTime := info.ModTime().Add(2 * time.Second)
	if err := os.WriteFile(path, []byte(oldLine), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, changedTime, changedTime); err != nil {
		t.Fatal(err)
	}
	scanned = 0
	transcriptScanAfterLineHook = func(gotPath string, offset int64) {
		if gotPath == path {
			scanned = offset
		}
	}
	got, err = transcriptTimestampLines(path, map[string]struct{}{oldNeedle: {}})
	transcriptScanAfterLineHook = nil
	if err != nil || got[oldNeedle] != 1 || scanned != int64(len(oldLine)) {
		t.Fatalf("same-size mtime invalidation = %v, %v, scanned=%d; want line 1 and full rebuild", got, err, scanned)
	}
}

func TestTranscriptIndexScansPastRequestedLine(t *testing.T) {
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	path := filepath.Join(t.TempDir(), "1234abcd-0000-0000-0000-000000000001.jsonl")
	write(t, path, `{"timestamp":"2026-01-01T00:00:00.000Z"}`+"\n"+`{"timestamp":"2026-01-02T00:00:00.000Z"}`+"\n")
	if _, err := transcriptTimestampLines(path, map[string]struct{}{`"timestamp":"2026-01-01T00:00:00.000Z"`: {}}); err != nil {
		t.Fatal(err)
	}
	cache, ok, err := loadTranscriptLines(path)
	if err != nil || !ok || cache.Lines[`"timestamp":"2026-01-01T00:00:00.000Z"`] != 1 ||
		cache.Lines[`"timestamp":"2026-01-02T00:00:00.000Z"`] != 2 {
		t.Fatalf("prewarmed lines = %+v, loaded=%v err=%v", cache.Lines, ok, err)
	}
}

func TestReplyPrewarmCacheSkipsAndIndexesAppendedHeaders(t *testing.T) {
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	root := t.TempDir()
	repliesPath := filepath.Join(root, "user-replies.md")
	transcript := filepath.Join(root, "1234abcd-0000-0000-0000-000000000001.jsonl")
	firstHeader := "### 2026-01-01T00:00:00Z (01.01 00:00 UTC) · claude 1234abcd-0000-0000-0000-000000000001\n"
	secondHeader := "### 2026-01-02T00:00:00Z (02.01 00:00 UTC) · claude 1234abcd-0000-0000-0000-000000000001\n"
	firstLine := `{"timestamp":"2026-01-01T00:00:00.000Z"}` + "\n"
	secondLine := `{"timestamp":"2026-01-02T00:00:00.000Z"}` + "\n"
	write(t, repliesPath, firstHeader)
	write(t, transcript, firstLine)
	index := transcriptIndex{"1234abcd": {transcript}}
	prewarmReplyTranscripts(repliesPath, firstHeader, index)

	var scanned int
	transcriptScanAfterLineHook = func(path string, _ int64) {
		if path == transcript {
			scanned++
		}
	}
	prewarmReplyTranscripts(repliesPath, firstHeader, index)
	transcriptScanAfterLineHook = nil
	if scanned != 0 {
		t.Fatalf("unchanged reply journal rescanned %d transcript lines", scanned)
	}

	f, err := os.OpenFile(repliesPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(secondHeader); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f, err = os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(secondLine); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	prewarmReplyTranscripts(repliesPath, firstHeader+secondHeader, index)
	cache, ok, err := loadTranscriptLines(transcript)
	if err != nil || !ok || cache.Lines[`"timestamp":"2026-01-02T00:00:00.000Z"`] != 2 {
		t.Fatalf("appended reply index = %+v, loaded=%v err=%v", cache.Lines, ok, err)
	}
}

func TestCachedRepliesAppendAndInvalidation(t *testing.T) {
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	path := filepath.Join(t.TempDir(), "user-replies.md")
	initial := "first reply\n"
	write(t, path, initial)
	got, err := readCachedReplies(path)
	if err != nil || string(got) != initial {
		t.Fatalf("initial cache read = %q, %v", got, err)
	}
	appendix := "second reply\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(appendix); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	want := initial + appendix
	got, err = readCachedReplies(path)
	if err != nil || string(got) != want {
		t.Fatalf("appended cache read = %q, %v; want %q", got, err, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := strings.Repeat("x", len(want)-1) + "\n"
	if err := os.WriteFile(path, []byte(replacement), 0600); err != nil {
		t.Fatal(err)
	}
	mtime := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	got, err = readCachedReplies(path)
	if err != nil || string(got) != replacement {
		t.Fatalf("same-size changed cache read = %q, %v; want %q", got, err, replacement)
	}
	cachePath := replyCachePath(path)
	if err := os.WriteFile(cachePath, []byte("broken cache"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = readCachedReplies(path)
	if err != nil || string(got) != replacement {
		t.Fatalf("broken cache rebuild = %q, %v; want %q", got, err, replacement)
	}
}

func TestCachedRepliesRejectsRewrittenPrefixBeforeAppend(t *testing.T) {
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	path := filepath.Join(t.TempDir(), "user-replies.md")
	var old strings.Builder
	for i := range 31 {
		fmt.Fprintf(&old, "### 2026-01-01T00:%02d:00Z lead\nzebra ответ %d\n\n", i, i+1)
	}
	write(t, path, old.String())
	if _, err := readCachedReplies(path); err != nil {
		t.Fatal(err)
	}
	rebuilt := strings.Replace(old.String(), "zebra ответ 5", "horse ответ 5 исправлено", 1) +
		"### 2026-01-02T00:00:00Z lead\nzebra ответ 33\n\n"
	if err := os.WriteFile(path, []byte(rebuilt), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := readCachedReplies(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) || strings.Count(string(got), "zebra ответ 33") != 1 || strings.Contains(string(got), "zebra ответ 5") {
		t.Fatalf("cached journal differs from rebuilt source; zebra33 count=%d", strings.Count(string(got), "zebra ответ 33"))
	}
}

func TestTranscriptCacheRejectsLongRewrittenPrefixAndAppend(t *testing.T) {
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	path := filepath.Join(t.TempDir(), "long-prefix.jsonl")
	var original strings.Builder
	for day := 1; day <= 6; day++ {
		fmt.Fprintf(&original, `{"timestamp":"2026-01-%02dT00:00:00.000Z","pad":"%s"}`+"\n", day, strings.Repeat("x", 80))
	}
	target := `"timestamp":"2026-01-05T00:00:00.000Z"`
	write(t, path, original.String())
	got, err := transcriptTimestampLines(path, map[string]struct{}{target: {}})
	if err != nil || got[target] != 5 {
		t.Fatalf("initial target line=%d err=%v; want 5", got[target], err)
	}
	changed := []byte(original.String())
	padAt := bytes.Index(changed, []byte(`"pad":"`)) + len(`"pad":"`)
	if padAt < len(`"pad":"`) || changed[padAt] != 'x' {
		t.Fatal("test prefix marker not found")
	}
	changed[padAt] = '\n' // same byte length, adds a line before the target
	changed = append(changed, []byte(`{"timestamp":"2026-01-07T00:00:00.000Z"}`+"\n")...)
	if err := os.WriteFile(path, changed, 0644); err != nil {
		t.Fatal(err)
	}
	got, err = transcriptTimestampLines(path, map[string]struct{}{target: {}})
	if err != nil || got[target] != 6 {
		t.Fatalf("rewritten-prefix target line=%d err=%v; want 6", got[target], err)
	}
}

func TestTranscriptCacheLongSameSizeMTimeChangeAndLargeLine(t *testing.T) {
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	path := filepath.Join(t.TempDir(), "long-line.jsonl")
	target := `"timestamp":"2026-01-02T00:00:00.000Z"`
	first := `{"timestamp":"2026-01-01T00:00:00.000Z","pad":"` + strings.Repeat("x", (1<<20)+64) + `"}` + "\n"
	second := `{"timestamp":"2026-01-02T00:00:00.000Z"}` + "\n"
	write(t, path, first+second)
	got, err := transcriptTimestampLines(path, map[string]struct{}{target: {}})
	if err != nil || got[target] != 2 {
		t.Fatalf("large-line lookup=%d err=%v; want 2", got[target], err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := []byte(first + second)
	changed[bytes.Index(changed, []byte("xxxx"))+(16<<10)] = 'y'
	if err := os.WriteFile(path, changed, 0644); err != nil {
		t.Fatal(err)
	}
	changedTime := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, changedTime, changedTime); err != nil {
		t.Fatal(err)
	}
	var scanned int64
	transcriptScanAfterLineHook = func(gotPath string, offset int64) {
		if gotPath == path {
			scanned = offset
		}
	}
	t.Cleanup(func() { transcriptScanAfterLineHook = nil })
	got, err = transcriptTimestampLines(path, map[string]struct{}{target: {}})
	if err != nil || got[target] != 2 || scanned != int64(len(changed)) {
		t.Fatalf("same-size mtime lookup=%d err=%v scanned=%d; want line 2 and full %d-byte rebuild", got[target], err, scanned, len(changed))
	}
}

func TestUnreadableSourcesDoNotUseCachedText(t *testing.T) {
	t.Run("replies", func(t *testing.T) {
		t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
		path := filepath.Join(t.TempDir(), "user-replies.md")
		write(t, path, "cached reply\n")
		if _, err := readCachedReplies(path); err != nil {
			t.Fatal(err)
		}
		previous := openPriorSource
		openPriorSource = func(got string) (*os.File, error) {
			if got == path {
				return nil, fmt.Errorf("forced source open failure")
			}
			return os.Open(got)
		}
		t.Cleanup(func() { openPriorSource = previous })
		if _, err := readCachedReplies(path); err == nil || !strings.Contains(err.Error(), "forced source open failure") {
			t.Fatalf("cached journal hid source open error: %v", err)
		}
	})
	t.Run("report", func(t *testing.T) {
		t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
		q, err := newQuery([]string{"needle"})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "2026-01-01", "report.md")
		write(t, path, "needle cached\n")
		index := runIndexManifest{Version: runCacheVersion, Files: make(map[string]runIndexEntry)}
		changed := atomic.Bool{}
		_, entry, ok, err := matchRunFileCached(q, path, index, &changed)
		if err != nil || !ok {
			t.Fatalf("warm source result=%v cache=%v err=%v", ok, entry, err)
		}
		index.Files[runIndexKey(path)] = entry
		previous := openPriorSource
		openPriorSource = func(got string) (*os.File, error) {
			if got == path {
				return nil, fmt.Errorf("forced source open failure")
			}
			return os.Open(got)
		}
		t.Cleanup(func() { openPriorSource = previous })
		if _, _, _, err := matchRunFileCached(q, path, index, &changed); err == nil || !strings.Contains(err.Error(), "forced source open failure") {
			t.Fatalf("cached report hid source open error: %v", err)
		}
	})
}

func TestReplyPrewarmSkipsWhenCacheCannotBeWritten(t *testing.T) {
	root := t.TempDir()
	cacheRoot := filepath.Join(root, "cache-file")
	write(t, cacheRoot, "not a directory")
	t.Setenv("AKIT_PRIOR_CACHE_DIR", cacheRoot)
	repliesPath := filepath.Join(root, "user-replies.md")
	transcript := filepath.Join(root, "1234abcd-0000-0000-0000-000000000001.jsonl")
	header := "### 2026-01-01T00:00:00Z (01.01 00:00 UTC) · claude 1234abcd-0000-0000-0000-000000000001\n"
	write(t, repliesPath, header)
	write(t, transcript, `{"timestamp":"2026-01-01T00:00:00.000Z"}`+"\n")
	var scans int
	transcriptScanAfterLineHook = func(path string, _ int64) {
		if path == transcript {
			scans++
		}
	}
	t.Cleanup(func() { transcriptScanAfterLineHook = nil })
	prewarmReplyTranscripts(repliesPath, header, transcriptIndex{"1234abcd": {transcript}})
	if scans != 0 {
		t.Fatalf("nonwritable cache caused %d prewarm scans", scans)
	}
}

func TestPriorReplyLinkUsesFirstTranscriptMatch(t *testing.T) {
	days := []int{1, 5, 2, 7, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10}
	var unordered strings.Builder
	for _, day := range days {
		fmt.Fprintf(&unordered, `{"timestamp":"2026-01-%02dT00:00:00.000Z"}`+"\n", day)
	}
	for _, tc := range []struct {
		name, transcript string
	}{
		{"sparse samples do not prove ordering", unordered.String()},
		{"timestamp on next line stays on that line", "{\"a\":1}\n{\"timestamp\":\"2026-01-05T00:00:00.000Z\"}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			write(t, filepath.Join(home, ".claude/projects/-p/1234abcd-0000-0000-0000-000000000001.jsonl"), tc.transcript)
			write(t, filepath.Join(home, ".claude/user-replies.md"),
				"### 2026-01-05T00:00:00Z (05.01 00:00 UTC) · ~/x · claude 1234abcd-0000-0000-0000-000000000001\nключслово\n")
			got := sectionOf(runPrior(t, home, "ключслово"), "реплики")
			if len(got) < 2 || !strings.HasSuffix(got[1], "· tl 1234abcd:2") {
				t.Fatalf("prior reply link = %q, want first match tl 1234abcd:2", got)
			}
		})
	}
}

// Блок журнала — одно место в выводе, даже если совпали и фраза агента, и ответ; ранг — разные слова темы во всём
// блоке: короткий ответ «да» на вопрос агента ровно по теме не вытесняется ответами с одним словом, а под совпавшей
// фразой агента видна первая строка ответа.
func TestRepliesBlockOnceRankedByBlockWords(t *testing.T) {
	home := t.TempDir()
	var b strings.Builder
	b.WriteString("### 2026-10-01T10:00:00Z · ~/x\nв ответ на: удалить alpha и beta?\nда\nвторая строка ответа\n")
	b.WriteString("### 2026-10-02T10:00:00Z · ~/x\nв ответ на: про alpha\nответ alpha и beta\n")
	b.WriteString("### 2026-10-03T10:00:00Z · ~/x\nв ответ на: снова beta\nи тут beta\n") // одно слово дважды — одно
	for i := range 8 {
		fmt.Fprintf(&b, "### 2026-10-%02dT10:00:00Z · ~/x\nв ответ на: что-то\nпро beta %d\n", 10+i, i)
	}
	write(t, filepath.Join(home, ".claude/user-replies.md"), b.String())
	got := sectionOf(runPrior(t, home, "alpha", "beta"), "реплики")[1:]
	text := strings.Join(got, "\n")
	if strings.Count(text, "### 2026-10-01T10:00:00Z") != 1 || strings.Count(text, "### 2026-10-02T10:00:00Z") != 1 {
		t.Fatalf("блок с вопросом по теме вытеснен или повторён:\n%s", text)
	}
	if !strings.Contains(text, "  да") || strings.Contains(text, "вторая строка ответа") {
		t.Errorf("под фразой агента не первая строка ответа:\n%s", text)
	}
	if strings.Contains(text, "и тут beta") || strings.Contains(text, "про beta 1") || !strings.Contains(text, "про beta 2") || !strings.Contains(text, "про beta 7") {
		t.Errorf("при равенстве слов вошли не самые свежие:\n%s", text)
	}
}
