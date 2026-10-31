package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// fixture — источник (домашняя папка) и данные выгрузки, как в cfg.
func fixture(t *testing.T) (src, rules string) {
	t.Helper()
	root := t.TempDir()
	src, rules = filepath.Join(root, "home"), filepath.Join(root, "rules")
	write(t, filepath.Join(src, ".claude/skills/jira/SKILL.md"), "# jira\nПочта: me@corp.example → тикет\nличное: удалить\n", 0o644)
	write(t, filepath.Join(src, ".claude/skills/jira/run.sh"), "#!/bin/sh\necho ok\n", 0o755)
	write(t, filepath.Join(src, ".claude/skills/jira/logo.png"), "\x89PNG\x00\x01me@corp.example", 0o644)
	if err := os.Symlink("SKILL.md", filepath.Join(src, ".claude/skills/jira/README.md")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, ".claude/skills/secret/SKILL.md"), "# не выгружать\n", 0o644)
	write(t, filepath.Join(src, ".claude/CLAUDE.md"), "Факты\n- общий факт\n  продолжение\n\nЛичное\n- не выгружать\nGit\n- правило git\n", 0o644)
	write(t, filepath.Join(src, "work/CLAUDE.md"), "# Work rules\n", 0o644)
	write(t, filepath.Join(rules, "manifest/skills.txt"), "jira\n", 0o644)
	write(t, filepath.Join(rules, "manifest/global-sections.txt"), "Факты\nGit\n", 0o644)
	write(t, filepath.Join(rules, "manifest/replace.tsv"), "# замены\nme@corp.example\t<твоя почта>\n", 0o644)
	write(t, filepath.Join(rules, "manifest/drop-lines.txt"), "личное:\n", 0o644)
	write(t, filepath.Join(rules, "manifest/pii.tsv"), "email\t[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\\.[a-z]{2,}\tnoreply@anthropic\\.com\n"+
		"IPv4\t\\b(?:[0-9]{1,3}\\.){3}[0-9]{1,3}\\b\t127\\.\\d+\\.\\d+\\.\\d+|0\\.0\\.0\\.0\n", 0o644)
	write(t, filepath.Join(rules, "README.template.md"), "# Правила\n", 0o644)
	return src, rules
}

func runExport(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	opts, err := parseArgs(args, t.TempDir())
	if err != nil {
		t.Fatalf("parseArgs %q: %v", args, err)
	}
	var out, errOut bytes.Buffer
	code := run(opts, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestParseArgs(t *testing.T) {
	for _, args := range [][]string{{}, {"a", "b"}, {"--rules"}, {"--frob", "x"}} {
		if _, err := parseArgs(args, "/h"); err == nil {
			t.Errorf("%q принято", args)
		}
	}
	opts, err := parseArgs([]string{"out"}, "/h")
	if err != nil || opts.srcHome != "/h" || opts.rules != "/h/.claude/tools/claude-rules-export" {
		t.Errorf("умолчания: %+v %v", opts, err)
	}
}

func TestExport(t *testing.T) {
	src, rules := fixture(t)
	out := filepath.Join(t.TempDir(), "out")
	write(t, filepath.Join(out, ".git/HEAD"), "ref\n", 0o644)
	write(t, filepath.Join(out, "stale.md"), "старое\n", 0o644)
	code, stdout, stderr := runExport(t, "--src-home", src, "--rules", rules, out)
	if code != 0 {
		t.Fatalf("code %d\n%s%s", code, stdout, stderr)
	}
	if got := read(t, filepath.Join(out, "skills/jira/SKILL.md")); got != "# jira\nПочта: <твоя почта> → тикет\n" {
		t.Errorf("замена и вычёркивание: %q", got)
	}
	if got := read(t, filepath.Join(out, "global/CLAUDE.md")); got != "Факты\n- общий факт\n  продолжение\n\nGit\n- правило git\n" {
		t.Errorf("разделы: %q", got)
	}
	if st, err := os.Stat(filepath.Join(out, "skills/jira/run.sh")); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("права скрипта: %v %v", st, err)
	}
	if link, err := os.Readlink(filepath.Join(out, "skills/jira/README.md")); err != nil || link != "SKILL.md" {
		t.Errorf("ссылка: %q %v", link, err)
	}
	if got := read(t, filepath.Join(out, "skills/jira/logo.png")); !strings.Contains(got, "me@corp.example") {
		t.Error("двоичный файл тронут")
	}
	for _, p := range []string{"skills/secret", "stale.md"} {
		if _, err := os.Stat(filepath.Join(out, p)); err == nil {
			t.Errorf("лишнее в выгрузке: %s", p)
		}
	}
	if read(t, filepath.Join(out, ".git/HEAD")) != "ref\n" {
		t.Error(".git тронут")
	}
	if !strings.Contains(stdout, "no private data") {
		t.Errorf("итог: %q", stdout)
	}
}

func TestExportPII(t *testing.T) {
	src, rules := fixture(t)
	write(t, filepath.Join(src, ".claude/skills/jira/SKILL.md"), "# jira\nКонтакт: fixture@example.org, хост 10.1.2.3, локально 127.0.0.1\nnoreply@anthropic.com\n", 0o644)
	// README — шаблон как есть, без замен: почта в нём ловится проверкой
	write(t, filepath.Join(rules, "README.template.md"), "# Правила\nАвтор: me@corp.example\n", 0o644)
	out := filepath.Join(t.TempDir(), "out")
	code, stdout, _ := runExport(t, "--src-home", src, "--rules", rules, out)
	if code != exitDirty {
		t.Fatalf("code %d\n%s", code, stdout)
	}
	for _, want := range []string{"skills/jira/SKILL.md:2: email", "skills/jira/SKILL.md:2: IPv4", "README.md:2: email"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("нет %q в\n%s", want, stdout)
		}
	}
	if strings.Count(stdout, "IPv4") != 1 || strings.Contains(stdout, ":3: email") {
		t.Errorf("разрешённое попало в находки:\n%s", stdout)
	}
}

func TestExportErrors(t *testing.T) {
	src, rules := fixture(t)
	write(t, filepath.Join(rules, "manifest/skills.txt"), "jira\nnope\n", 0o644)
	if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, filepath.Join(t.TempDir(), "o")); code != exitDirty || !strings.Contains(stderr, "missing skill: nope") {
		t.Errorf("нет скилла: code %d %q", code, stderr)
	}
	write(t, filepath.Join(rules, "manifest/skills.txt"), "jira\n", 0o644)
	if code, _, _ := runExport(t, "--src-home", src, "--rules", rules, src); code != exitUsage {
		t.Errorf("OUT_DIR = источник: code %d", code)
	}
	root := filepath.Dir(src)
	for _, out := range []string{root, rules, filepath.Join(src, ".claude")} {
		code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out)
		if code != exitUsage || !strings.Contains(stderr, "must not contain") {
			t.Errorf("OUT_DIR %s содержит источник или правила: code %d %q", out, code, stderr)
		}
		if read(t, filepath.Join(src, "work/CLAUDE.md")) != "# Work rules\n" || read(t, filepath.Join(rules, "manifest/skills.txt")) != "jira\n" {
			t.Fatalf("OUT_DIR %s: источник или правила стёрты", out)
		}
	}
	// то же через симлинки: OUT_DIR — ссылка на читаемое или путь через ссылку на него, источник — ссылка
	links := t.TempDir()
	for name, target := range map[string]string{"home": src, "root": root, "rules": rules, "claude": filepath.Join(src, ".claude")} {
		if err := os.Symlink(target, filepath.Join(links, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range [][2]string{
		{src, filepath.Join(links, "home")}, {src, filepath.Join(links, "root")}, {src, filepath.Join(links, "rules")},
		{src, filepath.Join(links, "claude")}, {src, filepath.Join(links, "root", "home")}, {filepath.Join(links, "home"), src},
	} {
		code, _, stderr := runExport(t, "--src-home", c[0], "--rules", rules, c[1])
		if code != exitUsage || !strings.Contains(stderr, "must not contain") {
			t.Errorf("--src-home %s, OUT_DIR %s: code %d %q", c[0], c[1], code, stderr)
		}
		if read(t, filepath.Join(src, "work/CLAUDE.md")) != "# Work rules\n" || read(t, filepath.Join(rules, "manifest/skills.txt")) != "jira\n" {
			t.Fatalf("--src-home %s, OUT_DIR %s: источник или правила стёрты", c[0], c[1])
		}
	}
	// скилл — ссылка на папку вне home: OUT_DIR, где она лежит, стёр бы исходник скилла
	ext := filepath.Join(t.TempDir(), "ext")
	write(t, filepath.Join(ext, "jira2/SKILL.md"), "# jira2\n", 0o644)
	if err := os.Symlink(filepath.Join(ext, "jira2"), filepath.Join(src, ".claude/skills/jira2")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(rules, "manifest/skills.txt"), "jira\njira2\n", 0o644)
	if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, ext); code != exitUsage || read(t, filepath.Join(ext, "jira2/SKILL.md")) != "# jira2\n" {
		t.Fatalf("OUT_DIR с исходником скилла: code %d %q", code, stderr)
	}
	write(t, filepath.Join(rules, "manifest/skills.txt"), "jira\n", 0o644)
	if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, filepath.Join(src, "export")); code != 0 {
		t.Errorf("OUT_DIR внутри home, вне читаемого: code %d %q", code, stderr)
	}
	write(t, filepath.Join(rules, "manifest/pii.tsv"), "# пусто\n", 0o644)
	if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, filepath.Join(t.TempDir(), "o")); code != exitUsage || !strings.Contains(stderr, "no rules") {
		t.Errorf("пустой pii.tsv: code %d %q", code, stderr)
	}
	write(t, filepath.Join(rules, "manifest/replace.tsv"), "без табуляции\n", 0o644)
	if code, _, _ := runExport(t, "--src-home", src, "--rules", rules, filepath.Join(t.TempDir(), "o")); code != exitUsage {
		t.Errorf("кривая замена: code %d", code)
	}
}

// Читаемый файл или папка — ссылка вне home и правил: OUT_DIR с её целью стёр бы цель; ссылка внутри скилла — так же.
func TestExportRefusesLinkedReads(t *testing.T) {
	for _, c := range []struct{ name, link, file string }{
		{"global CLAUDE.md", "home/.claude/CLAUDE.md", "CLAUDE.md"},
		{"work CLAUDE.md", "home/work/CLAUDE.md", "CLAUDE.md"},
		{"manifest", "rules/manifest", "manifest/skills.txt"},
		{"manifest file", "rules/manifest/skills.txt", "skills.txt"},
		{"README template", "rules/README.template.md", "README.template.md"},
		{"link in a skill", "home/.claude/skills/jira/data", "data/f"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, rules := fixture(t)
			root := filepath.Dir(src)
			link := filepath.Join(root, c.link)
			target := filepath.Join(t.TempDir(), "target")
			moved := filepath.Join(target, filepath.Base(link))
			if _, err := os.Lstat(link); err == nil {
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(link, moved); err != nil {
					t.Fatal(err)
				}
			} else {
				write(t, filepath.Join(moved, "f"), "data\n", 0o644)
			}
			if err := os.Symlink(moved, link); err != nil {
				t.Fatal(err)
			}
			want := read(t, filepath.Join(target, c.file))
			code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, target)
			if code != exitUsage || !strings.Contains(stderr, "must not contain") {
				t.Errorf("code %d %q", code, stderr)
			}
			if got := read(t, filepath.Join(target, c.file)); got != want {
				t.Errorf("цель ссылки стёрта: %q", got)
			}
		})
	}
}

// Папка скилла — ссылка: в выгрузку идёт её содержимое, а не ссылка, висящая вне home.
func TestExportLinkedSkillCopiesContent(t *testing.T) {
	src, rules := fixture(t)
	ext := filepath.Join(t.TempDir(), "ext")
	write(t, filepath.Join(ext, "jira3/SKILL.md"), "# jira3\n", 0o644)
	if err := os.Symlink(filepath.Join(ext, "jira3"), filepath.Join(src, ".claude/skills/jira3")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(rules, "manifest/skills.txt"), "jira\njira3\n", 0o644)
	out := filepath.Join(t.TempDir(), "o")
	if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != 0 {
		t.Fatalf("code %d %q", code, stderr)
	}
	st, err := os.Lstat(filepath.Join(out, "skills/jira3"))
	if err != nil || !st.IsDir() {
		t.Fatalf("skills/jira3 не папка: %v %v", st, err)
	}
	if got := read(t, filepath.Join(out, "skills/jira3/SKILL.md")); got != "# jira3\n" {
		t.Errorf("SKILL.md: %q", got)
	}
}
