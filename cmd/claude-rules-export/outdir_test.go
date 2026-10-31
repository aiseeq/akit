package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
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
		{"setuid", func(t *testing.T, out string) {
			if err := os.Chmod(filepath.Join(out, "skills/jira/run.sh"), 0o755|os.ModeSetuid); err != nil {
				t.Fatal(err)
			}
		}, "skills/jira/run.sh"},
		{"sticky", func(t *testing.T, out string) {
			if err := os.Chmod(filepath.Join(out, "work/CLAUDE.md"), 0o644|os.ModeSticky); err != nil {
				t.Fatal(err)
			}
		}, "work/CLAUDE.md"},
		{"folder mode", func(t *testing.T, out string) {
			if err := os.Chmod(filepath.Join(out, "skills/jira"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, "skills/jira"},
		{"xattr", func(t *testing.T, out string) {
			if err := syscall.Setxattr(filepath.Join(out, "skills/jira/SKILL.md"), "user.note", []byte("своё"), 0); errors.Is(err, syscall.ENOTSUP) {
				t.Skip("no user xattrs on this file system")
			} else if err != nil {
				t.Fatal(err)
			}
		}, "skills/jira/SKILL.md"},
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

// Записи прерванного прогона без inode в журнале: файл на их месте выгрузка не создавала (файл получает имя только
// после записи inode), ссылку держит её текст.
func TestExportRefusesChangedPending(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(t *testing.T, out string)
	}{
		{"file with the same content", func(*testing.T, string) {}},
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
	}{{"removed", 2}, {"cleared", 1}, {"created", 1}, {"created", 3}, {"journaled", 1}, {"journaled", 4}, {"wrote", 1}, {"wrote", 4}} {
		t.Run(stop.step, func(t *testing.T) {
			src, rules := fixture(t)
			out := filepath.Join(t.TempDir(), "out")
			write(t, filepath.Join(out, ".git/HEAD"), "ref\n", 0o644)
			if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != 0 {
				t.Fatalf("first run: code %d %q", code, stderr)
			}
			write(t, filepath.Join(src, ".claude/skills/jira/run.sh"), "#!/bin/sh\necho new\n", 0o755)
			list, err := listFile(out)
			if err != nil {
				t.Fatal(err)
			}
			seen := 0
			transferStep = func(step string) error {
				if step == "intended" { // случайное имя — в журнале до создания
					logged, err := loadJournal(journalFile(list))
					if err != nil || len(logged) == 0 {
						t.Fatalf("journal: %v %v", logged, err)
					}
					if _, err := os.Lstat(filepath.Join(out, logged[len(logged)-1].Path)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("%s created before its journal line: %v", logged[len(logged)-1].Path, err)
					}
				}
				if step == "journaled" { // файл из последней строки журнала ещё без имени
					logged, err := loadJournal(journalFile(list))
					if err != nil || len(logged) == 0 {
						t.Fatalf("journal: %v %v", logged, err)
					}
					if _, err := os.Lstat(filepath.Join(out, logged[len(logged)-1].Path)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("%s named before its journal line: %v", logged[len(logged)-1].Path, err)
					}
				}
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
			// после очистки список пуст: новое — только в журнале, обрывы подряд список не растят
			if entries, err := loadList(list, out); err != nil || stop.step != "removed" && len(entries) != 0 {
				t.Errorf("list after the cut: %d entries, %v", len(entries), err)
			}
			if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != 0 {
				t.Fatalf("resumed run: code %d %q", code, stderr)
			}
			if read(t, filepath.Join(out, "skills/jira/run.sh")) != "#!/bin/sh\necho new\n" || read(t, filepath.Join(out, ".git/HEAD")) != "ref\n" {
				t.Error("resumed export incomplete or .git changed")
			}
			if s := snapshot(t, out); strings.Contains(s, tempPrefix) {
				t.Errorf("temporary name left:\n%s", s)
			}
		})
	}
}

// Папка, которую создал оборванный прогон, сверяется по журналу: её права, сменённые после обрыва, — отказ.
func TestExportRefusesChangedJournaled(t *testing.T) {
	src, rules := fixture(t)
	out := filepath.Join(t.TempDir(), "out")
	seen := 0
	transferStep = func(step string) error {
		if step == "wrote" {
			if seen++; seen == 2 { // README.md, затем папка global
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
	if err := os.Chmod(filepath.Join(out, "global"), 0o700); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, out)
	if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != exitUsage || !strings.Contains(stderr, "global") {
		t.Errorf("code %d %q", code, stderr)
	}
	if snapshot(t, out) != before {
		t.Error("OUT_DIR changed")
	}
}

// После обрыва пользователь создаёт или меняет папку и ссылку на месте записей выгрузки: следующий прогон
// отказывает, их не трогает.
func TestExportKeepsUserEntriesAfterCut(t *testing.T) {
	for _, c := range []struct {
		name, step string
		n          int
		change     func(t *testing.T, out string)
		want       string
	}{
		{"own folder after clearing", "cleared", 1, func(t *testing.T, out string) {
			if err := os.Mkdir(filepath.Join(out, "work"), 0o750); err != nil {
				t.Fatal(err)
			}
		}, "work"},
		{"old folder chmod mid-clearing", "removed", 1, func(t *testing.T, out string) {
			if err := os.Chmod(filepath.Join(out, "skills/jira"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, "skills/jira"},
		// шестая запись — ссылка skills/jira/README.md: под своим именем её ещё нет
		{"own link before naming", "journaled", 6, func(t *testing.T, out string) {
			if err := os.Symlink("SKILL.md", filepath.Join(out, "skills/jira/README.md")); err != nil {
				t.Fatal(err)
			}
		}, "skills/jira/README.md"},
		// вторая запись — папка global: строка с inode её случайного имени уже в журнале, переименования нет
		{"temp folder chmod after its line", "journaled", 2, func(t *testing.T, out string) {
			tmp, err := filepath.Glob(filepath.Join(out, tempPrefix+"*"))
			if err != nil || len(tmp) != 1 {
				t.Fatalf("temp names %v %v", tmp, err)
			}
			if err := os.Chmod(tmp[0], 0o750); err != nil {
				t.Fatal(err)
			}
		}, tempPrefix},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, rules := fixture(t)
			out := filepath.Join(t.TempDir(), "out")
			if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != 0 {
				t.Fatalf("first run: code %d %q", code, stderr)
			}
			seen := 0
			transferStep = func(step string) error {
				if step == c.step {
					if seen++; seen == c.n {
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
			c.change(t, out)
			before := snapshot(t, out)
			if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != exitUsage || !strings.Contains(stderr, c.want) {
				t.Errorf("code %d %q", code, stderr)
			}
			if snapshot(t, out) != before {
				t.Error("OUT_DIR changed")
			}
		})
	}
}

// Запись с тем же inode на другом устройстве или с другим временем рождения (inode отдан заново) — другой файл:
// отказ.
func TestExportRefusesOtherIdentity(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(e *entry)
	}{
		{"device", func(e *entry) { e.Dev++ }},
		{"birth", func(e *entry) { e.Birth++ }},
		// у root владелец и группа и так 0: случай ничего бы не менял
		{"owner", func(e *entry) { e.Uid = 0 }},
		{"group", func(e *entry) { e.Gid = 0 }},
		// у ссылки xattr — метка SELinux; без неё на этой системе случай пропускается
		{"link xattr", func(e *entry) {
			if e.Kind == "l" {
				e.Xattr = ""
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
			if c.name == "birth" && entries[0].Birth == 0 {
				t.Skip("no birth time on this file system")
			}
			if (c.name == "owner" || c.name == "group") && (os.Getuid() == 0 || os.Getgid() == 0) {
				t.Skip("root: owner and group are already 0")
			}
			if c.name == "link xattr" {
				n, err := unix.Llistxattr(filepath.Join(out, "skills/jira/README.md"), nil)
				if err != nil {
					t.Fatal(err)
				}
				if n == 0 {
					t.Skip("no xattrs on links on this file system")
				}
			}
			for i := range entries {
				c.change(&entries[i])
			}
			if err := saveList(file, out, entries); err != nil {
				t.Fatal(err)
			}
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

// OUT_DIR без O_TMPFILE — отказ до очистки: прошлая выгрузка цела.
func TestExportRefusesWithoutUnnamedFiles(t *testing.T) {
	src, rules := fixture(t)
	out := filepath.Join(t.TempDir(), "out")
	if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != 0 {
		t.Fatalf("first run: code %d %q", code, stderr)
	}
	before := snapshot(t, out)
	saved := openUnnamed
	openUnnamed = func(int) (int, error) { return -1, syscall.EOPNOTSUPP }
	code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out)
	openUnnamed = saved
	if code != exitUsage || !strings.Contains(stderr, "O_TMPFILE") {
		t.Errorf("code %d %q", code, stderr)
	}
	if snapshot(t, out) != before {
		t.Error("OUT_DIR changed")
	}
}

// OUT_DIR без времени рождения — отказ до очистки: своя копия с повторённым inode не сойдёт за запись выгрузки.
func TestExportRefusesWithoutBirthTime(t *testing.T) {
	src, rules := fixture(t)
	out := filepath.Join(t.TempDir(), "out")
	if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != 0 {
		t.Fatalf("first run: code %d %q", code, stderr)
	}
	before := snapshot(t, out)
	saved := statx
	statx = func(dirfd int, path string, flags, mask int, st *unix.Statx_t) error {
		err := saved(dirfd, path, flags, mask, st)
		st.Mask &^= unix.STATX_BTIME
		return err
	}
	code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out)
	statx = saved
	if code != exitUsage || !strings.Contains(stderr, "birth time") {
		t.Errorf("code %d %q", code, stderr)
	}
	if snapshot(t, out) != before {
		t.Error("OUT_DIR changed")
	}
}

// Сборка оборванного прогона убирается следующим.
func TestExportRemovesOldStage(t *testing.T) {
	src, rules := fixture(t)
	out := filepath.Join(t.TempDir(), "out")
	stage, err := stageDir(out)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(stage, "skills/big/f"), "left\n", 0o644)
	if code, _, stderr := runExport(t, "--src-home", src, "--rules", rules, out); code != 0 {
		t.Fatalf("code %d %q", code, stderr)
	}
	if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stage left: %v", err)
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

// snapshot — пути, права, inode, xattr, содержимое и цели ссылок папки одной строкой.
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
		b.WriteString(p + " " + st.Mode().String() + " " + strconv.FormatUint(st.Sys().(*syscall.Stat_t).Ino, 10))
		if st.Mode()&os.ModeSymlink == 0 {
			b.WriteString(" " + xattrs(t, p))
		}
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

func xattrs(t *testing.T, p string) string {
	t.Helper()
	buf := make([]byte, 4096)
	n, err := syscall.Listxattr(p, buf)
	if errors.Is(err, syscall.ENOTSUP) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Split(strings.TrimSuffix(string(buf[:n]), "\x00"), "\x00")
	for i, name := range names {
		v := make([]byte, 4096)
		m, err := syscall.Getxattr(p, name, v)
		if err != nil {
			t.Fatal(err)
		}
		names[i] = name + "=" + string(v[:m])
	}
	return strings.Join(names, ",")
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

// xattr ссылки читаются без перехода по ней и совпадают с прочитанными по пути (llistxattr).
func TestLinkXattrMatchesPath(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "l")
	if err := os.Symlink("missing", link); err != nil {
		t.Fatal(err)
	}
	n, err := unix.Llistxattr(link, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Skip("no xattrs on links on this file system")
	}
	want, err := xattrSum(func(b []byte) (int, error) { return unix.Llistxattr(link, b) },
		func(name string, b []byte) (int, error) { return unix.Lgetxattr(link, name, b) })
	if err != nil {
		t.Fatal(err)
	}
	d, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	got, err := linkXattr(int(d.Fd()), "l")
	if err != nil || got != want || got == "" {
		t.Errorf("linkXattr = %q, %v; want %q", got, err, want)
	}
}
