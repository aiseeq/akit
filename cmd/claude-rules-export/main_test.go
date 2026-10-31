package main

import (
	"bytes"
	"os"
	"os/exec"
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
	for _, args := range [][]string{{}, {"a", "b"}, {"--rules"}, {"--frob", "x"}, {""}, {"--src-home", "", "out"}, {"--rules", "", "out"}} {
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
	write(t, filepath.Join(out, "skills/old/SKILL.md"), "старое\n", 0o644)
	// висячая ссылка в скилле — не ошибка: выгружается ссылкой, как у cp -R
	if err := os.Symlink("gone", filepath.Join(src, ".claude/skills/jira/stale")); err != nil {
		t.Fatal(err)
	}
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
	if link, err := os.Readlink(filepath.Join(out, "skills/jira/stale")); err != nil || link != "gone" {
		t.Errorf("висячая ссылка: %q %v", link, err)
	}
	if got := read(t, filepath.Join(out, "skills/jira/logo.png")); !strings.Contains(got, "me@corp.example") {
		t.Error("двоичный файл тронут")
	}
	for _, p := range []string{"skills/secret", "skills/old"} {
		if _, err := os.Stat(filepath.Join(out, p)); err == nil {
			t.Errorf("лишнее в выгрузке: %s", p)
		}
	}
	if read(t, filepath.Join(out, ".git/HEAD")) != "ref\n" {
		t.Error(".git тронут")
	}
	// файл .git (worktree) — не выгрузка: замены его не трогают
	wt := filepath.Join(t.TempDir(), "wt")
	write(t, filepath.Join(wt, ".git"), "gitdir: /x/me@corp.example/wt\n", 0o644)
	if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, wt); code != 0 && code != exitDirty {
		t.Fatalf("worktree: code %d %q", code, stderr)
	}
	if got := read(t, filepath.Join(wt, ".git")); got != "gitdir: /x/me@corp.example/wt\n" {
		t.Errorf("файл .git тронут: %q", got)
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
	if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, ext); code != exitUsage || !strings.Contains(stderr, "must not contain") || read(t, filepath.Join(ext, "jira2/SKILL.md")) != "# jira2\n" {
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

// «..» после ссылки система берёт от цели ссылки, а filepath.Abs стёр бы ссылку по тексту: проверка и чтение идут
// по тому же пути, что очистка, — по пути, как его поймёт система.
func TestExportDotDotAfterLink(t *testing.T) {
	src, rules := fixture(t)
	links := t.TempDir()
	if err := os.Symlink(filepath.Join(src, ".claude"), filepath.Join(links, "claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(src, filepath.Join(links, "home")); err != nil {
		t.Fatal(err)
	}
	// по тексту links/rules — двойник правил; система читает настоящие правила рядом с home
	if err := copyTree(rules, filepath.Join(links, "rules")); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{
		{rules, filepath.Join(links, "claude") + "/.."},
		{filepath.Join(links, "home") + "/../rules", rules},
	} {
		code, _, stderr := runExport(t, "--src-home", src, "--rules", c[0], c[1])
		if code != exitUsage || !strings.Contains(stderr, "must not contain") {
			t.Errorf("--rules %s, OUT_DIR %s: code %d %q", c[0], c[1], code, stderr)
		}
		if read(t, filepath.Join(src, "work/CLAUDE.md")) != "# Work rules\n" || read(t, filepath.Join(rules, "manifest/skills.txt")) != "jira\n" {
			t.Fatalf("--rules %s, OUT_DIR %s: источник или правила стёрты", c[0], c[1])
		}
	}
}

// OUT_DIR — та же папка, показанная bind mount под другим путём: по тексту путей её не узнать, очистка стёрла бы
// источник. Монтирование — в своём пространстве имён (unshare -rm), где тест запускает сам себя.
func TestExportRefusesBindMount(t *testing.T) {
	if out := os.Getenv("CRE_BIND_OUT"); out != "" {
		// второй рубеж без mountinfo: та же папка под путём bind mount узнаётся сравнением самих папок
		if from := os.Getenv("CRE_BIND_SAME"); from != "" {
			if ok, err := within(out, from); !ok || err != nil {
				t.Errorf("within(%s, %s) = %v, %v", out, from, ok, err)
			}
		}
		code, _, stderr := runExport(t, "--src-home", os.Getenv("CRE_BIND_SRC"), "--rules", os.Getenv("CRE_BIND_RULES"), out)
		// отказ любой проверкой; что источник цел, смотрит запустивший тест
		if code != exitUsage {
			t.Fatalf("OUT_DIR %s: code %d %q", out, code, stderr)
		}
		return
	}
	if !userNamespaces() {
		t.Skip("no unshare or user namespaces are off: no mount --bind")
	}
	src, rules := fixture(t)
	write(t, filepath.Join(src, ".claude/hooks/h.sh"), "#!/bin/sh\n", 0o755)
	write(t, filepath.Join(rules, "extra/x"), "x\n", 0o644)
	mnt := t.TempDir()
	// чужая папка из одних имён выгрузки: проверка содержимого её пропустила бы
	repo := filepath.Join(t.TempDir(), "repo")
	write(t, filepath.Join(repo, "skills/z/f"), "z\n", 0o644)
	// $1 — что монтируется, $2 — папка случая; OUT_DIR — out внутри неё
	bind := `mkdir -p "$2/m" && mount --bind "$1" "$2/m"`
	casefold := `{ mkdir -p "$2/fs" && mount -t tmpfs -o casefold tmpfs "$2/fs" && mkdir "$2/fs/d" && chattr +F "$2/fs/d" || exit 77; }`
	for _, c := range []struct{ name, mount, from, out string }{
		{"home", bind, src, "m"},
		{"inside .claude", bind, filepath.Join(src, ".claude"), "m/x"},
		// подпапка читаемого под другим путём: у точки монтирования другие предки
		{"hooks", bind, filepath.Join(src, ".claude/hooks"), "m"},
		{"skill not exported", bind, filepath.Join(src, ".claude/skills/secret"), "m"},
		{"rules subfolder", bind, filepath.Join(rules, "extra"), "m"},
		// точка монтирования внутри OUT_DIR: очистка зашла бы в неё
		{"mount inside OUT_DIR", `mkdir -p "$2/o/skills/h" && mount --bind "$1" "$2/o/skills/h"`, filepath.Join(src, ".claude/hooks"), "o"},
		// overlay: источник — верхний слой, OUT_DIR — слитая папка; mountinfo пути к источнику не покажет
		{"overlay upper", `mkdir -p "$2/l" "$2/w" "$2/o" && mount -t overlay overlay -o lowerdir="$2/l",upperdir="$1",workdir="$2/w" "$2/o"`,
			filepath.Join(src, ".claude/hooks"), "o"},
		// OUT_DIR — сам точка монтирования чужой папки, где только имена выгрузки
		{"overlay of export-like folder", `mkdir -p "$2/l" "$2/w" "$2/o" && mount -t overlay overlay -o lowerdir="$2/l",upperdir="$1",workdir="$2/w" "$2/o"`,
			repo, "o"},
		// папка без учёта регистра: OUT_DIR в другом регистре, чем точка монтирования в mountinfo; код 77 —
		// ядро без casefold у tmpfs
		{"casefold mount inside OUT_DIR", casefold + ` && mkdir -p "$2/fs/d/out/skills" && mount --bind "$1" "$2/fs/d/out/skills"`,
			repo, "fs/d/OUT"},
		{"casefold OUT_DIR is a mount point", casefold + ` && mkdir -p "$2/fs/d/out" && mount --bind "$1" "$2/fs/d/out"`,
			repo, "fs/d/OUT"},
		// без /proc точек монтирования не узнать: отказ, а не очистка вслепую
		{"no /proc", `mkdir -p "$2/o/skills/h" && mount --bind "$1" "$2/o/skills/h" && mount -t tmpfs none /proc`,
			filepath.Join(src, ".claude/hooks"), "o"},
	} {
		dir := filepath.Join(mnt, c.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("unshare", "-rm", "sh", "-c", c.mount+` && exec "$3" -test.run '^TestExportRefusesBindMount$' -test.count=1`,
			"sh", c.from, dir, os.Args[0])
		cmd.Env = append(os.Environ(), "CRE_BIND_SRC="+src, "CRE_BIND_RULES="+rules, "CRE_BIND_OUT="+filepath.Join(dir, c.out))
		if c.mount == bind {
			cmd.Env = append(cmd.Env, "CRE_BIND_SAME="+c.from)
		}
		b, err := cmd.CombinedOutput()
		if code := cmd.ProcessState.ExitCode(); code == 77 {
			t.Logf("%s: no casefold tmpfs: %s", c.name, b)
		} else if err != nil {
			t.Errorf("%s: %v\n%s", c.name, err, b)
		}
		for _, f := range []string{"work/CLAUDE.md", ".claude/CLAUDE.md", ".claude/hooks/h.sh", ".claude/skills/secret/SKILL.md", "../rules/extra/x", repo + "/skills/z/f"} {
			if !filepath.IsAbs(f) {
				f = filepath.Join(src, f)
			}
			if _, err := os.Stat(f); err != nil {
				t.Fatalf("%s: источник стёрт: %v", c.name, err)
			}
		}
	}
}

// userNamespaces — среда даёт своё пространство имён с монтированием: без него bind mount в тесте не сделать.
// Пробный запуск, а не настройка ядра: во вложенной песочнице unshare отказывает и при разрешённых пространствах.
func userNamespaces() bool {
	return exec.Command("unshare", "-rm", "true").Run() == nil
}

// Очищается только то, что пишет выгрузка: чужое в OUT_DIR — отказ до очистки, файл цел.
func TestExportRefusesForeignContent(t *testing.T) {
	src, rules := fixture(t)
	out := filepath.Join(t.TempDir(), "out")
	write(t, filepath.Join(out, "skills/x/SKILL.md"), "# x\n", 0o644)
	write(t, filepath.Join(out, "notes.md"), "своё\n", 0o644)
	code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out)
	if code != exitUsage || !strings.Contains(stderr, "OUT_DIR holds notes.md") {
		t.Errorf("code %d %q", code, stderr)
	}
	if read(t, filepath.Join(out, "notes.md")) != "своё\n" || read(t, filepath.Join(out, "skills/x/SKILL.md")) != "# x\n" {
		t.Error("OUT_DIR тронут до отказа")
	}
}
