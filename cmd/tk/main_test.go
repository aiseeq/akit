package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNoteListJSONAndDue(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "docs", "tasks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	past := "# Waiting\n\nСтатус: ждёт\nЖдёт: 2020-01-01 10:00\nИсполнитель: Agent\n\n## Приёмка\n\n## Журнал\n- 2020-01-01 10:00 открыт — Agent\n"
	future := "# Future\n\nСтатус: ждёт\nЖдёт: 2099-01-01 10:00\n\n## Приёмка\n- check\n\n## Журнал\n- 2020-01-01 10:00 открыт — Other\n"
	for name, body := range map[string]string{
		"20200101-1000-MEDIUM-waiting.md": past,
		"20200101-1000-INFO-future.md":    future,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	if code := list("", options{cmd: "ls", root: base, due: true}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "waiting") || strings.Contains(out.String(), "future") {
		t.Fatalf("due listing %d: %q %s", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := list("", options{cmd: "ls", root: base, by: "aGENT", json: true}, &out, &errOut); code != 0 {
		t.Fatalf("JSON listing %d: %s", code, errOut.String())
	}
	var rows []taskJSON
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].Slug != "waiting" || !rows[0].Overdue || !rows[0].NoCheck || rows[0].Repo == "" {
		t.Fatalf("unexpected JSON list %#v, err=%v, raw=%s", rows, err, out.String())
	}
	ticket, err := find(dir, "waiting")
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := addNote(ticket, options{by: "Agent", note: "progress recorded"}, &out, &errOut); code != 0 {
		t.Fatalf("note %d: %s", code, errOut.String())
	}
	updated, err := os.ReadFile(ticket.path)
	if err != nil || !strings.Contains(string(updated), "note — Agent: progress recorded") || !strings.Contains(string(updated), "Статус: ждёт") {
		t.Fatalf("note changed wrong content: %s, %v", updated, err)
	}
}

func TestLintReportsRulesAndLegacyScope(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"bad-name.md":                   "# malformed\nСтатус: strange\n",
		"20200101-1000-MEDIUM-ready.md": "# Ready\nСтатус: готово\nИсполнитель: Same\n\n## Приёмка\ncheck\n\n## Журнал\n- 2020-01-01 10:00 готово — same: checked\n",
		"20200101-1000-INFO-legacy.md":  "# Legacy\n## Incomplete\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	if code := lint(root, "", &out, &errOut); code != 1 || !strings.Contains(out.String(), "bad-name.md: invalid file name") || !strings.Contains(out.String(), "unknown status strange") || !strings.Contains(out.String(), "checker equals executor") || strings.Contains(out.String(), "legacy.md: missing") {
		t.Fatalf("lint result %d: %q; err=%q", code, out.String(), errOut.String())
	}
}

func TestOutsideRepositoryBaseSearch(t *testing.T) {
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	oldHome := os.Getenv("HOME")
	base := t.TempDir()
	home := filepath.Join(base, "home")
	work := filepath.Join(home, "work", "nested")
	if err := os.MkdirAll(filepath.Join(home, "docs", "tasks"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldWD)
		_ = os.Setenv("HOME", oldHome)
	})
	root, dir, found, err := resolveTaskBase()
	if err != nil || !found || root != home || dir != filepath.Join(home, "docs", "tasks") {
		t.Fatalf("resolved base %q %q %v %v", root, dir, found, err)
	}
}

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

func TestDoneGlintBlocksAndDebtAllows(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".glint.yaml"), []byte("rules: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	glint := filepath.Join(bin, "glint")
	script := "#!/bin/sh\nprintf '%s' '{\"summary\":{\"total\":1},\"issues\":[{\"rule\":\"sample-rule\",\"severity\":\"low\",\"file\":\"sample.go\",\"line\":7,\"message\":\"synthetic\"}]}'\n"
	if err := os.WriteFile(glint, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := filepath.Join(dir, "20200101-1200-MEDIUM-change.md")
	body := "# Change\n\nСтатус: на проверке\nИсполнитель: worker\n\n## Приёмка\ncheck\n\n## Журнал\n- 2020-01-01 12:00 открыт — worker\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	ticket, err := find(dir, "change")
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	options := options{cmd: "done", by: "reviewer", check: "synthetic check", baseRoot: root}
	if code := transition(ticket, options, &out, &errOut); code != exitNone || !strings.Contains(errOut.String(), "1 findings") || !strings.Contains(errOut.String(), "low sample.go:7 sample-rule") {
		t.Fatalf("glint should block completion: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != body {
		t.Fatalf("blocked completion changed ticket: %v", err)
	}
	debt := filepath.Join(dir, "20200101-1200-LOW-debt.md")
	if err := os.WriteFile(debt, []byte("# Debt\n\nСтатус: открыт\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	options.glintDebt = "debt"
	if code := transition(ticket, options, &out, &errOut); code != 0 {
		t.Fatalf("debt completion failed: code=%d err=%q", code, errOut.String())
	}
	updated, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(updated), "готово — reviewer: synthetic check; glint: 1 findings, debt debt") {
		t.Fatalf("missing debt record: %s, %v", updated, err)
	}
}

func TestDoneGlintInvalidOutputDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".glint.yaml"), []byte("rules: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "glint"), []byte("#!/bin/sh\necho unavailable >&2\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := filepath.Join(t.TempDir(), "20200101-1200-MEDIUM-change.md")
	body := "# Change\n\nСтатус: на проверке\nИсполнитель: worker\n\n## Журнал\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := transition(task{path: path, slug: "change", body: body}, options{cmd: "done", by: "reviewer", check: "check", baseRoot: root}, &out, &errOut)
	updated, err := os.ReadFile(path)
	if code != exitUsage || err != nil || string(updated) != body || !strings.Contains(errOut.String(), "glint check failed") {
		t.Fatalf("invalid glint result: code=%d err=%q body=%q readErr=%v", code, errOut.String(), updated, err)
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

func TestLintAcceptsEarlierDoneLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "20200101-1200-LOW-closed.md")
	body := "# Closed\n\nСтатус: готово\nИсполнитель: worker\n\n## Приёмка\n\n## Журнал\n- 2020-01-01 12:00 готово — reviewer — go test зелёный\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if issues, err := lintTicket(p, "closed"); err != nil || len(issues) != 0 {
		t.Fatalf("строка «готово — кто — проверка» прежних версий: %v %v", issues, err)
	}
}
