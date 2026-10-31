package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseOptions(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantPos []string
		want    options
		wantErr string
	}{
		{name: "values and switches", args: []string{"--title", "A task", "--all", "--due", "--json", "--root", "/repo", "task"}, wantPos: []string{"task"}, want: options{title: "A task", all: true, due: true, json: true, root: "/repo"}},
		{name: "flag missing value", args: []string{"--title"}, wantErr: "--title needs one value"},
		{name: "flag followed by flag", args: []string{"--title", "--all"}, wantErr: "--title needs one value"},
		{name: "duplicate value flag", args: []string{"--by", "one", "--by", "two"}, wantErr: "--by needs one value"},
		{name: "unknown flag", args: []string{"--unknown"}, wantErr: "unknown flag --unknown"},
		{name: "older missing value", args: []string{"--older"}, wantErr: "--older needs a value"},
		{name: "older duration", args: []string{"--older", "2d"}, want: options{older: 48 * time.Hour}},
		{name: "positionals retained", args: []string{"one", "two"}, wantPos: []string{"one", "two"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := options{}
			pos, err := parseOptions(tt.args, &got)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(pos, tt.wantPos) {
				t.Fatalf("parseOptions = %q, %v; want %q", pos, err, tt.wantPos)
			}
			if got.title != tt.want.title || got.all != tt.want.all || got.due != tt.want.due || got.json != tt.want.json || got.root != tt.want.root || got.older != tt.want.older {
				t.Fatalf("options = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestValidateCommand(t *testing.T) {
	tests := []struct {
		name                      string
		o                         options
		pos                       []string
		wantID, wantNote, wantErr string
	}{
		{name: "note assigns arguments", o: options{cmd: "note"}, pos: []string{"ticket", "progress"}, wantID: "ticket", wantNote: "progress"},
		{name: "note argument count", o: options{cmd: "note"}, pos: []string{"ticket"}, wantErr: "note needs a ticket ID and text"},
		{name: "note invalid id", o: options{cmd: "note"}, pos: []string{"Bad ID", "x"}, wantErr: "ID is the ticket slug (e.g. graft-hooks) or its file name"},
		{name: "note empty text", o: options{cmd: "note"}, pos: []string{"ticket", "  "}, wantErr: "note text must not be empty"},
		{name: "too many args", o: options{cmd: "show"}, pos: []string{"a", "b"}, wantErr: "too many arguments"},
		{name: "ticket id", o: options{cmd: "show"}, pos: []string{"ticket"}, wantID: "ticket"},
		{name: "ticket action invalid id", o: options{cmd: "start"}, pos: []string{"Bad ID"}, wantErr: "ID is the ticket slug (e.g. graft-hooks) or its file name"},
		{name: "unknown command", o: options{cmd: "other"}, wantErr: "unknown command \"other\""},
		{name: "lint positional", o: options{cmd: "lint"}, pos: []string{"id"}, wantErr: "lint takes no id"},
		{name: "archive positional", o: options{cmd: "archive"}, pos: []string{"id"}, wantErr: "archive takes no id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCommand(&tt.o, tt.pos)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || tt.o.id != tt.wantID || tt.o.note != tt.wantNote {
				t.Fatalf("result = %+v, %v", tt.o, err)
			}
		})
	}
}

func TestValidateNew(t *testing.T) {
	tests := []struct {
		name              string
		o                 options
		pos               []string
		wantPrio, wantErr string
	}{
		{name: "default priority", o: options{id: "new-task"}, pos: []string{"new-task"}, wantPrio: "MEDIUM"},
		{name: "all priorities", o: options{id: "new-task", prio: "HIGH"}, pos: []string{"new-task"}, wantPrio: "HIGH"},
		{name: "bad slug", o: options{id: "Bad"}, pos: []string{"Bad"}, wantErr: "new needs a lowercase slug"},
		{name: "extra positional", o: options{id: "new-task"}, pos: []string{"new-task", "extra"}, wantErr: "new needs a lowercase slug"},
		{name: "bad priority", o: options{id: "new-task", prio: "urgent"}, pos: []string{"new-task"}, wantErr: "--prio must be HIGH, MEDIUM, or LOW"},
		{name: "bad actor", o: options{id: "new-task", by: "two words"}, pos: []string{"new-task"}, wantErr: "--by must be a name without spaces; use --by NAME"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNew(&tt.o, tt.pos)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || tt.o.prio != tt.wantPrio {
				t.Fatalf("priority = %q, error = %v", tt.o.prio, err)
			}
		})
	}
}

func TestNewPlanSectionIsOptionalAndTextAddsItBeforeJournal(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "tasks")
	var out, errOut bytes.Buffer
	if code := create(root, dir, options{id: "no-plan", by: "worker", prio: "MEDIUM"}, &out, &errOut); code != 0 {
		t.Fatalf("new without plan: %d %s", code, errOut.String())
	}
	plain, err := os.ReadFile(filepath.Join(root, strings.TrimSpace(out.String())))
	if err != nil || strings.Contains(string(plain), "## План") {
		t.Fatalf("new without plan contains a plan section: %s, %v", plain, err)
	}
	out.Reset()
	errOut.Reset()
	if code := create(root, dir, options{id: "with-plan", by: "worker", prio: "MEDIUM", text: "## План\n- build: go build ./cmd/tk"}, &out, &errOut); code != 0 {
		t.Fatalf("new with plan text: %d %s", code, errOut.String())
	}
	withPlan, err := os.ReadFile(filepath.Join(root, strings.TrimSpace(out.String())))
	if err != nil || !strings.Contains(string(withPlan), "## План\n- build: go build ./cmd/tk\n\n## Журнал") {
		t.Fatalf("--text plan placement: %s, %v", withPlan, err)
	}
}

func TestValidateList(t *testing.T) {
	tests := []struct {
		name    string
		o       options
		pos     []string
		wantErr string
	}{
		{name: "filters accepted", o: options{status: "готово", by: "agent", slug: "Task-"}},
		{name: "positional rejected", pos: []string{"id"}, wantErr: "ls takes no id"},
		{name: "bad status", o: options{status: "unknown"}, wantErr: "unknown status \"unknown\""},
		{name: "bad actor", o: options{by: "two words"}, wantErr: "--by must be a name without spaces; use --by NAME"},
		{name: "bad slug prefix", o: options{slug: "Bad/"}, wantErr: "--slug must be a lowercase slug prefix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateList(&tt.o, tt.pos)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDuration(t *testing.T) {
	tests := []struct {
		input   string
		want    time.Duration
		wantErr string
	}{
		{input: "2h30m", want: 150 * time.Minute}, {input: "1.5d", want: 36 * time.Hour}, {input: "0s", want: 0},
		{input: "0d", want: 0}, {input: "-1h", wantErr: "invalid duration \"-1h\""}, {input: "-1d", wantErr: "invalid duration \"-1d\""},
		{input: "nope", wantErr: "invalid duration \"nope\""}, {input: "1x", wantErr: "invalid duration \"1x\""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := duration(tt.input)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("duration = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

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

func TestPlanParseAddSkipCheckAndJSON(t *testing.T) {
	for _, args := range [][]string{
		{"plan", "ticket", "--add", "Bad_Name: true", "--by", "worker"},
		{"plan", "ticket", "--add", "valid:   ", "--by", "worker"},
		{"skip", "ticket", "valid", "--by", "worker"},
		{"plan", "ticket", "--add", "one: true\n- two: false", "--by", "worker"},
		{"skip", "ticket", "valid", "--why", "a\nb", "--by", "worker"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%q) accepted invalid plan arguments", args)
		}
	}
	if _, err := parseArgs([]string{"plan", "ticket", "--add", "valid: true", "--by", "worker"}); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	dir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "20200101-1200-MEDIUM-task.md")
	body := "# Task\n\nСтатус: в работе\n\n## План\n\n## Журнал\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	ticket := task{path: path, slug: "task", body: body}
	var out, errOut bytes.Buffer
	if code := addPlanItem(ticket, options{by: "worker", add: "present: test -f marker"}, &out, &errOut); code != 0 {
		t.Fatalf("add plan: %d %s", code, errOut.String())
	}
	updated, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(updated), "## План\n- present: test -f marker\n\n## Журнал") || !strings.Contains(string(updated), "план — worker: добавлен пункт present") {
		t.Fatalf("plan insertion: %s, %v", updated, err)
	}
	if code := addPlanItem(task{path: path, slug: "task", body: string(updated)}, options{by: "worker", add: "present: true"}, &out, &errOut); code != exitUsage {
		t.Fatalf("duplicate plan entry returned %d", code)
	}
	if err := os.WriteFile(filepath.Join(root, "marker"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	if code := checkPlan(root, task{path: path, body: string(updated)}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "present: ok") {
		t.Fatalf("check plan: %d %q %s", code, out.String(), errOut.String())
	}
	if code := skipPlanItem(task{path: path, slug: "task", body: string(updated)}, options{by: "planner", item: "present", why: "covered elsewhere"}, &out, &errOut); code != 0 {
		t.Fatalf("skip plan: %d %s", code, errOut.String())
	}
	skipped, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(skipped), "план — planner: пропущен: present — covered elsewhere") {
		t.Fatalf("skip journal line without author:\n%s", skipped)
	}
	items := planItems(string(skipped))
	if len(items) != 1 || !items[0].Skipped || items[0].Why != "covered elsewhere" {
		t.Fatalf("skip state = %#v", items)
	}
	out.Reset()
	if code := checkPlan(root, task{path: path, body: string(skipped)}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "present: skipped — covered elsewhere") {
		t.Fatalf("check skipped: %d %q %s", code, out.String(), errOut.String())
	}
	obj := taskObject(root, task{path: path, slug: "task", body: string(skipped)}, "", false)
	if len(obj.Plan) != 1 || !obj.Plan[0].Skipped || obj.Plan[0].Cmd != "test -f marker" {
		t.Fatalf("plan JSON data = %#v", obj.Plan)
	}
}

func TestInvalidPlanItemAppearsInJSON(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "tasks", "20200101-1200-MEDIUM-task.md")
	body := "# Task\n\n## План\n- Bad_Name: go test ./...\n\n## Журнал\n"
	obj := taskObject(root, task{path: path, slug: "task", body: body}, "", false)
	if len(obj.Plan) != 1 || obj.Plan[0].Name != "Bad_Name" || obj.Plan[0].Cmd != "go test ./..." || obj.Plan[0].Invalid == "" {
		t.Fatalf("invalid plan item = %#v", obj.Plan)
	}
	data, err := json.Marshal(obj)
	if err != nil || !strings.Contains(string(data), `"invalid":"`) {
		t.Fatalf("invalid plan JSON = %s, %v", data, err)
	}
	repeated := planItems("# Task\n\n## План\n- build: true\n- build: false\n\n## Журнал\n")
	if len(repeated) != 2 || repeated[0].Invalid != "" || !strings.Contains(repeated[1].Invalid, "appears more than once") {
		t.Fatalf("repeated plan name in file = %#v", repeated)
	}
}

func TestNewTextAcceptsAndValidatesPlan(t *testing.T) {
	valid := fillTicket("# Task\n\nСтатус: открыт\n\n## Что сделать\n\n## Приёмка\n\n## План\n\n## Журнал\n", "## План\n- build: go build ./cmd/tk\n")
	if err := validatePlanSection(valid); err != nil || !strings.Contains(valid, "## План\n- build: go build ./cmd/tk\n") {
		t.Fatalf("valid --text plan = %q, %v", valid, err)
	}
	duplicate := fillTicket("# Task\n\nСтатус: открыт\n\n## План\n\n## Журнал\n", "## План\n- build: go build\n- build: go test\n")
	if err := validatePlanSection(duplicate); err == nil || !strings.Contains(err.Error(), "appears more than once") {
		t.Fatalf("duplicate --text plan accepted: %v", err)
	}
}

func TestPlanFailureBlocksDoneAndReportsFirstStderrLine(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "20200101-1200-MEDIUM-task.md")
	body := "# Task\n\nСтатус: на проверке\nИсполнитель: worker\n\n## Приёмка\ncheck\n\n## План\n- broken: echo first >&2; echo second >&2; exit 7\n\n## Журнал\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := transition(task{path: path, slug: "task", body: body}, options{cmd: "done", by: "reviewer", check: "verified", baseRoot: root}, &out, &errOut)
	updated, err := os.ReadFile(path)
	if err != nil || code != exitNone || string(updated) != body || !strings.Contains(errOut.String(), "broken: exit 7, first") || strings.Contains(errOut.String(), "second") {
		t.Fatalf("done failure code=%d stderr=%q body=%q readErr=%v", code, errOut.String(), updated, err)
	}
}

func TestPlanTimeoutKillsProcessGroup(t *testing.T) {
	oldTimeout := planTimeout
	planTimeout = 30 * time.Millisecond
	t.Cleanup(func() { planTimeout = oldTimeout })
	result := runPlanItem(t.TempDir(), planJSON{Name: "slow", Cmd: "sleep 5 & wait"})
	if !result.timedOut {
		t.Fatalf("expected timeout, got %#v", result)
	}
}

func TestPlanItemDoesNotWaitForLeftoverChild(t *testing.T) {
	oldTimeout := planTimeout
	planTimeout = 5 * time.Second
	t.Cleanup(func() { planTimeout = oldTimeout })
	dir := t.TempDir()
	start := time.Now()
	result := runPlanItem(dir, planJSON{Name: "bg", Cmd: "sleep 3 & echo $! >pid; echo first >&2"})
	if elapsed := time.Since(start); elapsed > time.Second || result.exit != 0 || result.timedOut || result.stderr != "first" {
		t.Fatalf("leftover child: %v %#v", elapsed, result)
	}
	data, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("leftover child %d still alive", pid)
		}
	}
}

func TestFillTicketPreservesSectionsAndJournal(t *testing.T) {
	template := "# Example\n\nСтатус: открыт\n\n## Что сделать\n\n## Приёмка\n\n## Журнал\n- time открыт — actor\n"
	input := "# Example\n## Что сделать\n1. Сделать раз.\n\n## Приёмка\n- graft gate\n\n## Ссылки\n- doc\n"
	body := fillTicket(template, input)
	if strings.Contains(body, "## Вход") {
		t.Errorf("«Вход» без текста для него:\n%s", body)
	}
	for _, want := range []string{"1. Сделать раз.", "## Приёмка\n- graft gate", "## Ссылки\n- doc\n\n## Журнал", "- time открыт — actor"} {
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
	body := "# Closed\n\nСтатус: готово\nИсполнитель: worker\n\n## Приёмка\n- go test\n\n## Журнал\n- 2020-01-01 12:00 готово — reviewer — go test зелёный\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if issues, err := lintTicket(p, "closed"); err != nil || len(issues) != 0 {
		t.Fatalf("строка «готово — кто — проверка» прежних версий: %v %v", issues, err)
	}
}

func TestFillTicketOldSectionNames(t *testing.T) {
	template := "# T\n\nСтатус: открыт\n\n## Что сделать\n\n## Приёмка\n\n## Журнал\n- открыт\n"
	got := fillTicket(template, "## Постановка\nсуть\n\n## Что сделать\n1. шаг\n\n## Чем проверить\n- тест\n")
	want := "# T\n\nСтатус: открыт\n\n## Вход\nсуть\n\n## Что сделать\n1. шаг\n\n## Приёмка\n- тест\n\n## Журнал\n- открыт\n"
	if got != want {
		t.Fatalf("fillTicket:\n%s\nждали:\n%s", got, want)
	}
	if noCheck(got) {
		t.Error("заполненная «Приёмка» помечена no-check")
	}
	if !noCheck("## Приёмка\n\n## Чем проверить\n\n## Журнал\n") || noCheck("## Приёмка\n\n## Чем проверить\n- тест\n") {
		t.Error("noCheck судит по первому разделу, а не по всем")
	}
}

func TestLintEmptyCheck(t *testing.T) {
	p := filepath.Join(t.TempDir(), "20200101-1200-LOW-empty.md")
	if err := os.WriteFile(p, []byte("# E\n\nСтатус: открыт\n\n## Приёмка\n\n## Журнал\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if issues, err := lintTicket(p, "empty"); err != nil || len(issues) != 1 || !strings.Contains(issues[0], "empty Приёмка") {
		t.Fatalf("lint пустой «Приёмки»: %v %v", issues, err)
	}
}

func TestLintBadPlan(t *testing.T) {
	p := filepath.Join(t.TempDir(), "20200101-1200-LOW-plan.md")
	if err := os.WriteFile(p, []byte("# P\n\nСтатус: открыт\n\n## Приёмка\n- x\n\n## План\n- Bad: true\n\n## Журнал\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if issues, err := lintTicket(p, "plan"); err != nil || len(issues) != 1 || !strings.Contains(issues[0], "plan: plan item needs") {
		t.Fatalf("lint плохого плана: %v %v", issues, err)
	}
}

func TestDoneSessionAndEmptyAcceptance(t *testing.T) {
	oldSession := session
	session = func() string { return "checker-session" }
	t.Cleanup(func() { session = oldSession })
	path := filepath.Join(t.TempDir(), "20261001-1200-MEDIUM-session-check.md")
	body := "# Check\n\nСтатус: на проверке\nИсполнитель: worker\n\n## Приёмка\n- проверка\n\n## Журнал\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	ticket := task{path: path, slug: "session-check", body: body}
	if code := transition(ticket, options{cmd: "done", by: "reviewer", check: "verified"}, &out, &errOut); code != 0 {
		t.Fatalf("done returned %d: %s", code, errOut.String())
	}
	updated, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(updated), "готово — reviewer (session checker-session): verified") {
		t.Fatalf("journal lacks checker session: %s, %v", updated, err)
	}
	closed, err := find(filepath.Dir(path), "session-check")
	if err != nil {
		t.Fatal(err)
	}
	obj := taskObject(filepath.Dir(filepath.Dir(filepath.Dir(path))), closed, "", false)
	if obj.Checker != "reviewer" || obj.Check != "verified" || obj.CheckerSession != "checker-session" {
		t.Fatalf("completion fields: %#v", obj)
	}

	oldBody := "# Empty\n\nСтатус: на проверке\nИсполнитель: worker\n\n## Приёмка\n\n## Журнал\n"
	emptyPath := filepath.Join(filepath.Dir(path), "20261001-1200-MEDIUM-empty-check.md")
	if err := os.WriteFile(emptyPath, []byte(oldBody), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := transition(task{path: emptyPath, slug: "empty-check", body: oldBody}, options{cmd: "done", by: "reviewer", check: "verified"}, &out, &errOut); code != exitNone || errOut.String() != "tk: empty Приёмка: fill it before done\n" {
		t.Fatalf("empty acceptance result %d: %q", code, errOut.String())
	}
	unchanged, err := os.ReadFile(emptyPath)
	if err != nil || string(unchanged) != oldBody {
		t.Fatalf("empty acceptance ticket changed: %s, %v", unchanged, err)
	}
}

// Тесты не видят сессию машины, на которой идут; нужная тесту — подменой session.
func init() { session = func() string { return "" } }

func TestDoneCheckerSession(t *testing.T) {
	oldSession := session
	session = func() string { return "Worker" }
	t.Cleanup(func() { session = oldSession })
	path := filepath.Join(t.TempDir(), "20261001-1200-MEDIUM-self-check.md")
	body := "# Check\n\nСтатус: на проверке\nИсполнитель: worker\n\n## Приёмка\n- check\n\n## Журнал\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := transition(task{path: path, slug: "self-check", body: body}, options{cmd: "done", by: "reviewer", check: "verified"}, &out, &errOut); code != 0 || !strings.Contains(errOut.String(), "executor's session Worker") {
		t.Fatalf("done result %d: %q", code, errOut.String())
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), " готово — reviewer (session Worker): verified") {
		t.Fatalf("no checker session in journal:\n%s", updated)
	}
	if checker, check, s := completion(string(updated)); checker != "reviewer" || check != "verified" || s != "Worker" {
		t.Fatalf("completion = %q %q %q", checker, check, s)
	}
	// Сессия проверяющего — данные, не находка: свежий суб-агент живёт в сессии исполнителя.
	if issues, err := lintTicket(path, "self-check.md"); err != nil || len(issues) != 0 {
		t.Fatalf("lint: %v %v", issues, err)
	}
	legacy := strings.Replace(string(updated), " (session Worker)", "", 1)
	if checker, _, s := completion(legacy); checker != "reviewer" || s != "" {
		t.Fatalf("legacy completion = %q %q", checker, s)
	}
}

func TestArchiveRootAndListSlugJSON(t *testing.T) {
	base := t.TempDir()
	for _, repo := range []string{"one", "two"} {
		dir := filepath.Join(base, repo, "docs", "tasks")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		name := "20200101-1200-LOW-archive-" + repo + ".md"
		body := "# Closed\n\nСтатус: готово\n\n## Приёмка\n- check\n\n## Журнал\n- 2020-01-01 12:00 готово — reviewer: verified\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	if code := archive("", "", options{root: base, older: time.Hour}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "archived 2") || !strings.Contains(out.String(), "one/docs/tasks-archive/") || !strings.Contains(out.String(), "two/docs/tasks-archive/") {
		t.Fatalf("root archive %d: %q %s", code, out.String(), errOut.String())
	}

	listDir := filepath.Join(base, "listing", "docs", "tasks")
	if err := os.MkdirAll(listDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"alpha-one", "alpha-two", "beta-one"} {
		name := "20200101-1200-LOW-" + slug + ".md"
		body := "# Ticket\n\nСтатус: готово\n\n## Приёмка\n- check\n\n## Журнал\n- 2020-01-01 12:00 готово — checker (session inspect): verified\n"
		if err := os.WriteFile(filepath.Join(listDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out.Reset()
	if code := list("", options{root: base, all: true, json: true, slug: "ALPHA"}, &out, &errOut); code != 0 {
		t.Fatalf("slug JSON list %d: %s", code, errOut.String())
	}
	var rows []taskJSON
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 2 {
		t.Fatalf("slug JSON rows %#v: %v; %s", rows, err, out.String())
	}
	for _, row := range rows {
		if !strings.HasPrefix(row.Slug, "alpha") || row.Checker != "checker" || row.Check != "verified" || row.CheckerSession != "inspect" {
			t.Fatalf("slug JSON completion fields: %#v", row)
		}
	}
}
