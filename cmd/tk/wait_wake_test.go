package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func mustWaitReferences(t *testing.T, value string) []string {
	t.Helper()
	refs, err := waitReferencesUsing(value, resolveWaitProject)
	if err != nil {
		t.Fatal(err)
	}
	return refs
}

func mustIndexWaitTasks(t *testing.T, index *waitReferenceIndex, tasks []task) {
	t.Helper()
	if err := index.addTasks(tasks); err != nil {
		t.Fatal(err)
	}
}

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
	for _, until := range []string{"2026-11-30 12:00: в очереди sample впереди MED старше", "2026-11-30 12:00: (state.json очереди)", "2026-11-30 12:00: ничья очереди газа", "2026-11-30 12:00: очередь выкатки graft", "2026-11-30 12:00: ответ Юры, он в очереди", "2026-11-30 12:00: ответ Юры про очередь", "2026-11-30 12:00: релиз через общую очередь", "2026-11-30 12:00: показа demo"} {
		errout.Reset()
		code = runMain([]string{"wait", "external", "--until", until, "--by", "tester"}, time.Date(2026, 11, 24, 12, 0, 0, 0, time.Local), &out, &errout)
		if code != 0 {
			t.Errorf("wait falsely rejected %q: code=%d stderr=%q", until, code, errout.String())
		}
	}
	errout.Reset()
	code = runMain([]string{"wait", "current", "--until", "2026-02-30 10:00: срок", "--by", "tester"}, time.Date(2026, 2, 1, 12, 0, 0, 0, time.Local), &out, &errout)
	if code == 0 || !strings.Contains(errout.String(), "невозможная дата") {
		t.Errorf("impossible calendar date error = code %d, stderr %q", code, errout.String())
	}
	errout.Reset()
	code = runMain([]string{"wait", "current", "--until", "31.02 10:00: срок", "--by", "tester"}, time.Date(2026, 2, 1, 12, 0, 0, 0, time.Local), &out, &errout)
	if code == 0 || !strings.Contains(errout.String(), "невозможная дата") {
		t.Errorf("impossible short calendar date error = code %d, stderr %q", code, errout.String())
	}
	for _, until := range []string{"2026-11-30 12:00: очередь после G3", "2026-11-30 12:00: после p1/own-other", "2026-11-30 12:00: после git/p1/own-other", "2026-11-30 12:00: очередь после own-other"} {
		errout.Reset()
		code = runMain([]string{"wait", "current", "--until", until, "--by", "tester"}, time.Now(), &out, &errout)
		if code == 0 || !strings.Contains(errout.String(), "после своего тикета") {
			t.Errorf("wait did not reject own-ticket link %q: code=%d stderr=%q", until, code, errout.String())
		}
	}
}

func TestRunMainAcceptsSingleDigitShortDates(t *testing.T) {
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.Local)
	_, dir := testRunMainRoot(t)
	cases := []struct {
		until string
		want  time.Time
	}{
		{"1.12 9:05 ответ", time.Date(2026, 12, 1, 9, 5, 0, 0, time.Local)},
		{"5.3 10:00 x", time.Date(2027, 3, 5, 10, 0, 0, 0, time.Local)},
		{"01.2 10:00 x", time.Date(2027, 2, 1, 10, 0, 0, 0, time.Local)},
		{"1.12 x", time.Date(2026, 12, 1, 0, 0, 0, 0, time.Local)},
		{"7.11 10:00 x", time.Date(2027, 11, 7, 10, 0, 0, 0, time.Local)},
		{"17.1 10:00 x", time.Date(2027, 1, 17, 10, 0, 0, 0, time.Local)},
		{"1.12 09:00-10:00 созвон", time.Date(2026, 12, 1, 9, 0, 0, 0, time.Local)},
	}
	for i, tc := range cases {
		slug := fmt.Sprintf("short-date-%d", i)
		writeRunMainTask(t, dir, fmt.Sprintf("20261120-10%02d-MED-%s.md", i, slug), "# Task\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
		var out, errout bytes.Buffer
		if code := runMain([]string{"wait", slug, "--until", tc.until, "--by", "tester"}, now, &out, &errout); code != 0 {
			t.Errorf("short date %q rejected: code=%d stderr=%q", tc.until, code, errout.String())
			continue
		}
		updated, err := find(dir, slug)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := waitTimeDeadline(waiting(updated.body), now)
		if !ok || !got.Equal(tc.want) {
			t.Errorf("short date %q deadline = %v, ok=%v; want %v (wait=%q)", tc.until, got, ok, tc.want, waiting(updated.body))
		}
	}
	path := writeRunMainTask(t, dir, "20261120-1100-MED-impossible-short.md", "# Impossible\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	var out, errout bytes.Buffer
	if code := runMain([]string{"wait", "impossible-short", "--until", "31.02 10:00 x", "--by", "tester"}, now, &out, &errout); code == 0 || !strings.Contains(errout.String(), "невозможная дата") {
		t.Fatalf("impossible short date result: code=%d stderr=%q", code, errout.String())
	}
	updated, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(updated), "Статус: открыт") || strings.Contains(string(updated), "Ждёт:") {
		t.Fatalf("impossible date changed ticket: body=%q err=%v", updated, err)
	}
}

func TestWaitOwnQueueUsesOnlyEstablishedForms(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "git", "p1")
	dir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	otherDir := filepath.Join(home, "git", "p2", "docs", "tasks")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
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
	writeRunMainTask(t, dir, "20261124-1210-MED-own-other.md", "# Own\n\nСтатус: открыт\n")
	writeRunMainTask(t, otherDir, "20261124-1210-MED-own-other.md", "# Foreign\n\nСтатус: открыт\n")
	cases := []struct {
		text   string
		refuse bool
	}{
		{"2026-11-30 12:00: очередь после G3", true},
		{"2026-11-30 12:00: своя очередь", true},
		{"2026-11-30 12:00: после own-other", true},
		{"2026-11-30 12:00: после own-other.", true},
		{"2026-11-30 12:00: после own-other,", true},
		{"2026-11-30 12:00: после own-other)", true},
		{"2026-11-30 12:00: после p2/own-other", false},
		{"2026-11-30 12:00: после G3", true},
		{"2026-11-30 12:00: жду своей очереди", false},
		{"2026-11-30 12:00: моя очередь", false},
		{"2026-11-30 12:00: до закрытия own-other", false},
		{"2026-11-30 12:00: ждёт own-other", false},
		{"2026-11-30 12:00: очередь", true},
		{"2026-11-30 12:00: жду очередь", true},
		{"2026-11-30 12:00: отчёт: очередь", true},
		{"2026-11-30 12:00: ответ: жду очередь", true},
		{"2026-11-30 12:00: ответ Юры, он в очереди", false},
	}
	for i, tc := range cases {
		slug := fmt.Sprintf("current-%d", i)
		writeRunMainTask(t, dir, "20261124-1300-MED-"+slug+".md", "# Current\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
		var out, errout bytes.Buffer
		code := runMain([]string{"wait", slug, "--until", tc.text, "--by", "tester"}, time.Now(), &out, &errout)
		if tc.refuse && (code == 0 || !strings.Contains(errout.String(), "после своего тикета")) {
			t.Errorf("established own-queue form was accepted: %q code=%d stderr=%q", tc.text, code, errout.String())
		}
		if !tc.refuse && code != 0 {
			t.Errorf("ordinary wait text was refused: %q code=%d stderr=%q", tc.text, code, errout.String())
		}
	}
}

func TestNextWakesOnClosedLocalReferencesAndDueListsUndatedWait(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, dir := testRunMainRoot(t)
	project := filepath.Dir(filepath.Dir(dir))
	writeRunMainTask(t, dir, "20261120-1000-MED-blocker.md", "# Blocker\n\nСтатус: готово\n\n## Журнал\n- 2026-11-21 10:00 готово — checker: закрыт\n")
	writeRunMainTask(t, dir, "20261120-1100-MED-mentioned.md", "# Mentioned\n\nСтатус: отклонён\n\n## Журнал\n- 2026-11-21 10:00 отклонён — checker: закрыт\n")
	writeRunMainTask(t, dir, "20261120-1009-MED-stale-blocker-ref.md", "# Stale blocker ref\n\nСтатус: готово\n\n## Журнал\n- 2026-11-19 10:00 готово — checker: closed\n")
	writeRunMainTask(t, dir, "20261120-1030-MED-example-parent-p1.md", "# Example parent p1\n\nСтатус: готово\n")
	writeRunMainTask(t, dir, "20261120-1035-MED-example-parent.md", "# Example parent\n\nСтатус: готово\n")
	writeRunMainTask(t, dir, "20261120-1040-MED-blk.md", "# Blk\n\nСтатус: готово\n\n## Журнал\n- 2026-11-21 10:00 готово — checker: закрыт\n")
	writeRunMainTask(t, dir, "20261120-1050-MED-state.md", "# State\n\nСтатус: готово\n")
	writeRunMainTask(t, dir, "20261120-1055-MED-release.md", "# Release\n\nСтатус: готово\n")
	writeRunMainTask(t, dir, "20261120-1056-MED-doc-topic.md", "# Doc topic\n\nСтатус: готово\n\n## Журнал\n- 2026-11-21 10:00 готово — checker: закрыт\n")
	writeRunMainTask(t, dir, "20261120-1110-MED-openref.md", "# Open ref\n\nСтатус: открыт\n")
	writeRunMainTask(t, dir, "20261120-1120-MED-progressref.md", "# Progress ref\n\nСтатус: в работе\n")
	writeRunMainTask(t, dir, "20261120-1200-MED-by-blocker.md", "# By blocker\n\nСтатус: ждёт\nЗаблокирован: blocker\nЖдёт: ответ от blocker\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	writeRunMainTask(t, dir, "20261120-1300-MED-by-text.md", "# By text\n\nСтатус: ждёт\nЖдёт: после mentioned\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	writeRunMainTask(t, dir, "20261120-1310-MED-by-id.md", "# By ID\n\nСтатус: ждёт\nЖдёт: после 20261120-1100-MED-mentioned\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	writeRunMainTask(t, dir, "20261120-1400-MED-undated.md", "# Undated\n\nСтатус: ждёт\nЖдёт: ответ Юры\n")
	writeRunMainTask(t, dir, "20261120-1500-MED-openrefwait.md", "# Open ref wait\n\nСтатус: ждёт\nЖдёт: после openref\n")
	writeRunMainTask(t, dir, "20261120-1510-MED-progressrefwait.md", "# Progress ref wait\n\nСтатус: ждёт\nЖдёт: после progressref\n")
	writeRunMainTask(t, dir, "20261120-1600-MED-multiref.md", "# Multi ref\n\nСтатус: ждёт\nЖдёт: после mentioned и openref\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	writeRunMainTask(t, dir, "20261120-1610-MED-range-plus-closed.md", "# Range plus closed\n\nСтатус: ждёт\nЖдёт: после тикетов foo-1…2 и после mentioned\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	writeRunMainTask(t, dir, "20261120-1620-MED-period-plus-closed.md", "# Period plus closed\n\nСтатус: ждёт\nЖдёт: после openref. Потом mentioned\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	writeRunMainTask(t, dir, "20261120-1622-MED-period-closed.md", "# Period closed\n\nСтатус: ждёт\nЖдёт: после mentioned.\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	writeRunMainTask(t, dir, "20261120-1625-MED-trailing-dot-plus-closed.md", "# Trailing dot plus closed\n\nСтатус: ждёт\nЖдёт: после open-one. Потом mentioned\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	writeRunMainTask(t, dir, "20261120-1630-MED-stale-blocker.md", "# Stale blocker\n\nСтатус: ждёт\nЗаблокирован: stale-blocker-ref\nЖдёт: после mentioned\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	writeRunMainTask(t, dir, "20261120-1635-MED-stale-blocker-only.md", "# Stale blocker only\n\nСтатус: ждёт\nЗаблокирован: stale-blocker-ref\nЖдёт: 2026-12-30 10:00: ответ\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	writeRunMainTask(t, dir, "20261120-1640-MED-missing-blocker.md", "# Missing blocker\n\nСтатус: ждёт\nЗаблокирован: absent-blocker\nЖдёт: после mentioned\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	falseReasons := map[string]string{"false-doc-label": "doc-topic README п. 263", "false-parent-slug": "example-parent p1", "false-underscore": "blk_extra", "false-dot": "state.json", "false-common-word": "release graft"}
	for slug, reason := range falseReasons {
		writeRunMainTask(t, dir, "20261120-1700-MED-"+slug+".md", "# False match\n\nСтатус: ждёт\nЖдёт: "+reason+"\n\n## Журнал\n- 2026-11-20 17:00 ждёт — tester: замера\n")
	}
	writeRunMainTask(t, dir, "20261120-1710-MED-unknown-plus-closed.md", "# Unknown plus closed\n\nСтатус: ждёт\nЖдёт: ABC-123 e-mail 0/0 после mentioned\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	writeRunMainTask(t, dir, "20261120-1720-MED-multi-project-and.md", "# Multi project and\n\nСтатус: ждёт\nЖдёт: после blk и p2/xblk\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	archive := filepath.Join(filepath.Dir(dir), "tasks-archive")
	if err := os.MkdirAll(archive, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRunMainTask(t, archive, "20261120-1800-MED-archived.md", "# Archived\n\nСтатус: готово\n\n## Журнал\n- 2026-11-21 10:00 готово — checker: закрыт\n")
	writeRunMainTask(t, archive, "20261120-1810-MED-readme.md", "# README\n\nСтатус: готово\n")
	writeRunMainTask(t, dir, "20261120-1900-MED-archivewait.md", "# Archive wait\n\nСтатус: ждёт\nЖдёт: после archived\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	var nextOut, nextErr bytes.Buffer
	if code := runMain([]string{"next", "--by", "tester"}, time.Now(), &nextOut, &nextErr); code != 0 || !strings.Contains(nextOut.String(), "by-blocker") || !strings.Contains(nextOut.String(), "условие наступило") {
		t.Fatalf("next local closed blocker: code=%d out=%q err=%q", code, nextOut.String(), nextErr.String())
	}
	loaded, err := loadTasks(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	idx := newWaitReferenceIndex()
	mustIndexWaitTasks(t, idx, loaded)
	for _, task := range loaded {
		task.waitIndex = idx
		switch task.slug {
		case "by-blocker", "by-text", "by-id", "archivewait", "period-closed":
			if !waitConditionReached(task) {
				t.Errorf("closed reference did not wake %s", task.slug)
			}
		case "openrefwait", "progressrefwait", "multiref", "range-plus-closed", "period-plus-closed", "trailing-dot-plus-closed", "stale-blocker-only", "false-doc-label", "false-parent-slug", "false-underscore", "false-dot", "false-common-word":
			if waitConditionReached(task) {
				t.Errorf("wait woke without all explicit references closed: %s", task.slug)
			}
		case "multi-project-and":
			if !waitConditionReached(task) {
				t.Error("closed ticket was masked by an unresolved project-prefixed token after conjunction")
			}
		}
		if task.slug == "unknown-plus-closed" && !waitConditionReached(task) {
			t.Error("recognized unresolved ticket text masked a found closed ticket")
		}
		if task.slug == "stale-blocker" && !waitConditionReached(task) {
			t.Error("closed before waiting blocker masked a found closed text reference")
		}
		if task.slug == "missing-blocker" && !waitConditionReached(task) {
			t.Error("missing blocker masked a found closed ticket")
		}
	}
	for _, ref := range []string{"git/other/cross", "other/cross", "work/other/cross"} {
		if got := mustWaitReferences(t, "ref "+ref); len(got) != 1 || got[0] != ref {
			t.Errorf("explicit project reference %q parsed as %q", ref, got)
		}
	}
	if _, _, ok, err := waitReferenceTargetErrUsing(project, "git/../evil-x", resolveWaitProject); err != nil || ok {
		t.Fatal("traversal project reference was accepted")
	}
	if got := mustWaitReferences(t, "после blk и p2/xblk"); len(got) != 2 || !slices.Contains(got, "blk") || !slices.Contains(got, "p2/xblk") {
		t.Errorf("project prefix after conjunction parsed as extra reference: %q", got)
	}
	if got := mustWaitReferences(t, "sample project-local-reference"); !slices.Contains(got, "project-local-reference") {
		t.Errorf("local multiword slug was not parsed as a complete token: %q", got)
	}
	if got := mustWaitReferences(t, "release"); len(got) != 0 {
		t.Errorf("common one-word slug was inferred as a reference: %q", got)
	}
	if got := mustWaitReferences(t, "2026-11-30 12:00"); len(got) != 0 {
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
	mustIndexWaitTasks(t, idx, loaded)
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

func TestResolveWaitProjectRejectsOverlongNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if project, ok, err := resolveWaitProject(strings.Repeat("x", 300)); err != nil || ok || project != "" {
		t.Fatalf("overlong project name resolved: project=%q ok=%v err=%v", project, ok, err)
	}
	for _, prefix := range []string{"git", "work"} {
		_, _, ok, err := waitReferenceTargetErrUsing("/project", prefix+"/"+strings.Repeat("x", 300)+"/ticket", resolveWaitProject)
		if err != nil || ok {
			t.Errorf("overlong %s path resolved: ok=%v err=%v", prefix, ok, err)
		}
	}
}

func TestOwnWaitHintReturnsReferenceLookupErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "git", "p1")
	if err := os.MkdirAll(filepath.Join(project, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "docs", "tasks"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	current := task{path: filepath.Join(project, "docs", "tasks", "20261120-1000-MED-current.md")}
	for _, value := range []string{"2026-12-01 10:00: после own-other", "2026-12-01 10:00: после p1/own-other"} {
		if hint, err := ownWaitHint(current, value); err == nil || hint != "" {
			t.Errorf("reference lookup error was swallowed for %q: hint=%q err=%v", value, hint, err)
		}
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
	writeRunMainTask(t, other, "20261120-1000-MED-cross.md", "# Cross\n\nСтатус: готово\n\n## Журнал\n- 2026-11-21 10:00 готово — checker: закрыт\n")
	writeRunMainTask(t, other, "20261120-1001-MED-cross-spaced.md", "# Cross spaced\n\nСтатус: отклонён\n\n## Журнал\n- 2026-11-21 10:00 отклонён — checker: закрыт\n")
	if got := mustWaitReferences(t, "other cross-spaced"); len(got) != 1 || got[0] != "other/cross-spaced" {
		t.Fatalf("spaced project reference parsed as %q", got)
	}
	writeRunMainTask(t, work, "20261120-1000-MED-workclosed.md", "# Work closed\n\nСтатус: отклонён\n")
	writeRunMainTask(t, dir, "20261120-1050-MED-blk.md", "# Blk\n\nСтатус: готово\n")
	blocked := writeRunMainTask(t, dir, "20261120-1100-MED-cross-blocked.md", "# Cross blocked\n\nСтатус: ждёт\nЗаблокирован: git/other/cross\nЖдёт: результат\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	first := writeRunMainTask(t, dir, "20261120-1200-MED-waiting.md", "# Waiting\n\nСтатус: ждёт\nЖдёт: после git/other/cross\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	writeRunMainTask(t, dir, "20261120-1300-MED-waiting-alias.md", "# Waiting alias\n\nСтатус: ждёт\nЖдёт: после other/cross\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	spaced := writeRunMainTask(t, dir, "20261120-1400-MED-waiting-spaced.md", "# Waiting spaced\n\nСтатус: ждёт\nЖдёт: после blk и other cross-spaced\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: ждёт\n")
	localTasks, err := loadTasks(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	idx := newWaitReferenceIndex()
	mustIndexWaitTasks(t, idx, localTasks)
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
	linked, ok, lookupErr := lookupWaitReferenceErr(project, "work/third/workclosed", idx)
	if lookupErr != nil || !ok || !closedStatus(linked.status) {
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
	if _, err := waitSpacedProjectSlugsUsing("broken sample-slug", resolveWaitProject); err == nil {
		t.Fatal("spaced-project parser dropped resolver error")
	}
	if err := addWaitSpacedProjectReferencesUsing("broken sample-slug", func(string) {}, resolveWaitProject); err == nil {
		t.Fatal("spaced-project reference parser dropped resolver error")
	}
	if _, err := waitSpacedProjectSlugsUsing("missing sample-slug", resolveWaitProject); err != nil {
		t.Fatalf("missing project should be skipped: %v", err)
	}
	if err := addWaitSpacedProjectReferencesUsing("missing sample-slug", func(string) {}, resolveWaitProject); err != nil {
		t.Fatalf("missing project should be skipped: %v", err)
	}
}

func TestWaitReferenceIndexLoadsArchiveOnlyAfterActiveMiss(t *testing.T) {
	project := t.TempDir()
	active := task{path: filepath.Join(project, "docs", "tasks", "20261120-1000-MED-active.md"), slug: "active", status: "готово"}
	index := newWaitReferenceIndex()
	entry := index.project(project)
	entry.activeLoaded = true
	addWaitIndexTask(&entry.active, &active)
	if got, ok, err := index.lookupErr(project, "active"); err != nil || !ok || got.slug != "active" {
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
	if _, err := waitReferencesUsing("proj first-ticket and proj second-ticket", index.resolveProject); err != nil {
		t.Fatal(err)
	}
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
	if got, ok, err := index.lookupErr(project, "duplicate"); err == nil || ok || got != nil {
		t.Fatalf("ambiguous lookup = %#v, %v, err=%v", got, ok, err)
	}
}

func TestWaitReferenceMustCloseAfterWaitJournalEntry(t *testing.T) {
	project := t.TempDir()
	dir := filepath.Join(project, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, stamp, extraWait, closedAt string
		want                             bool
	}{
		{name: "closed-before-wait", stamp: "2026-11-16 12:00", closedAt: "2026-11-02 10:00", want: false},
		{name: "closed-after-wait", stamp: "2026-11-16 12:00", closedAt: "2026-11-18 10:00", want: true},
		{name: "latest-wait-entry", stamp: "2026-11-16 12:00", extraWait: "- 2026-11-19 12:00 ждёт — tester: again\n", closedAt: "2026-11-18 10:00", want: false},
		{name: "same-minute", stamp: "2026-11-16 12:00", closedAt: "2026-11-16 12:00", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeRunMainTask(t, dir, "20261101-1000-MED-binary-trace.md", "# binary-trace\n\nСтатус: готово\n\n## Журнал\n- "+tc.closedAt+" готово — checker: закрыт\n")
			waitPath := writeRunMainTask(t, dir, "20261116-1100-MED-wait-"+tc.name+".md", "# wait\n\nСтатус: ждёт\nЖдёт: закрытия binary-trace\n\n## Журнал\n- "+tc.stamp+" ждёт — tester: закрытия binary-trace\n"+tc.extraWait)
			tasks, err := loadTasks(dir, true)
			if err != nil {
				t.Fatal(err)
			}
			idx := newWaitReferenceIndex()
			mustIndexWaitTasks(t, idx, tasks)
			for _, task := range tasks {
				if task.path == waitPath {
					task.waitIndex = idx
					if got := waitConditionReached(task); got != tc.want {
						t.Fatalf("wait condition = %v, want %v", got, tc.want)
					}
				}
			}
		})
	}
}

func TestWaitReferenceLookupErrorsAreReturned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "git", "foreign")
	dir := filepath.Join(project, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(project, "docs", "tasks-archive")
	if err := os.WriteFile(archive, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "20261399-1000-MED-invalid-date.md"), []byte("# invalid\n\nСтатус: готово\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	index := newWaitReferenceIndex()
	for _, ref := range []string{"foreign/missing", "foreign/invalid-date"} {
		if linked, ok, err := lookupWaitReferenceErr(project, ref, index); err == nil && ok && closedStatus(linked.status) {
			t.Errorf("unreadable or malformed reference %q was treated as closed", ref)
		} else if err == nil {
			t.Errorf("unreadable or malformed reference %q did not return an error", ref)
		}
	}
}

func TestNextKeepsWaitWhenArchiveReferenceCannotBeRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "git", "sample")
	dir := filepath.Join(project, "docs", "tasks")
	otherTasks := filepath.Join(home, "git", "other", "docs", "tasks")
	archive := filepath.Join(home, "git", "other", "docs", "tasks-archive")
	for _, path := range []string{dir, otherTasks} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(archive, []byte("not a directory"), 0o644); err != nil {
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
	writeRunMainTask(t, dir, "20261120-1100-MED-unreadable-wait.md", "# Waiting\n\nСтатус: ждёт\nЖдёт: после other/secret\n\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: waiting\n")
	var out, errout bytes.Buffer
	stderr, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = oldStderr })
	code := runMain([]string{"next", "--by", "tester"}, time.Now(), &out, &errout)
	if err := stderr.Close(); err != nil {
		t.Fatal(err)
	}
	stderrText, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "unreadable-wait") || !strings.Contains(string(stderrText), `cannot read wait reference for "unreadable-wait"`) {
		t.Fatalf("unreadable archive reference woke wait or was not reported: code=%d out=%q err=%q stderr=%q", code, out.String(), errout.String(), stderrText)
	}
}

func TestWaitConditionDoesNotWakeForOpenBlockerOrMention(t *testing.T) {
	project := t.TempDir()
	dir := filepath.Join(project, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRunMainTask(t, dir, "20261101-1000-MED-open-blocker.md", "# blocker\n\nСтатус: открыт\n")
	writeRunMainTask(t, dir, "20261101-1001-MED-open-mention.md", "# mention\n\nСтатус: в работе\n")
	for _, body := range []string{
		"Статус: ждёт\nЗаблокирован: open-blocker\nЖдёт: результата\n",
		"Статус: ждёт\nЖдёт: после open-mention\n",
	} {
		waiting := writeRunMainTask(t, dir, "20261101-1100-MED-waiting.md", "# waiting\n\n"+body+"\n## Журнал\n- 2026-11-02 10:00 ждёт — tester: ждёт\n")
		tasks, err := loadTasks(dir, true)
		if err != nil {
			t.Fatal(err)
		}
		idx := newWaitReferenceIndex()
		mustIndexWaitTasks(t, idx, tasks)
		for _, task := range tasks {
			if task.path == waiting {
				task.waitIndex = idx
				if waitConditionReached(task) {
					t.Fatal("wait woke for an open or in-progress reference")
				}
			}
		}
		if err := os.Remove(waiting); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWaitConditionKeepsUnresolvedBlockersAndCandidatesAsleep(t *testing.T) {
	cases := []struct {
		name, blocker, wait string
		wantOpen            bool
	}{
		{name: "missing-blocker-only", blocker: "no-such-ticket", wait: "answer"},
		{name: "open-blocker-and-closed-text", blocker: "open-one", wait: "after new-closed", wantOpen: true},
		{name: "path-and-closed-text", wait: "after new-closed, see git/other/docs/tasks/x-y.md"},
		{name: "uncontextual-dot-slug-and-closed-text", wait: "open-one. Then after new-closed", wantOpen: true},
		{name: "terminal-dot-word-and-closed-text", wait: "after new-closed, answer API."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			project := t.TempDir()
			dir := filepath.Join(project, "docs", "tasks")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.wantOpen {
				writeRunMainTask(t, dir, "20261101-1000-MED-open-one.md", "# Open\n\nСтатус: открыт\n")
			}
			if strings.Contains(tc.wait, "new-closed") {
				writeRunMainTask(t, dir, "20261101-1001-MED-new-closed.md", "# Closed\n\nСтатус: готово\n\n## Журнал\n- 2026-11-21 10:00 готово — checker: closed\n")
			}
			body := "Статус: ждёт\nЖдёт: " + tc.wait + "\n"
			if tc.blocker != "" {
				body = "Статус: ждёт\nЗаблокирован: " + tc.blocker + "\nЖдёт: " + tc.wait + "\n"
			}
			waitPath := writeRunMainTask(t, dir, "20261101-1100-MED-waiting.md", "# Waiting\n\n"+body+"\n## Журнал\n- 2026-11-20 10:00 ждёт — tester: waiting\n")
			tasks, err := loadTasks(dir, true)
			if err != nil {
				t.Fatal(err)
			}
			idx := newWaitReferenceIndex()
			mustIndexWaitTasks(t, idx, tasks)
			for _, task := range tasks {
				if task.path != waitPath {
					continue
				}
				task.waitIndex = idx
				got, err := waitConditionReachedErr(task)
				if err != nil || got {
					t.Fatalf("wait condition = %v, err=%v; want asleep", got, err)
				}
				return
			}
			t.Fatal("waiting task not loaded")
		})
	}
}
