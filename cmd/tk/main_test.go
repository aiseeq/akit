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
