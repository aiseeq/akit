package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Повторный прогон удаляет только записи прошлого, которые не менялись: любая правка, чужой файл или копия того же
// содержимого — отказ до очистки, OUT_DIR цел.
func TestExportRefusesChangedExport(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(t *testing.T, out string)
		want   string
	}{
		{"content", func(t *testing.T, out string) {
			write(t, filepath.Join(out, "skills/jira/run.sh"), "#!/bin/sh\necho changed\n", 0o755)
		}, "skills/jira/run.sh"},
		{"content, same size", func(t *testing.T, out string) {
			write(t, filepath.Join(out, "skills/jira/run.sh"), "#!/bin/sh\necho ko\n", 0o755)
		}, "skills/jira/run.sh"},
		{".git inside a skill", func(t *testing.T, out string) {
			write(t, filepath.Join(out, "skills/jira/.git/HEAD"), "ref\n", 0o644)
		}, "skills/jira/.git"},
		{"mode", func(t *testing.T, out string) {
			if err := os.Chmod(filepath.Join(out, "skills/jira/run.sh"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, "skills/jira/run.sh"},
		{"foreign file", func(t *testing.T, out string) {
			write(t, filepath.Join(out, "skills/jira/notes.md"), "своё\n", 0o644)
		}, "skills/jira/notes.md"},
		{"same content, other file", func(t *testing.T, out string) {
			p := filepath.Join(out, "work/CLAUDE.md")
			data := read(t, p)
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			write(t, p, data, 0o644)
		}, "work/CLAUDE.md"},
		{"link retargeted", func(t *testing.T, out string) {
			p := filepath.Join(out, "skills/jira/README.md")
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("run.sh", p); err != nil {
				t.Fatal(err)
			}
		}, "skills/jira/README.md"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, rules := fixture(t)
			out := filepath.Join(t.TempDir(), "out")
			if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != 0 {
				t.Fatalf("first run: code %d %q", code, stderr)
			}
			c.change(t, out)
			before := snapshot(t, out)
			code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out)
			if code != exitUsage || !strings.Contains(stderr, c.want) {
				t.Errorf("code %d %q", code, stderr)
			}
			if after := snapshot(t, out); after != before {
				t.Errorf("OUT_DIR changed:\n%s\n---\n%s", before, after)
			}
		})
	}
}

// Записи прерванного прогона — без inode: их держат хэш и цель ссылки.
func TestExportRefusesChangedPending(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(t *testing.T, out string)
	}{
		{"content, same size", func(t *testing.T, out string) {
			write(t, filepath.Join(out, "skills/jira/run.sh"), "#!/bin/sh\necho ko\n", 0o755)
		}},
		{"link retargeted", func(t *testing.T, out string) {
			p := filepath.Join(out, "skills/jira/README.md")
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("run.sh", p); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, rules := fixture(t)
			out := filepath.Join(t.TempDir(), "out")
			if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != 0 {
				t.Fatalf("first run: code %d %q", code, stderr)
			}
			file, err := listFile(out)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := loadList(file, out)
			if err != nil {
				t.Fatal(err)
			}
			for i := range entries {
				entries[i].Ino = 0
			}
			if err := saveList(file, out, entries); err != nil {
				t.Fatal(err)
			}
			c.change(t, out)
			before := snapshot(t, out)
			if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != exitUsage {
				t.Errorf("code %d %q", code, stderr)
			}
			if snapshot(t, out) != before {
				t.Error("OUT_DIR changed")
			}
		})
	}
}

// Список, записанный для другой папки, — отказ: файл списка не подходит к этому OUT_DIR.
func TestExportListForOtherFolder(t *testing.T) {
	src, rules := fixture(t)
	out := filepath.Join(t.TempDir(), "out")
	if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != 0 {
		t.Fatalf("first run: code %d %q", code, stderr)
	}
	file, err := listFile(out)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := loadList(file, out)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveList(file, out+"-other", entries); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != exitUsage || !strings.Contains(stderr, "is for") {
		t.Errorf("code %d %q", code, stderr)
	}
}

// Записи, которой уже нет (удалили руками или прошлый прогон оборвался посреди очистки), — не ошибка.
func TestExportMissingEntry(t *testing.T) {
	src, rules := fixture(t)
	out := filepath.Join(t.TempDir(), "out")
	if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != 0 {
		t.Fatalf("first run: code %d %q", code, stderr)
	}
	if err := os.Remove(filepath.Join(out, "skills/jira/run.sh")); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != 0 {
		t.Fatalf("second run: code %d %q", code, stderr)
	}
	if read(t, filepath.Join(out, "skills/jira/run.sh")) != "#!/bin/sh\necho ok\n" {
		t.Error("run.sh not restored")
	}
}

// Прогон, оборванный после очистки или посреди переноса, следующий доводит без ручной работы; своё пользователя
// рядом с выгрузкой (.git) цело.
func TestExportResumesInterrupted(t *testing.T) {
	for _, stop := range []struct {
		step string
		n    int
	}{{"cleared", 1}, {"wrote", 1}, {"wrote", 4}} {
		t.Run(stop.step, func(t *testing.T) {
			src, rules := fixture(t)
			out := filepath.Join(t.TempDir(), "out")
			write(t, filepath.Join(out, ".git/HEAD"), "ref\n", 0o644)
			if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != 0 {
				t.Fatalf("first run: code %d %q", code, stderr)
			}
			write(t, filepath.Join(src, ".claude/skills/jira/run.sh"), "#!/bin/sh\necho new\n", 0o755)
			seen := 0
			transferStep = func(step string) error {
				if step == stop.step {
					if seen++; seen == stop.n {
						return errors.New("interrupted")
					}
				}
				return nil
			}
			code, _, _ := runExport(t, "--src-home", src, "--rules", rules, out)
			transferStep = nil
			if code == 0 {
				t.Fatal("interrupted run: code 0")
			}
			if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != 0 {
				t.Fatalf("resumed run: code %d %q", code, stderr)
			}
			if read(t, filepath.Join(out, "skills/jira/run.sh")) != "#!/bin/sh\necho new\n" || read(t, filepath.Join(out, ".git/HEAD")) != "ref\n" {
				t.Error("resumed export incomplete or .git changed")
			}
		})
	}
}

// Путь из списка вне OUT_DIR или в его .git — отказ, внешний файл цел.
func TestExportListPathOutside(t *testing.T) {
	for _, bad := range []string{"../notes.txt", "/etc/passwd", "a/../../notes.txt", ".git/config", ".GIT/config", "."} {
		t.Run(bad, func(t *testing.T) {
			src, rules := fixture(t)
			dir := t.TempDir()
			out := filepath.Join(dir, "out")
			write(t, filepath.Join(dir, "notes.txt"), "своё\n", 0o644)
			if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != 0 {
				t.Fatalf("first run: code %d %q", code, stderr)
			}
			file, err := listFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if err := saveList(file, out, []entry{{Kind: "f", Path: bad, Mode: 0o644}}); err != nil {
				t.Fatal(err)
			}
			if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != exitUsage || !strings.Contains(stderr, "export path") {
				t.Errorf("code %d %q", code, stderr)
			}
			if read(t, filepath.Join(dir, "notes.txt")) != "своё\n" {
				t.Error("file outside OUT_DIR changed")
			}
		})
	}
}

// Скилл .git и пути, равные без учёта регистра, — ошибка сборки до касания OUT_DIR.
func TestExportRefusesUnsafeNames(t *testing.T) {
	for _, c := range []struct{ name, skills, want string }{
		{".git skill", "jira\n.GIT\n", "invalid skill name"},
		{"case collision", "jira\nJira\n", "differ only in case"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, rules := fixture(t)
			write(t, filepath.Join(src, ".claude/skills/.GIT/SKILL.md"), "x\n", 0o644)
			write(t, filepath.Join(src, ".claude/skills/Jira/SKILL.md"), "x\n", 0o644)
			write(t, filepath.Join(rules, "manifest/skills.txt"), c.skills, 0o644)
			out := filepath.Join(t.TempDir(), "out")
			write(t, filepath.Join(out, ".git/HEAD"), "ref\n", 0o644)
			code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out)
			if code != exitUsage || !strings.Contains(stderr, c.want) {
				t.Errorf("code %d %q", code, stderr)
			}
			if entries, err := os.ReadDir(out); err != nil || len(entries) != 1 {
				t.Errorf("OUT_DIR touched: %v %v", entries, err)
			}
		})
	}
}

// snapshot — пути, права, содержимое и цели ссылок папки одной строкой.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		b.WriteString(p + " " + st.Mode().String())
		switch {
		case st.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			b.WriteString(" -> " + l)
		case st.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			b.WriteString(" " + string(data))
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Обычный OUT_DIR — папка внутри ~/work, рядом с выгружаемым work/CLAUDE.md: два прогона подряд проходят, источник цел.
func TestExportIntoWorkFolder(t *testing.T) {
	src, rules := fixture(t)
	out := filepath.Join(src, "work", "claude-rules")
	write(t, filepath.Join(out, ".git/HEAD"), "ref\n", 0o644)
	for run := 1; run <= 2; run++ {
		if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != 0 {
			t.Fatalf("run %d: code %d %q", run, code, stderr)
		}
	}
	if read(t, filepath.Join(src, "work/CLAUDE.md")) != "# Work rules\n" || read(t, filepath.Join(out, "work/CLAUDE.md")) != "# Work rules\n" {
		t.Error("source or export wrong")
	}
}
