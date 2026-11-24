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
	for _, until := range []string{"ответ Юры", "ответ Юры к 12:3x", "после current", "за current", "после 20261124-1200-MED-current", "очередь после current", "своя очередь"} {
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var out, errout bytes.Buffer
		code := runMain([]string{"wait", "current", "--until", until, "--by", "tester"}, time.Now(), &out, &errout)
		wantHint := "ГГГГ-ММ-ДД ЧЧ:ММ"
		if waitOwnQueueRE.MatchString(until) || waitOwnTicketRE.MatchString(until) || waitQueueAfterRE.MatchString(until) {
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
	for _, until := range []string{"2026-11-30 12:00: после current", "2026-11-30 12:00: после 20261124-1200-MED-current", "2026-11-30 12:00: очередь после current"} {
		var out, errout bytes.Buffer
		code := runMain([]string{"wait", "current", "--until", until, "--by", "tester"}, time.Now(), &out, &errout)
		if code == 0 || !strings.Contains(errout.String(), "после своего тикета") {
			t.Errorf("dated own-ticket wait was accepted: %q code=%d stderr=%q", until, code, errout.String())
		}
	}
	var out, errout bytes.Buffer
	code := runMain([]string{"wait", "current", "--until", "2026-11-30 12:00: ответ Юры", "--by", "tester"}, time.Date(2026, 11, 24, 12, 0, 0, 0, time.Local), &out, &errout)
	if code != 0 {
		t.Fatalf("dated external wait: code=%d stderr=%q", code, errout.String())
	}
	errout.Reset()
	code = runMain([]string{"wait", "current", "--until", "2026-11-18 10:00: выкатка", "--by", "tester"}, time.Date(2026, 11, 24, 12, 0, 0, 0, time.Local), &out, &errout)
	if code != 0 {
		t.Fatalf("dated deploy wait: code=%d stderr=%q", code, errout.String())
	}
	writeRunMainTask(t, dir, "20261124-1300-MED-external.md", "# External\n\nСтатус: открыт\n\n## Приёмка\n- проверка\n\n## Журнал\n")
	writeRunMainTask(t, dir, "20261124-1310-MED-git.md", "# Git\n\nСтатус: открыт\n")
	errout.Reset()
	code = runMain([]string{"wait", "external", "--until", "2026-11-30 12:00: тикет graft X", "--by", "tester"}, time.Date(2026, 11, 24, 12, 0, 0, 0, time.Local), &out, &errout)
	if code != 0 {
		t.Fatalf("dated named external wait: code=%d stderr=%q", code, errout.String())
	}
	errout.Reset()
	code = runMain([]string{"wait", "external", "--until", "2026-11-30 12:00: после git/other/cross", "--by", "tester"}, time.Date(2026, 11, 24, 12, 0, 0, 0, time.Local), &out, &errout)
	if code != 0 {
		t.Fatalf("cross-project wait was mistaken for local git ticket: code=%d stderr=%q", code, errout.String())
	}
	for _, until := range []string{"2026-11-30 12:00: в очереди sample впереди MED старше", "2026-11-30 12:00: (state.json очереди)", "2026-11-30 12:00: ничья очереди газа", "2026-11-30 12:00: очередь выкатки graft", "2026-11-30 12:00: ответ Юры, он в очереди", "2026-11-30 12:00: показа demo"} {
		errout.Reset()
		code = runMain([]string{"wait", "external", "--until", until, "--by", "tester"}, time.Date(2026, 11, 24, 12, 0, 0, 0, time.Local), &out, &errout)
		if code != 0 {
			t.Errorf("wait falsely rejected %q: code=%d stderr=%q", until, code, errout.String())
		}
	}
}

func TestNextWakesOnClosedLocalReferencesAndDueListsUndatedWait(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, dir := testRunMainRoot(t)
	writeRunMainTask(t, dir, "20261120-1000-MED-blocker.md", "# Blocker\n\nСтатус: готово\n")
	writeRunMainTask(t, dir, "20261120-1100-MED-mentioned.md", "# Mentioned\n\nСтатус: отклонён\n")
	writeRunMainTask(t, dir, "20261120-1030-MED-arch-tick-p1.md", "# Arch tick p1\n\nСтатус: готово\n")
	writeRunMainTask(t, dir, "20261120-1035-MED-arch-tick.md", "# Arch tick parent\n\nСтатус: готово\n")
	writeRunMainTask(t, dir, "20261120-1040-MED-blk.md", "# Blk\n\nСтатус: готово\n")
	writeRunMainTask(t, dir, "20261120-1050-MED-state.md", "# State\n\nСтатус: готово\n")
	writeRunMainTask(t, dir, "20261120-1055-MED-release.md", "# Release\n\nСтатус: готово\n")
	writeRunMainTask(t, dir, "20261120-1110-MED-openref.md", "# Open ref\n\nСтатус: открыт\n")
	writeRunMainTask(t, dir, "20261120-1120-MED-progressref.md", "# Progress ref\n\nСтатус: в работе\n")
	writeRunMainTask(t, dir, "20261120-1200-MED-by-blocker.md", "# By blocker\n\nСтатус: ждёт\nЗаблокирован: blocker\nЖдёт: ответ от blocker\n")
	writeRunMainTask(t, dir, "20261120-1300-MED-by-text.md", "# By text\n\nСтатус: ждёт\nЖдёт: после mentioned\n")
	writeRunMainTask(t, dir, "20261120-1310-MED-by-id.md", "# By ID\n\nСтатус: ждёт\nЖдёт: после 20261120-1100-MED-mentioned\n")
	writeRunMainTask(t, dir, "20261120-1400-MED-undated.md", "# Undated\n\nСтатус: ждёт\nЖдёт: ответ Юры\n")
	writeRunMainTask(t, dir, "20261120-1500-MED-openrefwait.md", "# Open ref wait\n\nСтатус: ждёт\nЖдёт: после openref\n")
	writeRunMainTask(t, dir, "20261120-1510-MED-progressrefwait.md", "# Progress ref wait\n\nСтатус: ждёт\nЖдёт: после progressref\n")
	writeRunMainTask(t, dir, "20261120-1600-MED-multiref.md", "# Multi ref\n\nСтатус: ждёт\nЖдёт: после mentioned и openref\n")
	writeRunMainTask(t, dir, "20261120-1700-MED-false-match.md", "# False match\n\nСтатус: ждёт\nЖдёт: binary-trace README п. 263; arch-tick p1; blk_extra; state.json; release graft\n")
	archive := filepath.Join(filepath.Dir(dir), "tasks-archive")
	if err := os.MkdirAll(archive, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRunMainTask(t, archive, "20261120-1800-MED-archived.md", "# Archived\n\nСтатус: готово\n")
	writeRunMainTask(t, archive, "20261120-1810-MED-readme.md", "# README\n\nСтатус: готово\n")
	writeRunMainTask(t, dir, "20261120-1900-MED-archivewait.md", "# Archive wait\n\nСтатус: ждёт\nЖдёт: после archived\n")
	var nextOut, nextErr bytes.Buffer
	if code := runMain([]string{"next", "--by", "tester"}, time.Now(), &nextOut, &nextErr); code != 0 || !strings.Contains(nextOut.String(), "by-blocker") || !strings.Contains(nextOut.String(), "условие наступило") {
		t.Fatalf("next local closed blocker: code=%d out=%q err=%q", code, nextOut.String(), nextErr.String())
	}
	loaded, err := loadTasks(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	idx := newWaitReferenceIndex()
	idx.addTasks(loaded)
	for _, task := range loaded {
		task.waitIndex = idx
		switch task.slug {
		case "by-blocker", "by-text", "by-id", "archivewait":
			if !waitConditionReached(task) {
				t.Errorf("closed reference did not wake %s", task.slug)
			}
		case "openrefwait", "progressrefwait", "multiref", "false-match":
			if waitConditionReached(task) {
				t.Errorf("wait woke without all explicit references closed: %s", task.slug)
			}
		}
	}
	for _, ref := range []string{"git/other/cross", "other/cross", "work/other/cross"} {
		if got := waitReferences("после " + ref); len(got) != 1 || got[0] != ref {
			t.Errorf("explicit project reference %q parsed as %q", ref, got)
		}
	}
	if got := waitReferences("sample project-local-reference"); len(got) != 1 || got[0] != "project-local-reference" {
		t.Errorf("local multiword slug was not parsed as a complete token: %q", got)
	}
	if got := waitReferences("release"); len(got) != 0 {
		t.Errorf("common one-word slug was inferred as a reference: %q", got)
	}
	if got := waitReferences("2026-11-30 12:00"); len(got) != 0 {
		t.Errorf("date was inferred as a local slug: %q", got)
	}
	for _, slug := range []string{"openrefwait", "multiref", "false-match"} {
		nextOut.Reset()
		nextErr.Reset()
		code := runMain([]string{"next", "--by", "tester"}, time.Now(), &nextOut, &nextErr)
		if code != 0 || strings.Contains(nextOut.String(), slug) {
			t.Errorf("next falsely woke %s: code=%d out=%q err=%q", slug, code, nextOut.String(), nextErr.String())
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "20261120-1200-MED-by-blocker.md"), []byte("# By blocker\n\nСтатус: готово\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nextOut.Reset()
	nextErr.Reset()
	if code := runMain([]string{"next", "--by", "tester"}, time.Now(), &nextOut, &nextErr); code != 0 || !strings.Contains(nextOut.String(), "by-text") || !strings.Contains(nextOut.String(), "условие наступило") {
		t.Fatalf("next local closed mentioned ticket: code=%d out=%q err=%q", code, nextOut.String(), nextErr.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "20261120-1110-MED-openref.md"), []byte("# Open ref\n\nСтатус: готово\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err = loadTasks(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	idx = newWaitReferenceIndex()
	idx.addTasks(loaded)
	for _, task := range loaded {
		task.waitIndex = idx
		if task.slug == "multiref" && !waitConditionReached(task) {
			t.Fatal("wait with multiple references did not wake after all references closed")
		}
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
	work := filepath.Join(home, "work", "third", "docs", "tasks")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRunMainTask(t, other, "20261120-1000-MED-cross.md", "# Cross\n\nСтатус: готово\n")
	writeRunMainTask(t, work, "20261120-1000-MED-workclosed.md", "# Work closed\n\nСтатус: отклонён\n")
	blocked := writeRunMainTask(t, dir, "20261120-1100-MED-cross-blocked.md", "# Cross blocked\n\nСтатус: ждёт\nЗаблокирован: git/other/cross\nЖдёт: результат\n")
	first := writeRunMainTask(t, dir, "20261120-1200-MED-waiting.md", "# Waiting\n\nСтатус: ждёт\nЖдёт: после git/other/cross\n")
	writeRunMainTask(t, dir, "20261120-1300-MED-waiting-alias.md", "# Waiting alias\n\nСтатус: ждёт\nЖдёт: после other/cross\n")
	localTasks, err := loadTasks(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	idx := newWaitReferenceIndex()
	idx.addTasks(localTasks)
	if !closedWaitReference(project, "work/third/workclosed", idx) {
		t.Fatal("closed work/project/slug reference was not resolved")
	}
	var out, errout bytes.Buffer
	if code := runMain([]string{"next", "--by", "tester"}, time.Now(), &out, &errout); code != 0 || !strings.Contains(out.String(), "cross-blocked") || !strings.Contains(out.String(), "условие наступило") {
		t.Fatalf("next cross-project blocker: code=%d out=%q err=%q", code, out.String(), errout.String())
	}
	if err := os.WriteFile(blocked, []byte("# Cross blocked\n\nСтатус: готово\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errout.Reset()
	if code := runMain([]string{"next", "--by", "tester"}, time.Now(), &out, &errout); code != 0 || !strings.HasPrefix(strings.TrimSpace(out.String()), "waiting ") || !strings.Contains(out.String(), "условие наступило") {
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
