package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestWaitRequiresLeadingDateAndRejectsOwnQueue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "git", "p1")
	dir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	path := writeRunMainTask(t, dir, "20261124-1200-MED-current.md", "# Current\n\nСтатус: открыт\n\n## Приёмка\n- проверка\n\n## Журнал\n")
	writeRunMainTask(t, dir, "20261124-1210-MED-own-other.md", "# Own other\n\nСтатус: открыт\n")
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
	for _, until := range []string{"2026-11-30 12:00: очередь после G3", "2026-11-30 12:00: отчёт: очередь", "2026-11-30 12:00: ответ: жду очередь", "2026-11-30 12:00: после p1/own-other", "2026-11-30 12:00: после git/p1/own-other", "2026-11-30 12:00: очередь после own-other"} {
		errout.Reset()
		code = runMain([]string{"wait", "current", "--until", until, "--by", "tester"}, time.Now(), &out, &errout)
		if code == 0 || !strings.Contains(errout.String(), "после своего тикета") {
			t.Errorf("wait did not reject own-ticket link %q: code=%d stderr=%q", until, code, errout.String())
		}
	}
}

func TestNextWakesOnClosedLocalReferencesAndDueListsUndatedWait(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, dir := testRunMainRoot(t)
	writeRunMainTask(t, dir, "20261120-1000-MED-blocker.md", "# Blocker\n\nСтатус: готово\n")
	writeRunMainTask(t, dir, "20261120-1100-MED-mentioned.md", "# Mentioned\n\nСтатус: отклонён\n")
	writeRunMainTask(t, dir, "20261120-1030-MED-example-parent-p1.md", "# Example parent p1\n\nСтатус: готово\n")
	writeRunMainTask(t, dir, "20261120-1035-MED-example-parent.md", "# Example parent\n\nСтатус: готово\n")
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
	falseReasons := map[string]string{"false-doc-label": "doc-topic README п. 263", "false-parent-slug": "example-parent p1", "false-underscore": "blk_extra", "false-dot": "state.json", "false-common-word": "release graft"}
	for slug, reason := range falseReasons {
		writeRunMainTask(t, dir, "20261120-1700-MED-"+slug+".md", "# False match\n\nСтатус: ждёт\nЖдёт: "+reason+"\n")
	}
	writeRunMainTask(t, dir, "20261120-1710-MED-unknown-plus-closed.md", "# Unknown plus closed\n\nСтатус: ждёт\nЖдёт: ABC-123 e-mail 0/0 после mentioned\n")
	writeRunMainTask(t, dir, "20261120-1720-MED-multi-project-and.md", "# Multi project and\n\nСтатус: ждёт\nЖдёт: после blk и p2/xblk\n")
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
		case "openrefwait", "progressrefwait", "multiref", "false-doc-label", "false-parent-slug", "false-underscore", "false-dot", "false-common-word":
			if waitConditionReached(task) {
				t.Errorf("wait woke without all explicit references closed: %s", task.slug)
			}
		case "multi-project-and":
			if !waitConditionReached(task) {
				t.Error("closed ticket was masked by an unresolved project-prefixed token after conjunction")
			}
		}
		if task.slug == "unknown-plus-closed" && !waitConditionReached(task) {
			t.Error("unresolved text masked a found closed ticket")
		}
	}
	for _, ref := range []string{"git/other/cross", "other/cross", "work/other/cross"} {
		if got := waitReferences("ref " + ref); len(got) != 1 || got[0] != ref {
			t.Errorf("explicit project reference %q parsed as %q", ref, got)
		}
	}
	if got := waitReferences("после blk и p2/xblk"); len(got) != 2 || !slices.Contains(got, "blk") || !slices.Contains(got, "p2/xblk") {
		t.Errorf("project prefix after conjunction parsed as extra reference: %q", got)
	}
	if got := waitReferences("sample project-local-reference"); !slices.Contains(got, "project-local-reference") {
		t.Errorf("local multiword slug was not parsed as a complete token: %q", got)
	}
	if got := waitReferences("release"); len(got) != 0 {
		t.Errorf("common one-word slug was inferred as a reference: %q", got)
	}
	if got := waitReferences("2026-11-30 12:00"); len(got) != 0 {
		t.Errorf("date was inferred as a local slug: %q", got)
	}
	for _, slug := range []string{"openrefwait", "multiref", "false-doc-label", "false-parent-slug", "false-underscore", "false-dot", "false-common-word"} {
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
	writeRunMainTask(t, other, "20261120-1001-MED-cross-spaced.md", "# Cross spaced\n\nСтатус: отклонён\n")
	if got := waitReferences("other cross-spaced"); len(got) != 1 || got[0] != "other/cross-spaced" {
		t.Fatalf("spaced project reference parsed as %q", got)
	}
	writeRunMainTask(t, work, "20261120-1000-MED-workclosed.md", "# Work closed\n\nСтатус: отклонён\n")
	writeRunMainTask(t, dir, "20261120-1050-MED-blk.md", "# Blk\n\nСтатус: готово\n")
	blocked := writeRunMainTask(t, dir, "20261120-1100-MED-cross-blocked.md", "# Cross blocked\n\nСтатус: ждёт\nЗаблокирован: git/other/cross\nЖдёт: результат\n")
	first := writeRunMainTask(t, dir, "20261120-1200-MED-waiting.md", "# Waiting\n\nСтатус: ждёт\nЖдёт: после git/other/cross\n")
	writeRunMainTask(t, dir, "20261120-1300-MED-waiting-alias.md", "# Waiting alias\n\nСтатус: ждёт\nЖдёт: после other/cross\n")
	spaced := writeRunMainTask(t, dir, "20261120-1400-MED-waiting-spaced.md", "# Waiting spaced\n\nСтатус: ждёт\nЖдёт: после blk и other cross-spaced\n")
	localTasks, err := loadTasks(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	idx := newWaitReferenceIndex()
	idx.addTasks(localTasks)
	for _, task := range localTasks {
		if task.slug == "waiting-spaced" {
			task.waitIndex = idx
			if !waitConditionReached(task) {
				t.Fatal("closed spaced project reference after a conjunction did not wake")
			}
		}
	}
	if err := os.Remove(spaced); err != nil {
		t.Fatal(err)
	}
	if !closedWaitReference(project, "work/third/workclosed", idx) {
		t.Fatal("closed work/project/slug reference was not resolved")
	}
	if _, ok, err := resolveWaitProject("other"); err != nil || !ok {
		t.Fatal("project existing in exactly one root was not resolved")
	}
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "work", "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveWaitProject("other"); err != nil || ok {
		t.Fatalf("ambiguous git/work project resolved: ok=%v err=%v", ok, err)
	}
	if err := os.Remove(filepath.Join(home, "work", "other")); err != nil {
		t.Fatal(err)
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

func TestSpacedWaitProjectReportsLookupErrorsAndSkipsMissingProjects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	gitRoot := filepath.Join(home, "git")
	if err := os.MkdirAll(gitRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	blockedParent := filepath.Join(gitRoot, "file")
	if err := os.WriteFile(blockedParent, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(blockedParent, "child"), filepath.Join(gitRoot, "broken")); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldWriter) })
	waitSpacedProjectSlugs("broken sample-slug")
	addWaitSpacedProjectReferences("broken sample-slug", func(string) {})
	if got := strings.Count(logs.String(), "cannot resolve spaced wait project \"broken\""); got != 2 {
		t.Fatalf("both spaced-project parsers should report lookup errors; count=%d logs=%q", got, logs.String())
	}
	logs.Reset()
	waitSpacedProjectSlugs("missing sample-slug")
	addWaitSpacedProjectReferences("missing sample-slug", func(string) {})
	if logs.Len() != 0 {
		t.Fatalf("a missing project should be skipped without an error log: %q", logs.String())
	}
}

func TestWaitReferenceIndexLoadsArchiveOnlyAfterActiveMiss(t *testing.T) {
	project := t.TempDir()
	active := task{path: filepath.Join(project, "docs", "tasks", "20261120-1000-MED-active.md"), slug: "active", status: "готово"}
	index := newWaitReferenceIndex()
	entry := index.project(project)
	entry.activeLoaded = true
	addWaitIndexTask(&entry.active, &active)
	if got, ok := index.lookup(project, "active"); !ok || got.slug != "active" {
		t.Fatalf("active lookup = %#v, %v", got, ok)
	}
	if entry.archiveLoaded {
		t.Fatal("archive was loaded despite an active match")
	}
}

func TestWaitReferenceIndexCachesSpacedProjectLookup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "git", "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	index := newWaitReferenceIndex()
	waitReferencesUsing("proj first-ticket and proj second-ticket", index.resolveProject)
	if len(index.projectLookups) != 1 {
		t.Fatalf("project lookups = %v, want one cached project", index.projectLookups)
	}
}

func TestWaitReferenceIndexRejectsAmbiguousIDs(t *testing.T) {
	project := t.TempDir()
	index := newWaitReferenceIndex()
	entry := index.project(project)
	entry.activeLoaded = true
	for _, name := range []string{"20261120-1000-MED-one.md", "20261120-1001-MED-two.md"} {
		ticket := task{path: filepath.Join(project, "docs", "tasks", name), slug: "duplicate", status: "готово"}
		addWaitIndexTask(&entry.active, &ticket)
	}
	if got, ok := index.lookup(project, "duplicate"); ok || got != nil {
		t.Fatalf("ambiguous lookup = %#v, %v", got, ok)
	}
}
