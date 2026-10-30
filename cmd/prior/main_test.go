package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"### 2026-10-25T11:00 · ~/git/akit", "  в ответ на: про akit?",
	}
	if got := sectionOf(out, "реплики"); !strings.HasPrefix(got[0], "(daydigest не дописал журнал") || strings.Join(got[1:], "\n") != strings.Join(want, "\n") {
		t.Errorf("реплики:\n%s", strings.Join(got, "\n"))
	}
	if got := sectionOf(out, "механизмы"); len(got) != 2 || !strings.HasPrefix(got[0], "(карточки не сверены") || got[1] != "(нет карточек)" {
		t.Errorf("механизмы без inventory и карточек: %q", got)
	}
	if got := sectionOf(runPrior(t, home, "нигде"), "журнал решений"); len(got) != 1 || got[0] != "(нет)" {
		t.Errorf("ничего: %q", got)
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
