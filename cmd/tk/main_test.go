package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLegacyTicketAndTextPreservation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "20200101-1200-LOW-old-ticket.md")
	original := "# Старый заголовок\n\n## Что известно\nНе менять этот текст.\n\n## Вход\nПример.\n"
	if e := os.WriteFile(p, []byte(original), 0644); e != nil {
		t.Fatal(e)
	}
	ts, e := load(dir)
	if e != nil || len(ts) != 1 || ts[0].status != "открыт" || ts[0].title != "Старый заголовок" {
		t.Fatalf("legacy parse: %#v, %v", ts, e)
	}
	var out, errOut bytes.Buffer
	if code := transition(ts[0], options{cmd: "review", by: "reviewer"}, &out, &errOut); code != 0 {
		t.Fatalf("transition %d: %s", code, errOut.String())
	}
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	got := string(b)
	for _, want := range []string{"Статус: на проверке", "Исполнитель: reviewer", "Не менять этот текст.", "Пример.", "## Журнал"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
}

func TestStartSetsExecutor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "20200101-1200-MEDIUM-new.md")
	body := "# New\n\nСтатус: открыт\n\n## Постановка\nText.\n"
	if err := os.WriteFile(path, []byte(body), fileMode); err != nil {
		t.Fatal(err)
	}
	ticket := task{path: path, slug: "new", status: "открыт", body: body}
	var out, errOut bytes.Buffer
	if code := transition(ticket, options{cmd: "start", by: "worker"}, &out, &errOut); code != 0 {
		t.Fatalf("start returned %d: %s", code, errOut.String())
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "Исполнитель: worker") {
		t.Fatalf("executor was not set: %s", updated)
	}
}

func TestArchiveUsesLastJournalEntry(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "20200101-1200-HIGH-closed.md")
	body := "# Закрыта\nСтатус: готово\n\n## Журнал\n- 2020-01-01 12:00 открыт — a\n- 2020-01-03 12:00 готово — b\n"
	if e := os.WriteFile(p, []byte(body), 0644); e != nil {
		t.Fatal(e)
	}
	ts, e := load(dir)
	if e != nil {
		t.Fatal(e)
	}
	want := time.Date(2020, 1, 3, 12, 0, 0, 0, time.Local)
	if !ts[0].changed.Equal(want) {
		t.Fatalf("changed %v, want %v", ts[0].changed, want)
	}
}

func TestListRoot(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "sample")
	dir := filepath.Join(repo, "docs", "tasks")
	if e := os.MkdirAll(dir, 0755); e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(dir, "20200101-1200-MEDIUM-task-a.md")
	if e := os.WriteFile(p, []byte("# A task\nСтатус: открыт\n"), 0644); e != nil {
		t.Fatal(e)
	}
	var out, errOut bytes.Buffer
	o := options{cmd: "ls", root: base}
	if code := list(repo, o, &out, &errOut); code != 0 {
		t.Fatalf("list %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "sample task-a открыт MEDIUM") {
		t.Fatalf("output %q", out.String())
	}
}

func TestBadStatusIsError(t *testing.T) {
	dir := t.TempDir()
	if e := os.WriteFile(filepath.Join(dir, "20200101-1200-LOW-bad.md"), []byte("# Bad\n\nСтатус: закрыт\n"), 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := load(dir); e == nil || !strings.Contains(e.Error(), "закрыт") {
		t.Fatalf("неизвестный статус прочитан без ошибки: %v", e)
	}
}

func TestListRootIncludesBase(t *testing.T) {
	base := t.TempDir()
	if e := os.MkdirAll(filepath.Join(base, "docs", "tasks"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(base, "docs", "tasks", "20200101-1200-LOW-own.md"), []byte("# Own\n"), 0644); e != nil {
		t.Fatal(e)
	}
	var out, errOut bytes.Buffer
	if code := list("", options{cmd: "ls", root: base}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "own открыт LOW") {
		t.Fatalf("list %d: %q %s", code, out.String(), errOut.String())
	}
}

func TestTicketIDFormsAndInvalidHint(t *testing.T) {
	if _, err := parseArgs([]string{"start", "bad id", "--by", "worker"}); err == nil || !strings.Contains(err.Error(), "ID is the ticket slug") {
		t.Fatalf("invalid ID hint: %v", err)
	}
	dir := t.TempDir()
	name := "20200101-1200-INFO-good-ticket.md"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("# Ticket\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"good-ticket", name, strings.TrimSuffix(name, ".md")} {
		if _, err := find(dir, id); err != nil {
			t.Errorf("find %q: %v", id, err)
		}
	}
}

func TestWaitReplaceAndStartClearsWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "20200101-1200-MEDIUM-waiting.md")
	body := "# Waiting\n\nСтатус: открыт\n\n## Что сделать\nне менять\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	for _, until := range []string{"2020-01-01 10:00", "после ответа"} {
		var out, errOut bytes.Buffer
		if code := transition(task{path: path, slug: "waiting", body: body}, options{cmd: "wait", by: "worker", until: until}, &out, &errOut); code != 0 {
			t.Fatalf("wait %d: %s", code, errOut.String())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		body = string(data)
		if strings.Count(body, "Ждёт:") != 1 || !strings.Contains(body, "Статус: ждёт\nЖдёт: "+until) {
			t.Fatalf("wait did not replace adjacent field: %s", body)
		}
	}
	var out, errOut bytes.Buffer
	if code := transition(task{path: path, slug: "waiting", body: body}, options{cmd: "start", by: "worker"}, &out, &errOut); code != 0 {
		t.Fatalf("start %d: %s", code, errOut.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Ждёт:") || !strings.Contains(string(data), "Статус: в работе") {
		t.Fatalf("start left wait field or status: %s", data)
	}
	if _, err := parseArgs([]string{"wait", "waiting", "--by", "worker"}); err == nil {
		t.Fatal("wait without --until accepted")
	}
	legacy := filepath.Join(t.TempDir(), "20200101-1200-LOW-legacy-wait.md")
	if err := os.WriteFile(legacy, []byte("# Legacy\n## Что сделать\ntext\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var legacyOut, legacyErr bytes.Buffer
	if code := transition(task{path: legacy, slug: "legacy-wait", body: "# Legacy\n## Что сделать\ntext\n"}, options{cmd: "wait", by: "worker", until: "after response"}, &legacyOut, &legacyErr); code != 0 {
		t.Fatalf("legacy wait %d: %s", code, legacyErr.String())
	}
	legacyData, err := os.ReadFile(legacy)
	if err != nil || !strings.Contains(string(legacyData), "Статус: ждёт\nЖдёт: after response") {
		t.Fatalf("legacy wait field not inserted: %s, %v", legacyData, err)
	}
	waitingTask := task{status: "ждёт", body: "Статус: ждёт\nЖдёт: 2020-01-01 10:00"}
	if !includeTask(waitingTask, options{cmd: "ls"}) {
		t.Fatal("default ls hides waiting tickets")
	}
	out.Reset()
	printTask(&out, waitingTask, "")
	if !strings.Contains(out.String(), "[до 2020-01-01 10:00 overdue]") {
		t.Fatalf("overdue wait not shown: %q", out.String())
	}
}

func TestNamesAndLegacyExecutor(t *testing.T) {
	if _, err := parseArgs([]string{"start", "ticket", "--by", "two words"}); err == nil {
		t.Fatal("--by with spaces accepted")
	}
	if _, err := parseArgs([]string{"done", "ticket", "--check", "verified", "--doer", "two words"}); err == nil {
		t.Fatal("--doer with spaces accepted")
	}
	path := filepath.Join(t.TempDir(), "20200101-1200-LOW-legacy.md")
	body := "# Legacy\nСтатус: в работе\nИсполнитель: legacy executor\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := transition(task{path: path, slug: "legacy", body: body}, options{cmd: "done", check: "verified", by: "reviewer"}, &out, &errOut)
	if code != exitNone || !strings.Contains(errOut.String(), "--doer NAME") {
		t.Fatalf("legacy executor result %d %q", code, errOut.String())
	}
	code = transition(task{path: path, slug: "legacy", body: "# Legacy\nИсполнитель: Worker\n"}, options{cmd: "done", check: "verified", by: "worker"}, &out, &errOut)
	if code != exitNone || !strings.Contains(errOut.String(), "differ") {
		t.Fatalf("case-insensitive reviewer comparison: %d %q", code, errOut.String())
	}
}

func TestListSkipsLinkedWorktreesAndReportsBadNames(t *testing.T) {
	base := t.TempDir()
	for _, repo := range []string{"regular", "linked", "base-linked"} {
		path := base
		if repo != "base-linked" {
			path = filepath.Join(base, repo)
		}
		dir := filepath.Join(path, "docs", "tasks")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if repo != "regular" {
			if err := os.WriteFile(filepath.Join(path, ".git"), []byte("gitdir: synthetic\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		name := "20200101-1200-INFO-" + repo + ".md"
		if err := os.WriteFile(filepath.Join(dir, name), []byte("# Task\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	badDir := filepath.Join(base, "regular", "docs", "tasks")
	if err := os.WriteFile(filepath.Join(badDir, "notes.md"), []byte("# Note\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := list("", options{cmd: "ls", root: base}, &out, &errOut); code != 0 {
		t.Fatalf("list %d: %s", code, errOut.String())
	}
	text := out.String()
	if !strings.Contains(text, "regular regular открыт INFO") || strings.Contains(text, "linked открыт") || strings.Contains(text, "base-linked открыт") {
		t.Fatalf("unexpected linked-worktree listing: %s", text)
	}
	if !strings.Contains(text, "skipped 1 files not named YYYYMMDD-HHMM-PRIO-slug.md: notes.md") {
		t.Fatalf("malformed file not reported: %s", text)
	}
}

func TestFillTicketPreservesSectionsAndJournal(t *testing.T) {
	template := "# Example\n\nСтатус: открыт\n\n## Вход\n- Проблема:\n- Метод:\n- Дешевле:\n- Инструмент:\n- Цена:\n- Если не делать:\n\n## Что сделать\n\n## Приёмка\n\n## Журнал\n- time открыт — actor\n"
	input := "# Example\n## Что сделать\n1. Сделать раз.\n\n## Приёмка\n- graft gate\n\n## Ссылки\n- doc\n"
	body := fillTicket(template, input)
	for _, want := range []string{"## Вход\n- Проблема:", "1. Сделать раз.", "## Приёмка\n- graft gate", "## Ссылки\n- doc\n\n## Журнал", "- time открыт — actor"} {
		if !strings.Contains(body, want) {
			t.Errorf("filled ticket missing %q:\n%s", want, body)
		}
	}
	if strings.Count(body, "## Приёмка") != 1 {
		t.Fatalf("filled ticket duplicated acceptance section:\n%s", body)
	}
}
