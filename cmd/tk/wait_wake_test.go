package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWaitRequiresLeadingDateAndRejectsOwnQueue(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, dir := testRunMainRoot(t)
	path := writeRunMainTask(t, dir, "20261124-1200-MED-current.md", "# Current\n\nСтатус: открыт\n\n## Приёмка\n- проверка\n\n## Журнал\n")
	for _, until := range []string{"ответ Юры", "очередь после G3", "после current"} {
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var out, errout bytes.Buffer
		code := runMain([]string{"wait", "current", "--until", until, "--by", "tester"}, time.Now(), &out, &errout)
		wantHint := "ГГГГ-ММ-ДД ЧЧ:ММ"
		if strings.Contains(until, "очередь") || strings.Contains(until, "после current") {
			wantHint = "после своего тикета — `tk set <ID> --blocks <X>`; своя очередь — оставь открытым"
		}
		if code == 0 || !strings.Contains(errout.String(), wantHint) {
			t.Errorf("wait %q: code=%d stderr=%q", until, code, errout.String())
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Errorf("refused wait modified ticket: %v", err)
		}
	}
	var out, errout bytes.Buffer
	code := runMain([]string{"wait", "current", "--until", "2026-11-30 12:00: ответ Юры", "--by", "tester"}, time.Date(2026, 11, 24, 12, 0, 0, 0, time.Local), &out, &errout)
	if code != 0 {
		t.Fatalf("dated external wait: code=%d stderr=%q", code, errout.String())
	}
	writeRunMainTask(t, dir, "20261124-1300-MED-external.md", "# External\n\nСтатус: открыт\n\n## Приёмка\n- проверка\n\n## Журнал\n")
	errout.Reset()
	code = runMain([]string{"wait", "external", "--until", "2026-11-30 12:00: тикет graft X", "--by", "tester"}, time.Date(2026, 11, 24, 12, 0, 0, 0, time.Local), &out, &errout)
	if code != 0 {
		t.Fatalf("dated named external wait: code=%d stderr=%q", code, errout.String())
	}
}

func TestNextWakesOnClosedLocalReferencesAndDueListsUndatedWait(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, dir := testRunMainRoot(t)
	writeRunMainTask(t, dir, "20261120-1000-MED-blocker.md", "# Blocker\n\nСтатус: готово\n")
	writeRunMainTask(t, dir, "20261120-1100-MED-mentioned.md", "# Mentioned\n\nСтатус: отклонён\n")
	writeRunMainTask(t, dir, "20261120-1200-MED-by-blocker.md", "# By blocker\n\nСтатус: ждёт\nЗаблокирован: blocker\nЖдёт: ответ от blocker\n")
	writeRunMainTask(t, dir, "20261120-1300-MED-by-text.md", "# By text\n\nСтатус: ждёт\nЖдёт: после mentioned\n")
	writeRunMainTask(t, dir, "20261120-1400-MED-undated.md", "# Undated\n\nСтатус: ждёт\nЖдёт: ответ Юры\n")
	var nextOut, nextErr bytes.Buffer
	if code := runMain([]string{"next", "--by", "tester"}, time.Now(), &nextOut, &nextErr); code != 0 || !strings.Contains(nextOut.String(), "by-blocker") || !strings.Contains(nextOut.String(), "условие наступило") {
		t.Fatalf("next local closed blocker: code=%d out=%q err=%q", code, nextOut.String(), nextErr.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "20261120-1200-MED-by-blocker.md"), []byte("# By blocker\n\nСтатус: готово\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nextOut.Reset()
	nextErr.Reset()
	if code := runMain([]string{"next", "--by", "tester"}, time.Now(), &nextOut, &nextErr); code != 0 || !strings.Contains(nextOut.String(), "by-text") || !strings.Contains(nextOut.String(), "условие наступило") {
		t.Fatalf("next local closed mentioned ticket: code=%d out=%q err=%q", code, nextOut.String(), nextErr.String())
	}
	var dueOut, dueErr bytes.Buffer
	if code := runMain([]string{"ls", "--due"}, time.Now(), &dueOut, &dueErr); code != 0 || !strings.Contains(dueOut.String(), "undated") || !strings.Contains(dueOut.String(), "требует даты") {
		t.Fatalf("ls --due undated wait: code=%d out=%q err=%q", code, dueOut.String(), dueErr.String())
	}
}

func TestNextWakesOnClosedCrossProjectWaitReference(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "git", "sample")
	dir := filepath.Join(project, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	other := filepath.Join(home, "git", "other", "docs", "tasks")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRunMainTask(t, other, "20261120-1000-MED-cross.md", "# Cross\n\nСтатус: готово\n")
	blocked := writeRunMainTask(t, dir, "20261120-1100-MED-cross-blocked.md", "# Cross blocked\n\nСтатус: ждёт\nЗаблокирован: git/other/cross\nЖдёт: результат\n")
	first := writeRunMainTask(t, dir, "20261120-1200-MED-waiting.md", "# Waiting\n\nСтатус: ждёт\nЖдёт: git/other/cross\n")
	writeRunMainTask(t, dir, "20261120-1300-MED-waiting-alias.md", "# Waiting alias\n\nСтатус: ждёт\nЖдёт: other/cross\n")
	var out, errout bytes.Buffer
	if code := runMain([]string{"next", "--by", "tester"}, time.Now(), &out, &errout); code != 0 || !strings.Contains(out.String(), "cross-blocked") || !strings.Contains(out.String(), "условие наступило") {
		t.Fatalf("next cross-project blocker: code=%d out=%q err=%q", code, out.String(), errout.String())
	}
	if err := os.WriteFile(blocked, []byte("# Cross blocked\n\nСтатус: готово\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errout.Reset()
	if code := runMain([]string{"next", "--by", "tester"}, time.Now(), &out, &errout); code != 0 || !strings.Contains(out.String(), "waiting") || !strings.Contains(out.String(), "условие наступило") {
		t.Fatalf("next cross-project wait reference: code=%d out=%q err=%q", code, out.String(), errout.String())
	}
	if err := os.WriteFile(first, []byte("# Waiting\n\nСтатус: готово\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errout.Reset()
	if code := runMain([]string{"next", "--by", "tester"}, time.Now(), &out, &errout); code != 0 || !strings.Contains(out.String(), "waiting-alias") || !strings.Contains(out.String(), "условие наступило") {
		t.Fatalf("next short cross project reference: code=%d out=%q err=%q", code, out.String(), errout.String())
	}
}
