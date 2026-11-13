package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aiseeq/akit/queuegate"
	"github.com/aiseeq/akit/timearg"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
)

func TestMain(m *testing.M) {
	if err := proc.BecomeChildSubreaper(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// Without a session and without --by the actor is unknown: tk refuses rather than guessing a name.
func TestPrepareActorWithoutSessionNeedsBy(t *testing.T) {
	oldSession := session
	session = func() string { return "" }
	t.Cleanup(func() { session = oldSession })
	t.Setenv("USER", "synthetic-user")
	o := options{cmd: "note"}
	var errOut bytes.Buffer
	if code := prepareActor(&o, &errOut); code != exitUsage || o.by != "" || !strings.Contains(errOut.String(), "--by") {
		t.Fatalf("actor = %q, code=%d, stderr=%q", o.by, code, errOut.String())
	}
}

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
		{name: "default priority", o: options{id: "new-task"}, pos: []string{"new-task"}, wantPrio: "MED"},
		{name: "all priorities", o: options{id: "new-task", prio: "HIGH", why: "reason"}, pos: []string{"new-task"}, wantPrio: "HIGH"},
		{name: "bad slug", o: options{id: "Bad"}, pos: []string{"Bad"}, wantErr: "new needs a lowercase slug"},
		{name: "extra positional", o: options{id: "new-task"}, pos: []string{"new-task", "extra"}, wantErr: "new needs a lowercase slug"},
		{name: "bad priority", o: options{id: "new-task", prio: "urgent"}, pos: []string{"new-task"}, wantErr: "--prio must be CRIT, HIGH, MED, MEDIUM, or LOW"},
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

func TestSetTextWithoutSectionHeadingsFails(t *testing.T) {
	body := "# T\n\nСтатус: открыт\n\n## Что сделать\nold\n\n## Приёмка\ncheck\n\n## Журнал\n"
	path := filepath.Join(t.TempDir(), "20260101-1000-MED-t.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := setTask(task{path: path, slug: "t", status: "открыт", body: body}, options{cmd: "set", text: "ordinary prose", by: "tester"}, &out, &errOut)
	if code != exitUsage || !strings.Contains(errOut.String(), "--text has no ticket sections") {
		t.Fatalf("set prose = %d %q", code, errOut.String())
	}
}

func TestSetAddsMissingSectionsInTemplateOrder(t *testing.T) {
	body := "# T\n\nСтатус: открыт\n\n## Журнал\n- existing\n"
	text := "## План\nplan\n\n## Приёмка\ncheck\n\n## Что сделать\nwork\n\n## Вход\ncontext\n\n## Ссылки\nref\n"
	got, err := replaceExistingTicketSections(body, text)
	if err != nil {
		t.Fatal(err)
	}
	order := []string{"## Вход", "## Что сделать", "## Приёмка", "## Ссылки", "## План", "## Журнал"}
	last := -1
	for _, heading := range order {
		at := strings.Index(got, heading)
		if at <= last {
			t.Fatalf("section order not template-like at %q: %s", heading, got)
		}
		last = at
	}
}

func TestRepeatedJournalSectionsKeepCompletionAndActors(t *testing.T) {
	body := "# Closed\n\nСтатус: готово\nИсполнитель: worker\n\n## Приёмка\ncheck\n\n## Журнал\n- 2026-01-01 10:00 готово — old-checker: old check\n## Журнал\n- 2026-01-02 10:00 готово — final-checker: final check\n"
	checker, check, _ := completion(body)
	if checker != "final-checker" || check != "final check" {
		t.Fatalf("completion = %q %q", checker, check)
	}
	if got := journalLines(body); len(got) != 2 || !strings.Contains(got[1], "final-checker") {
		t.Fatalf("journal lines = %q", got)
	}
	if got := journalActors(body); got["final-checker"] == "" {
		t.Fatalf("actors omit second journal: %#v", got)
	}
}

func TestTailMergesJournalSectionsAndKeepsContinuations(t *testing.T) {
	body := "# T\n\nСтатус: готово\n\n## Журнал\n- first entry\n first continuation\n\n## Журнал\n- second entry\n second continuation\n"
	got := showTail(body, 5)
	if strings.Count(got, "## Журнал") != 1 || !strings.Contains(got, "first continuation") || !strings.Contains(got, "second continuation") {
		t.Fatalf("merged journal = %q", got)
	}
	got = showTail(body, 1)
	if strings.Contains(got, "first entry") || !strings.Contains(got, "second entry") || !strings.Contains(got, "second continuation") {
		t.Fatalf("last journal record = %q", got)
	}
}

func TestShowSectionMissingListsAvailableAndMatchesCaseInsensitively(t *testing.T) {
	body := "# T\n\n## Приёмка\ncheck\n\n## Журнал\n"
	got, err := showSections(body, []string{"приёмка"})
	if err != nil || !strings.Contains(got, "## Приёмка") {
		t.Fatalf("case-insensitive section = %q, %v", got, err)
	}
	if got, err := showSections(body, []string{"чем проверить"}); err != nil || !strings.Contains(got, "check") {
		t.Fatalf("old name of the section in another case = %q, %v", got, err)
	}
	if _, err := showSections(body, []string{"План"}); err == nil || !strings.Contains(err.Error(), "Приёмка") || !strings.Contains(err.Error(), "Журнал") {
		t.Fatalf("missing section error = %v", err)
	}
	var out, errOut bytes.Buffer
	code := showTask(".", task{path: "20260101-1000-MED-t.md", slug: "t", body: body}, options{sections: []string{"План"}}, &out, &errOut)
	if code != exitNone || out.Len() != 0 || !strings.Contains(errOut.String(), "Приёмка") {
		t.Fatalf("show missing section = %d %q %q", code, out.String(), errOut.String())
	}
}

func TestShowSizeThreshold(t *testing.T) {
	prefix := "# T\n\nСтатус: открыт\n\n## Что сделать\nwork\n\n## Приёмка\ncheck\n\n## Журнал\n"
	records := []string{"- old-marker " + strings.Repeat("x", maxFullShowBytes)}
	for range defaultShowJournalEntries {
		records = append(records, "- record")
	}
	base := prefix + strings.Join(records, "\n")
	target := maxFullShowBytes - 1
	small := prefix + strings.Replace(strings.Join(records, "\n"), strings.Repeat("x", maxFullShowBytes), strings.Repeat("x", maxFullShowBytes-(len(base)-target)), 1)
	if len(small) != target {
		t.Fatalf("small fixture size = %d, want %d", len(small), target)
	}
	large := small + "xx"
	for _, tc := range []struct {
		name    string
		body    string
		wantOld bool
	}{{"under", small, true}, {"over", large, false}} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			showTask(".", task{path: "20260101-1000-MED-t.md", slug: "t", body: tc.body}, options{}, &out, &errOut)
			if strings.Contains(out.String(), "old-marker") != tc.wantOld {
				t.Fatalf("short display marker=%v, want %v", strings.Contains(out.String(), "old-marker"), tc.wantOld)
			}
			if !tc.wantOld && !strings.Contains(out.String(), "--tail 0") {
				t.Fatalf("large display lacks journal hint: %q", out.String())
			}
		})
	}
}

func TestJournalTicketStartsWithoutAcceptance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "20260101-1000-MED-journal.md")
	body := "# Journal\n\nСтатус: открыт\nВид: журнал\n\n## Журнал\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := transition(task{path: path, slug: "journal", status: "открыт", body: body}, options{cmd: "start", by: "tester"}, &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), "в работе") {
		t.Fatalf("journal start = %d %q %q", code, out.String(), errOut.String())
	}
}

func TestTicketBodyParseRoundTripsSectionBoundaries(t *testing.T) {
	body := "# T\n\nСтатус: открыт\n\n## Что сделать\nwork\n\n## Журнал\n- entry\n\ncontinuation\n"
	got := renderTicketDocument(parseTicketBody(body))
	if got != body {
		t.Fatalf("parse/render changed body:\nwant %q\n got %q", body, got)
	}
	if got := journalLines(body); !reflect.DeepEqual(got, []string{"- entry\n\ncontinuation"}) {
		t.Fatalf("journal entries = %q", got)
	}
	if got := showTail(body, 0); got != body {
		t.Fatalf("tail 0 changed full journal: %q", got)
	}
}

func TestNewTextEmptySectionMatchesTemplate(t *testing.T) {
	template := "# T\n\nСтатус: открыт\n\n## Что сделать\n\n## Приёмка\n\n## Журнал\n- opened\n"
	got := fillTicket(template, "## Приёмка\n")
	mainBehavior := fillTicket(template, "")
	if got != mainBehavior {
		t.Fatalf("empty text section changed template:\nwant %q\n got %q", mainBehavior, got)
	}
}

func TestSetTextCannotClearAcceptance(t *testing.T) {
	body := "# T\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n- existing\n"
	if got, err := replaceExistingTicketSections(body, "## Приёмка\n\n"); err == nil || got != "" {
		t.Fatalf("empty acceptance replacement = %q, %v", got, err)
	}
}

func TestShowTicketSectionsAndTail(t *testing.T) {
	body := "# T\n\nСтатус: открыт\nИсполнитель: worker\n\n## Что сделать\nwork\n\n## Приёмка\ncheck\n\n## Журнал\n- old\n- middle\n- new\n"
	got, err := showSections(body, []string{"Приёмка"})
	if err != nil || strings.Contains(got, "Что сделать") || !strings.Contains(got, "## Приёмка\ncheck") {
		t.Fatalf("sections = %q", got)
	}
	got = showTail(body, 2)
	if strings.Contains(got, "old") || !strings.Contains(got, "middle") || !strings.Contains(got, "new") || !strings.Contains(got, "## Что сделать") {
		t.Fatalf("tail = %q", got)
	}
	if strings.Contains(ticketHeader(body), "## ") {
		t.Fatalf("head includes sections: %q", ticketHeader(body))
	}
}

func TestSetReplacesOnlyProvidedSectionsAndKeepsJournal(t *testing.T) {
	body := "# T\n\nСтатус: открыт\n\n## Что сделать\nold work\n\n## Приёмка\nold check\n\n## Журнал\n- existing\n"
	got, err := replaceExistingTicketSections(body, "## Чем проверить\nnew check\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "## Что сделать\nold work") || !strings.Contains(got, "## Приёмка\nnew check") || !strings.Contains(got, "- existing") || strings.Contains(got, "old check") {
		t.Fatalf("set replacement = %q", got)
	}
}

func TestNewAcceptanceIsEmptyAndNoCheckIsShown(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "tasks")
	var out, errout bytes.Buffer
	if code := create(root, dir, options{id: "plain", by: "worker", prio: "MED"}, &out, &errout); code != 0 {
		t.Fatalf("new without text: %d %s", code, errout.String())
	}
	plainPath := filepath.Join(root, strings.TrimSpace(out.String()))
	plain, err := os.ReadFile(plainPath)
	if err != nil || !noCheck(string(plain)) {
		t.Fatalf("new without text should have an empty acceptance section: %s, %v", plain, err)
	}
	issues, err := lintTicket(plainPath, "plain")
	if err != nil || len(issues) != 1 || !strings.Contains(issues[0], "empty Приёмка") {
		t.Fatalf("fresh no-check ticket lint = %v, %v", issues, err)
	}
	out.Reset()
	errout.Reset()
	text := "## Приёмка\n- run checks\n"
	if code := create(root, dir, options{id: "with-text", by: "worker", prio: "MED", text: text}, &out, &errout); code != 0 {
		t.Fatalf("new with text: %d %s", code, errout.String())
	}
	withText, err := os.ReadFile(filepath.Join(root, strings.TrimSpace(out.String())))
	acceptance, ok := sectionBody(string(withText), "## Приёмка")
	if err != nil || !ok || strings.TrimSpace(acceptance) != "- run checks" {
		t.Fatalf("acceptance section = %q, found=%v, err=%v", acceptance, ok, err)
	}
	out.Reset()
	if code := list(root, options{}, &out, &errout); code != 0 || !strings.Contains(out.String(), "plain") || !strings.Contains(out.String(), "no-check") {
		t.Fatalf("ls without-text marker: code=%d output=%q err=%q", code, out.String(), errout.String())
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
	// Временная папка может лежать внутри чужого git-репо: база ищется от HOME, не от его корня.
	t.Setenv("GIT_CEILING_DIRECTORIES", base)
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
	root, dir, main, found, err := resolveTaskBase()
	if err != nil || !found || root != home || dir != filepath.Join(home, "docs", "tasks") || main != "" {
		t.Fatalf("resolved base %q %q %q %v %v", root, dir, main, found, err)
	}
}

// sharedTasksRepo строит основное дерево и linked worktree с веткой, где
// docs/tasks ещё отслеживается (тикет branch-only); в основном дереве после
// этого docs/tasks игнорируется (если ignore) и лежит тикет shared.
func sharedTasksRepo(t *testing.T, ignore bool) (string, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(base, "gitconfig"))
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Setenv("GIT_CEILING_DIRECTORIES", base)
	if err := os.WriteFile(filepath.Join(base, "gitconfig"), []byte("[user]\n\tname = test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	main, wt := filepath.Join(base, "proj"), filepath.Join(base, "wt")
	ticket := "# T\n\nСтатус: открыт\n\n## Приёмка\n- проверить\n\n## Журнал\n"
	writeTestFile(t, filepath.Join(main, "docs/tasks/20260101-1200-LOW-branch-only.md"), ticket)
	gitTest(t, main, "init", "-q")
	gitTest(t, main, "add", "-A")
	gitTest(t, main, "commit", "-qm", "tracked tickets")
	gitTest(t, main, "worktree", "add", "-q", wt, "-b", "feature")
	if ignore {
		writeTestFile(t, filepath.Join(main, ".gitignore"), "/docs/tasks/\n")
		gitTest(t, main, "rm", "-rq", "--cached", "docs/tasks")
		gitTest(t, main, "add", ".gitignore")
		gitTest(t, main, "commit", "-qm", "untrack tickets")
		if err := os.Remove(filepath.Join(main, "docs/tasks/20260101-1200-LOW-branch-only.md")); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(main, "docs/tasks/20260102-1200-LOW-shared.md"), ticket)
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(wt); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	return main, wt
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	b, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, b, err)
	}
	return string(b)
}

func TestWorktreeUsesIgnoredTasksOfMainTree(t *testing.T) {
	main, wt := sharedTasksRepo(t, true)
	root, _, gotMain, found, err := resolveTaskBase()
	if err != nil || !found || root != wt || gotMain != main {
		t.Fatalf("task base %q main %q found=%v err=%v", root, gotMain, found, err)
	}
	var out, errOut bytes.Buffer
	if code := run(options{cmd: "ls", all: true}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "shared") || strings.Contains(out.String(), "branch-only") {
		t.Fatalf("ls from the worktree %d: %s%s", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := run(options{cmd: "new", id: "from-wt", prio: "LOW", by: "t"}, &out, &errOut); code != 0 {
		t.Fatalf("new from the worktree %d: %s", code, errOut.String())
	}
	if got, _ := filepath.Glob(filepath.Join(main, "docs/tasks/*-from-wt.md")); len(got) != 1 {
		t.Fatalf("new did not write into the main tree: %v (%s)", got, out.String())
	}
	if got, _ := filepath.Glob(filepath.Join(wt, "docs/tasks/*-from-wt.md")); len(got) != 0 {
		t.Fatalf("new wrote into the worktree: %v", got)
	}
	if got := gitTest(t, main, "status", "--porcelain") + gitTest(t, wt, "status", "--porcelain"); got != "" {
		t.Fatalf("trees changed in git: %q", got)
	}
}

// symlinkTasks переносит docs/tasks основного дерева в отдельное хранилище и
// оставляет на месте относительный симлинк, игнорируемый шаблоном /docs/tasks.
func symlinkTasks(t *testing.T, main string) string {
	t.Helper()
	store := filepath.Join(filepath.Dir(main), "store", "proj")
	if err := os.MkdirAll(filepath.Dir(store), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(main, "docs/tasks"), store); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../store/proj", filepath.Join(main, "docs/tasks")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(main, ".gitignore"), "/docs/tasks\n")
	gitTest(t, main, "add", ".gitignore")
	gitTest(t, main, "commit", "-qm", "ignore the tasks symlink")
	return store
}

func TestWorktreeUsesSymlinkedTasksOfMainTree(t *testing.T) {
	main, wt := sharedTasksRepo(t, true)
	store := symlinkTasks(t, main)
	var out, errOut bytes.Buffer
	if code := run(options{cmd: "ls", all: true}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "shared") || strings.Contains(out.String(), "branch-only") {
		t.Fatalf("ls from the worktree %d: %s%s", code, out.String(), errOut.String())
	}
	if code := run(options{cmd: "new", id: "from-wt", prio: "LOW", by: "t"}, &out, &errOut); code != 0 {
		t.Fatalf("new from the worktree %d: %s", code, errOut.String())
	}
	if got, _ := filepath.Glob(filepath.Join(store, "*-from-wt.md")); len(got) != 1 {
		t.Fatalf("new did not write into the store: %v (%s)", got, out.String())
	}
	if got := gitTest(t, main, "status", "--porcelain") + gitTest(t, wt, "status", "--porcelain"); got != "" {
		t.Fatalf("trees changed in git: %q", got)
	}
}

func TestArchiveKeepsSymlinkedTasksInTheStore(t *testing.T) {
	base := t.TempDir()
	store := filepath.Join(base, "store", "one")
	body := "# Closed\n\nСтатус: готово\n\n## Приёмка\n- check\n\n## Журнал\n- 2020-01-01 12:00 готово — reviewer: verified\n"
	writeTestFile(t, filepath.Join(store, "20200101-1200-LOW-closed.md"), body)
	if err := os.MkdirAll(filepath.Join(base, "one", "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../store/one", filepath.Join(base, "one", "docs", "tasks")); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := archive("", "", options{root: filepath.Join(base, "one"), older: time.Hour}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "archived 1") {
		t.Fatalf("archive %d: %q %s", code, out.String(), errOut.String())
	}
	if _, err := os.Stat(filepath.Join(store, "archive", "20200101-1200-LOW-closed.md")); err != nil {
		t.Fatalf("closed ticket is not archived inside the store: %v (%s)", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(base, "one", "docs", "tasks-archive")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive went into the repository: %v", err)
	}
}

func TestWorktreeKeepsOwnTasksWhenMainTracksThem(t *testing.T) {
	sharedTasksRepo(t, false)
	var out, errOut bytes.Buffer
	if code := run(options{cmd: "ls", all: true}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "branch-only") || strings.Contains(out.String(), "shared") {
		t.Fatalf("ls from the worktree %d: %s%s", code, out.String(), errOut.String())
	}
}

func TestWorktreeDoneUsesCodeTreeAndDebtNextToTicket(t *testing.T) {
	main, wt := sharedTasksRepo(t, true)
	writeTestFile(t, filepath.Join(wt, "wt-marker"), "x\n")
	writeTestFile(t, filepath.Join(main, "docs/tasks/20260103-1200-LOW-debt.md"), "# Debt\n\nСтатус: открыт\n")
	writeTestFile(t, filepath.Join(wt, ".glint.yaml"), "rules: {}\n")
	bin := t.TempDir()
	glintCwd := filepath.Join(bin, "cwd")
	writeTestFile(t, filepath.Join(bin, "glint"), "#!/bin/sh\npwd > '"+glintCwd+"'\nprintf '%s' '{\"summary\":{\"total\":1},\"issues\":[{\"rule\":\"r\",\"severity\":\"low\",\"file\":\"a.go\",\"line\":1,\"message\":\"m\"}]}'\n")
	if err := os.Chmod(filepath.Join(bin, "glint"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out, errOut bytes.Buffer
	if code := run(options{cmd: "plan", id: "shared", add: "tree: test -f wt-marker", by: "t"}, &out, &errOut); code != 0 {
		t.Fatalf("plan %d: %s", code, errOut.String())
	}
	if code := run(options{cmd: "done", id: "shared", by: "reviewer", doer: "t", check: "c", glintDebt: "debt"}, &out, &errOut); code != 0 {
		t.Fatalf("done from the worktree %d: %s", code, errOut.String())
	}
	if body := read(t, filepath.Join(main, "docs/tasks/20260102-1200-LOW-shared.md")); !strings.Contains(body, "Статус: готово") || !strings.Contains(body, "glint: 1 findings, debt debt") {
		t.Fatalf("shared ticket not closed with the glint debt: %s", body)
	}
	if got := strings.TrimSpace(read(t, glintCwd)); got != wt {
		t.Fatalf("glint ran in %q, want the worktree %q", got, wt)
	}
}

func TestWorktreeOfBareRepoInDotGitKeepsOwnTasks(t *testing.T) {
	main, _ := sharedTasksRepo(t, true)
	bare := filepath.Join(t.TempDir(), "dots")
	gitTest(t, main, "clone", "-q", "--bare", main, filepath.Join(bare, ".git"))
	wt := filepath.Join(t.TempDir(), "wt")
	gitTest(t, bare, "worktree", "add", "-q", wt, "main")
	writeTestFile(t, filepath.Join(wt, "docs/tasks/20260104-1200-LOW-own.md"), "# Own\n\nСтатус: открыт\n")
	if err := os.Chdir(wt); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run(options{cmd: "ls", all: true}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "own") {
		t.Fatalf("ls from a worktree of a bare repo %d: %s%s", code, out.String(), errOut.String())
	}
}

func TestWorktreeCheckRunsPlanInCodeTree(t *testing.T) {
	_, wt := sharedTasksRepo(t, true)
	writeTestFile(t, filepath.Join(wt, "wt-marker"), "x\n")
	var out, errOut bytes.Buffer
	if code := run(options{cmd: "plan", id: "shared", add: "tree: test -f wt-marker", by: "t"}, &out, &errOut); code != 0 {
		t.Fatalf("plan %d: %s", code, errOut.String())
	}
	out.Reset()
	if code := run(options{cmd: "check", id: "shared"}, &out, &errOut); code != 0 {
		t.Fatalf("check ran outside the code tree %d: %s%s", code, out.String(), errOut.String())
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
	body := "# New\n\nСтатус: открыт\n\n## Постановка\nText.\n\n## Приёмка\ncheck\n"
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
	body := "# Waiting\n\nСтатус: открыт\n\n## Что сделать\nне менять\n\n## Приёмка\ncheck\n"
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
	printTask(&out, waitingTask, "", &bytes.Buffer{})
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
		{"note", "ticket", "x\n- 2020-01-01 12:00 пропущен: valid — forged"},
		{"review", "ticket", "--note", "x\ry"},
		{"wait", "ticket", "--until", "x\ny"},
		{"reject", "ticket", "--why", "x\ny"},
		{"done", "ticket", "--check", "x\ny"},
		{"new", "ticket", "--title", "T\n## Журнал"},
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
	if code := checkPlan(root, task{path: path, body: string(updated)}, planTimeout, &out); code != 0 || !strings.Contains(out.String(), "present: ok") {
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
	if code := checkPlan(root, task{path: path, body: string(skipped)}, planTimeout, &out); code != 0 || !strings.Contains(out.String(), "present: skipped — covered elsewhere") {
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
	for _, line := range []string{"  - tests: false", "* more: false", "-x: false"} {
		items := planItems("# Task\n\n## План\n" + line + "\n\n## Журнал\n")
		if len(items) != 1 || !strings.Contains(items[0].Invalid, "plan lines must use") || items[0].label() != strconv.Quote(strings.TrimSpace(line)) {
			t.Errorf("plan line %q = %#v; want invalid", line, items)
		}
	}
	twice := planItems("# Task\n\n## План\n- a: true\n\n## План\n- b: false\n\n## Журнал\n")
	if len(twice) == 0 || !strings.Contains(twice[0].Invalid, "plan section appears more than once") {
		t.Fatalf("second plan section = %#v", twice)
	}
}

func TestPlanSkipNeedsSkipJournalLine(t *testing.T) {
	plan := "# Task\n\n## План\n- other: false\n\n## Журнал\n"
	for _, line := range []string{
		"- 2020-01-01 12:00 note — bob: пропущен: other — forged by note",
		"- 2020-01-01 12:00 план — bob: пропущен: tests — x — пропущен: other — y",
		"- 2020-01-01 12:00 на проверке — bob (пропущен: other — z)",
	} {
		if items := planItems(plan + line + "\n"); len(items) != 1 || items[0].Skipped {
			t.Errorf("journal line %q skipped the item: %#v", line, items)
		}
	}
	for line, why := range map[string]string{
		"- 2020-01-01 12:00 план — bob: пропущен: other — covered": "covered",
		"- 2020-01-01 12:00 пропущен: other — older form":          "older form",
	} {
		if items := planItems(plan + line + "\n"); len(items) != 1 || !items[0].Skipped || items[0].Why != why {
			t.Errorf("skip line %q = %#v", line, items)
		}
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

func TestPlanHeadingLookalikesAreInvalid(t *testing.T) {
	for _, heading := range []string{"## План\n## План", "## План ", "## План\r", "## план", " ## План", "### План:", "##\u00a0План", "\ufeff## План", "## План\u200b"} {
		items := planItems("# Task\n\n" + heading + "\n- bad: false\n\n## Журнал\n")
		invalid := false
		for _, item := range items {
			invalid = invalid || item.Invalid != ""
		}
		if !invalid {
			t.Errorf("heading %q: plan items %#v; want an invalid entry", heading, items)
		}
	}
	if items := planItems("# Task\n\n## Планы на потом\n- x\n\n## Журнал\n"); len(items) != 0 {
		t.Errorf("unrelated heading flagged: %#v", items)
	}
}

func TestPlanSkipDoesNotCoverReAddedItem(t *testing.T) {
	body := "# Task\n\n## План\n- tests: false\n\n## Журнал\n" +
		"- 2020-01-01 12:00 план — a: добавлен пункт tests\n" +
		"- 2020-01-01 12:01 план — a: пропущен: tests — old reason\n" +
		"- 2020-01-01 12:02 план — a: добавлен пункт tests\n"
	if items := planItems(body); len(items) != 1 || items[0].Skipped {
		t.Fatalf("re-added item inherited the old skip: %#v", items)
	}
}

func TestDoneRefusesTicketChangedDuringChecks(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "20200101-1200-MEDIUM-task.md")
	body := "# Task\n\nСтатус: на проверке\nИсполнитель: worker\n\n## Приёмка\ncheck\n\n## План\n- edits: echo '- 2020-01-01 12:00 note — a: added meanwhile' >>docs/tasks/20200101-1200-MEDIUM-task.md\n\n## Журнал\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := transition(task{path: path, slug: "task", body: body}, options{cmd: "done", by: "reviewer", check: "verified", baseRoot: root}, &out, &errOut)
	updated, err := os.ReadFile(path)
	if err != nil || code != exitNone || !strings.Contains(errOut.String(), "changed while plan checks ran") || !strings.Contains(string(updated), "added meanwhile") || strings.Contains(string(updated), "Статус: готово") {
		t.Fatalf("done over a changed ticket: code=%d stderr=%q body=%q", code, errOut.String(), updated)
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
	result := runPlanItem(t.TempDir(), planJSON{Name: "slow", Cmd: "sleep 5 & wait"}, 30*time.Millisecond)
	if !result.timedOut {
		t.Fatalf("expected timeout, got %#v", result)
	}
	if msg := planFailure(result); !strings.Contains(msg, "timeout after 30ms") || !strings.Contains(msg, "--timeout") {
		t.Fatalf("timeout message %q does not name the limit and the flag", msg)
	}
}

func TestPlanTimeoutFlag(t *testing.T) {
	for _, args := range [][]string{{"check", "20260101-0000-LOW-x"}, {"done", "20260101-0000-LOW-x", "--check", "ok"}} {
		o, err := parseArgs(args)
		if err != nil || o.planLimit() != 8*time.Minute {
			t.Fatalf("%v: default timeout %v, err %v", args, o.timeout, err)
		}
		o, err = parseArgs(append(append([]string{}, args...), "--timeout", "15m"))
		if err != nil || o.planLimit() != 15*time.Minute {
			t.Fatalf("%v --timeout 15m: %v, err %v", args, o.timeout, err)
		}
	}
	for _, args := range [][]string{{"note", "20260101-0000-LOW-x", "text", "--timeout", "1m"}, {"check", "20260101-0000-LOW-x", "--timeout", "0s"}, {"check", "20260101-0000-LOW-x", "--timeout"}} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	root := t.TempDir()
	path := filepath.Join(root, "20260101-0000-LOW-x.md")
	body := "# x\n\n## План\n- slow: sleep 2\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := checkPlan(root, task{path: path, body: body}, 50*time.Millisecond, &out); code == 0 || !strings.Contains(out.String(), "slow: timeout after 50ms") {
		t.Fatalf("check with short limit: code %d, %q", code, out.String())
	}
}

func TestPlanItemStopsLeftoverChild(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	result := runPlanItem(dir, planJSON{Name: "bg", Cmd: "sleep 3 & echo $! >pid; echo first >&2"}, 5*time.Second)
	if elapsed := time.Since(start); elapsed > 2*time.Second || result.exit != 0 || result.timedOut || result.stderr != "first" {
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
	for attempt := range 2 {
		stat, err := proc.ReadStat("/proc", pid)
		if err != nil || stat.State == "Z" || stat.State == "X" {
			return
		}
		if attempt == 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	t.Fatalf("leftover child %d still alive immediately after runPlanItem", pid)
}

func TestCancelStopDegradedWarnsOnStderr(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{"plan item", func(t *testing.T) {
			if result := runPlanItem(t.TempDir(), planJSON{Name: "degraded", Cmd: "exit 0"}, time.Second); result.exit != 0 {
				t.Fatalf("result=%+v", result)
			}
		}},
		{"bounded command", func(t *testing.T) {
			if _, _, err := runBoundedCommand("/bin/true", nil, nil, time.Second); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldCancel, oldStderr := cancelByStopWithObserver, os.Stderr
			stderr, err := os.CreateTemp(t.TempDir(), "stderr-*")
			if err != nil {
				t.Fatal(err)
			}
			os.Stderr = stderr
			cancelByStopWithObserver = func(cmd *exec.Cmd, _ time.Duration, _ syscall.Signal, observer func(bool, error)) error {
				observer(true, nil)
				return nil
			}
			t.Cleanup(func() {
				cancelByStopWithObserver, os.Stderr = oldCancel, oldStderr
				_ = stderr.Close()
			})
			tc.run(t)
			os.Stderr = oldStderr
			if _, err := stderr.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(stderr)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(got), "остановлен только прямой потомок: pidfd недоступен") {
				t.Fatalf("stderr=%q", got)
			}
		})
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
	if found, filled := checkSections("## Что сделать\nwork\n"); found || filled {
		t.Error("missing acceptance must report not found and empty")
	}
	if found, filled := checkSections("## Приёмка\n\n## Чем проверить\n- check\n"); !found || !filled {
		t.Error("filled legacy acceptance must be reported")
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

func TestPrepareActorRequiresSessionOrExplicitActor(t *testing.T) {
	previous := session
	t.Cleanup(func() { session = previous })
	t.Setenv("USER", "runner")
	session = func() string { return "" }
	o := options{cmd: "new"}
	var errout bytes.Buffer
	if code := prepareActor(&o, &errout); code != exitUsage || !strings.Contains(errout.String(), "--by") {
		t.Fatalf("actor=%q code=%d stderr=%q", o.by, code, errout.String())
	}
	session = func() string { return "agent-session" }
	o = options{cmd: "next"}
	if code := prepareActor(&o, &bytes.Buffer{}); code != 0 || o.by != "agent-session" {
		t.Fatalf("actor=%q code=%d, want agent-session", o.by, code)
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

func TestReleaseReturnsInProgressTicketToOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "20260101-1000-MED-released.md")
	body := "# Released\n\nСтатус: в работе\nИсполнитель: departed-worker\n\n## Приёмка\n- check\n\n## Журнал\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := transition(task{path: path, slug: "released", status: "в работе", body: body}, options{cmd: "release", by: "available-worker", why: "owner session ended"}, &out, &errOut)
	updated, err := os.ReadFile(path)
	if code != 0 || err != nil || strings.Contains(string(updated), "Исполнитель:") || !strings.Contains(string(updated), "Статус: открыт") || !strings.Contains(string(updated), "открыт — available-worker — owner session ended") {
		t.Fatalf("release result=%d body=%q err=%v stderr=%q", code, updated, err, errOut.String())
	}
	if _, err := parseArgs([]string{"release", "released"}); err == nil {
		t.Fatal("release without --why succeeded")
	}
}

func TestReleaseRejectsOtherStatusesWithoutWriting(t *testing.T) {
	for _, status := range []string{"открыт", "ждёт", "на проверке", "готово"} {
		t.Run(status, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "20260101-1000-MED-task.md")
			body := "# Task\n\nСтатус: " + status + "\nИсполнитель: worker\n\n## Журнал\n"
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			code := transition(task{path: path, slug: "task", status: status, body: body}, options{cmd: "release", why: "reason", by: "worker"}, &out, &errOut)
			updated, err := os.ReadFile(path)
			if code == 0 || err != nil || string(updated) != body {
				t.Fatalf("release status %q result=%d body=%q err=%v", status, code, updated, err)
			}
		})
	}
}

func TestSetTitleAndRequesterJournalChangesWithoutRenaming(t *testing.T) {
	path := filepath.Join(t.TempDir(), "20260101-1000-MED-task.md")
	body := "# Old title\n\nСтатус: открыт\nЗаказчик: old-requester\nЗаказчик: duplicate\n\n## Журнал\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := setTask(task{path: path, slug: "task", body: body, title: "Old title"}, options{cmd: "set", title: "New title", back: "new-requester", by: "editor"}, &out, &errOut)
	updated, err := os.ReadFile(path)
	if code != 0 || err != nil || !strings.HasPrefix(string(updated), "# New title\n") || strings.Count(string(updated), "Заказчик: ") != 1 || !strings.Contains(string(updated), "Заказчик: new-requester") || !strings.Contains(string(updated), "заголовок — editor: Old title → New title") || !strings.Contains(string(updated), "заказчик — editor: old-requester → new-requester") {
		t.Fatalf("set result=%d body=%q err=%v stderr=%q", code, updated, err, errOut.String())
	}
	if out.String() != filepath.Base(path)+"\n" {
		t.Fatalf("set renamed ticket: %q", out.String())
	}
	if _, err := parseArgs([]string{"set", "task"}); err == nil {
		t.Fatal("set without fields succeeded")
	}
	if _, err := parseArgs([]string{"set", "task", "--title", "Updated"}); err != nil {
		t.Fatalf("set --title: %v", err)
	}
	if _, err := parseArgs([]string{"set", "task", "--back", "requester"}); err != nil {
		t.Fatalf("set --back: %v", err)
	}
}

func TestListJSONIncludesWhyAndPath(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "20260101-1000-MED-json-fields.md")
	body := "# JSON fields\n\nСтатус: открыт\nПричина: synthetic reason\n\n## Приёмка\n- check\n\n## Журнал\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := list(root, options{json: true}, &out, &errOut); code != 0 {
		t.Fatalf("list JSON code %d: %s", code, errOut.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0]["why"] != "synthetic reason" || rows[0]["path"] == "" {
		t.Fatalf("ls JSON fields = %#v, err=%v", rows, err)
	}
}

// Тесты не видят сессию машины, на которой идут; нужную тесту задаёт подмена session.
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

func TestWaitDueForms(t *testing.T) {
	now := time.Date(2026, 10, 31, 15, 0, 0, 0, time.Local)
	at := func(m, d, h int) time.Time { return time.Date(2026, time.Month(m), d, h, 0, 0, 0, time.Local) }
	for _, c := range []struct {
		wait string
		want time.Time
		ok   bool
	}{
		{"2026-10-31 12:00", at(10, 31, 12), true},
		{"2026-10-31T12:00", at(10, 31, 12), true},
		{"2026-10-31 12:00 итог замера", at(10, 31, 12), true},
		{"2026-11-02 09:00 CET (выходные)", at(11, 2, 9), true},
		{"2026-11-01 09:00 - ночь 01.11: шаг прошёл", at(11, 1, 9), true},
		{"31.10 12:00 после замера", at(10, 31, 12), true},
		{"2026-11-01 (днём)", at(11, 1, 0), true},
		{"2026-10-31T11:00:00Z", time.Date(2026, 10, 31, 11, 0, 0, 0, time.UTC), true},
		{"2026-10-31 11:00 UTC", time.Date(2026, 10, 31, 11, 0, 0, 0, time.UTC), true},
		{"2026-10-31T11:00Z", time.Date(2026, 10, 31, 11, 0, 0, 0, time.UTC), true},
		{"2026-11-01 08:00: план проверок", at(11, 1, 8), true},
		{"2026-11-06: итог хранилища", at(11, 6, 0), true},
		{"2026-11-06, после замера", at(11, 6, 0), true},
		{"после ответа", time.Time{}, false},
		{"ночь 30.10: файл есть", time.Time{}, false},
		{"2h после деплоя", time.Time{}, false},
		{"", time.Time{}, false},
	} {
		got, ok := timearg.Deadline(c.wait, now)
		if ok != c.ok || !got.Equal(c.want) {
			t.Errorf("timearg.Deadline(%q) = %v, %v; ждали %v, %v", c.wait, got, ok, c.want, c.ok)
		}
	}
	if got := datedWait("12:00 итог", now); got != "2026-10-31 12:00 итог" {
		t.Errorf("datedWait = %q", got)
	}
	if got := datedWait("после ответа", now); got != "после ответа" {
		t.Errorf("datedWait условия = %q", got)
	}
}

func TestDueWithTextAndISO(t *testing.T) {
	for _, wait := range []string{"2020-01-01T12:00", "2020-01-01 12:00 итог замера", "2020-01-01 12:00 CET (x)"} {
		waiting := task{status: "ждёт", body: "Статус: ждёт\nЖдёт: " + wait}
		if !matchesListFilters(waiting, options{cmd: "ls", due: true}) {
			t.Errorf("--due пропустил просроченное %q", wait)
		}
		if !taskObject("", waiting, "", false).Overdue {
			t.Errorf("overdue false у %q", wait)
		}
	}
	for _, wait := range []string{"2999-01-01 12:00 итог", "после ответа"} {
		if matchesListFilters(task{status: "ждёт", body: "Статус: ждёт\nЖдёт: " + wait}, options{cmd: "ls", due: true}) {
			t.Errorf("--due показал непросроченное %q", wait)
		}
	}
}

func TestWaitDatesBareClockAndWarnsOnCondition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "20200101-1200-MEDIUM-clock.md")
	body := "# Clock\n\nСтатус: открыт\n\n## Что сделать\nx\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := transition(task{path: path, slug: "clock", body: body}, options{cmd: "wait", by: "worker", until: "23:59 итог"}, &out, &errOut); code != 0 {
		t.Fatalf("wait %d: %s", code, errOut.String())
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "Ждёт: "+time.Now().Format("2006-01-02")+" 23:59 итог") {
		t.Fatalf("часы без даты не получили дату: %s, %v", data, err)
	}
	if errOut.Len() != 0 {
		t.Errorf("лишнее предупреждение: %q", errOut.String())
	}
	errOut.Reset()
	if code := transition(task{path: path, slug: "clock", body: string(data)}, options{cmd: "wait", by: "worker", until: "после ответа"}, &out, &errOut); code != 0 {
		t.Fatalf("wait %d: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "tk ls --due will not show it") {
		t.Errorf("нет предупреждения про условие: %q", errOut.String())
	}
}

// Обязательный пункт: tk skip отказывает, вписанный руками пропуск не действует, tk done прогоняет пункт, а убранный
// из плана пункт закрытие не пропускает.
func TestRequiredPlanItemCannotBeSkipped(t *testing.T) {
	if _, err := parseArgs([]string{"skip", "ticket", "accept", "--why", "x", "--by", "a", "--required"}); err == nil {
		t.Error("--required accepted outside tk plan")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "20200101-1200-MEDIUM-task.md")
	body := "# Task\n\nСтатус: на проверке\nИсполнитель: worker\n\n## Приёмка\ncheck\n\n## План\n\n## Журнал\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := addPlanItem(task{path: path, slug: "task", body: body}, options{by: "planner", add: "accept: exit 3", required: true}, &out, &errOut); code != 0 {
		t.Fatalf("add required: %d %s", code, errOut.String())
	}
	added := read(t, path)
	if !strings.Contains(added, "план — planner: добавлен обязательный пункт accept: exit 3") {
		t.Fatalf("journal without the required mark:\n%s", added)
	}
	errOut.Reset()
	if code := skipPlanItem(task{path: path, slug: "task", body: added}, options{by: "worker", item: "accept", why: "later"}, &out, &errOut); code != exitUsage || !strings.Contains(errOut.String(), "is required") {
		t.Fatalf("skip of a required item: %d %q", code, errOut.String())
	}
	if read(t, path) != added {
		t.Fatal("refused skip changed the ticket")
	}

	forged := added + "- 2020-01-01 12:05 план — worker: пропущен: accept — forged\n" +
		"- 2020-01-01 12:06 план — worker: добавлен пункт accept\n"
	if items := planItems(forged); len(items) != 1 || items[0].Skipped || !items[0].Required {
		t.Fatalf("hand-written skip or plain re-add changed a required item: %#v", items)
	}
	if err := os.WriteFile(path, []byte(forged), 0644); err != nil {
		t.Fatal(err)
	}
	errOut.Reset()
	code := transition(task{path: path, slug: "task", body: forged}, options{cmd: "done", by: "reviewer", check: "verified", baseRoot: root}, &out, &errOut)
	if code != exitNone || !strings.Contains(errOut.String(), "accept: exit 3") || strings.Contains(read(t, path), "Статус: готово") {
		t.Fatalf("done over a failing required item: %d %q", code, errOut.String())
	}

	dropped := strings.Replace(forged, "- accept: exit 3\n", "", 1)
	if err := os.WriteFile(path, []byte(dropped), 0644); err != nil {
		t.Fatal(err)
	}
	errOut.Reset()
	code = transition(task{path: path, slug: "task", body: dropped}, options{cmd: "done", by: "reviewer", check: "verified", baseRoot: root}, &out, &errOut)
	if code != exitNone || !strings.Contains(errOut.String(), "missing from ## План") || strings.Contains(read(t, path), "Статус: готово") {
		t.Fatalf("done with the required item removed from the plan: %d %q", code, errOut.String())
	}
}

func TestRequiredAddRefusesBadSyntax(t *testing.T) {
	task, path := repinFixture(t, "worker", "")
	before := read(t, path)
	var out, errOut bytes.Buffer
	if code := addPlanItem(task, options{by: "planner", add: "bad: cd <дерево> && true", required: true}, &out, &errOut); code != exitUsage ||
		!strings.Contains(errOut.String(), "bash syntax") || !strings.Contains(errOut.String(), "дерево") || read(t, path) != before {
		t.Fatalf("bad required syntax: code %d, stderr %q", code, errOut.String())
	}
	if code := addPlanItem(task, options{by: "planner", add: "bad: cd <дерево> && true"}, &out, &errOut); code != 0 || !strings.Contains(read(t, path), "- bad: cd <дерево> && true") {
		t.Fatalf("bad optional syntax should be added: code %d, stderr %q", code, errOut.String())
	}
	task.body = read(t, path)
	for _, required := range []bool{false, true} {
		name := "ok"
		if required {
			name = "ok-required"
		}
		if code := addPlanItem(task, options{by: "planner", add: name + ": true", required: required}, &out, &errOut); code != 0 {
			t.Fatalf("valid add (required %v): %d %s", required, code, errOut.String())
		}
		task.body = read(t, path)
	}
}

func TestRepinFromOtherSession(t *testing.T) {
	task, path := repinFixture(t, "a", "- 2020-01-01 12:00 план — a: добавлен обязательный пункт accept: false\n")
	addMarker := filepath.Join(t.TempDir(), "added")
	addCommand := "touch " + strconv.Quote(addMarker)
	var out, errOut bytes.Buffer
	if code := addPlanItem(task, options{by: "planner", add: "added: " + addCommand, required: true}, &out, &errOut); code != 0 {
		t.Fatalf("add required item: %d %s", code, errOut.String())
	}
	if _, err := os.Stat(addMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("required add command ran during syntax check: %v", err)
	}
	task = taskFromPath(path, "task", read(t, path))
	marker := filepath.Join(t.TempDir(), "repinned")
	command := "touch " + strconv.Quote(marker)
	oldSession := session
	session = func() string { return "b" }
	t.Cleanup(func() { session = oldSession })
	if code := repinPlanItem(task, options{by: "a", repin: "accept: " + command, why: "fixed"}, &out, &errOut); code != 0 {
		t.Fatalf("repin: %d %s", code, errOut.String())
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repinned command ran during syntax check: %v", err)
	}
	updated := read(t, path)
	if !strings.Contains(updated, "- accept: "+command) || !strings.Contains(updated, "план — b: перезакреплён обязательный пункт accept: false → "+command+" — fixed") {
		t.Fatalf("repin did not update plan and journal:\n%s", updated)
	}
	if code := transition(taskFromPath(path, "task", updated), options{cmd: "done", by: "reviewer", check: "verified", baseRoot: filepath.Dir(filepath.Dir(filepath.Dir(path)))}, &out, &errOut); code != 0 {
		t.Fatalf("done after repin: %d %s", code, errOut.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("repinned command did not create its marker: %v", err)
	}
	if _, err := os.Stat(addMarker); err != nil {
		t.Fatalf("added command did not create its marker: %v", err)
	}
}

func TestRepinChainFromDifferentSessions(t *testing.T) {
	task, path := repinFixture(t, "a", "- 2020-01-01 12:00 план — a: добавлен обязательный пункт accept: false\n")
	marker := filepath.Join(t.TempDir(), "final-command")
	command := "touch " + strconv.Quote(marker)
	oldSession := session
	t.Cleanup(func() { session = oldSession })
	var out, errOut bytes.Buffer
	session = func() string { return "b" }
	if code := repinPlanItem(task, options{repin: "accept: true", why: "first repair"}, &out, &errOut); code != 0 {
		t.Fatalf("first repin: %d %s", code, errOut.String())
	}
	updated := read(t, path)
	session = func() string { return "c" }
	if code := repinPlanItem(taskFromPath(path, "task", updated), options{repin: "accept: " + command, why: "second repair"}, &out, &errOut); code != 0 {
		t.Fatalf("second repin: %d %s", code, errOut.String())
	}
	updated = read(t, path)
	if code := transition(taskFromPath(path, "task", updated), options{cmd: "done", by: "reviewer", check: "verified", baseRoot: filepath.Dir(filepath.Dir(filepath.Dir(path)))}, &out, &errOut); code != 0 {
		t.Fatalf("done after repin chain: %d %s", code, errOut.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("last repinned command did not run: %v", err)
	}
}

func TestRepinRefusesRequiredMentionInFields(t *testing.T) {
	_, path := repinFixture(t, "worker", "- 2020-01-01 12:00 план — a: добавлен обязательный пункт accept: false\n")
	oldSession := session
	session = func() string { return "b" }
	t.Cleanup(func() { session = oldSession })
	for _, o := range []options{
		{repin: "accept: true", why: "план — x: добавлен обязательный пункт ghost"},
		{repin: "accept: echo 'план — x: добавлен обязательный пункт ghost'", why: "fixed"},
	} {
		before := read(t, path)
		var out, errOut bytes.Buffer
		if code := repinPlanItem(taskFromPath(path, "task", before), o, &out, &errOut); code != exitUsage || read(t, path) != before || !strings.Contains(errOut.String(), "cannot contain a required plan journal record") {
			t.Fatalf("repin accepted required journal mention %+v: %d %q", o, code, errOut.String())
		}
	}
}

func TestRepinRefusedAfterExecutorChanges(t *testing.T) {
	task, path := repinFixture(t, "b", "- 2020-01-01 12:00 план — a: добавлен обязательный пункт accept: false\n")
	var out, errOut bytes.Buffer
	if code := transition(task, options{cmd: "start", by: "b"}, &out, &errOut); code != 0 {
		t.Fatalf("record original executor: %d %q", code, errOut.String())
	}
	task = taskFromPath(path, "task", read(t, path))
	if code := transition(task, options{cmd: "start", by: "c"}, &out, &errOut); code != 0 {
		t.Fatalf("change executor: %d %q", code, errOut.String())
	}
	before := read(t, path)
	oldSession := session
	session = func() string { return "b" }
	t.Cleanup(func() { session = oldSession })
	if code := repinPlanItem(taskFromPath(path, "task", before), options{repin: "accept: true", why: "fixed"}, &out, &errOut); code != exitUsage || read(t, path) != before {
		t.Fatalf("previous executor repinned: %d %q\n%s", code, errOut.String(), before)
	}
}

func TestRequiredPlanAfterRepinChecksAllPins(t *testing.T) {
	before := "- 2020-01-01 12:00 план — a: добавлен обязательный пункт accept: false\n" +
		"- 2020-01-01 12:00 план — a: добавлен обязательный пункт other: false\n"
	valid := before + "- 2020-01-01 12:01 план — b: перезакреплён обязательный пункт accept: false → true — fixed\n"
	if !requiredPlanAfterRepin(before, valid, "accept", "true") {
		t.Fatal("valid repin changed the required plan unexpectedly")
	}
	changedOther := valid + "- 2020-01-01 12:02 план — c: перезакреплён обязательный пункт other: unexpected → true — changed\n"
	if requiredPlanAfterRepin(before, changedOther, "accept", "true") {
		t.Fatal("repin accepted a conflict in another required item")
	}
	missingOther := strings.Replace(valid, "добавлен обязательный пункт other: false", "", 1)
	if missingOther == valid {
		t.Fatal("test fixture did not remove the other required pin")
	}
	if requiredPlanAfterRepin(before, missingOther, "accept", "true") {
		t.Fatal("repin accepted the disappearance of another required item")
	}
}

func TestRepinRefusedForExecutorAndAuthor(t *testing.T) {
	cases := []struct {
		name, executor, journal, current, by string
	}{
		{"executor", "b", "- 2020-01-01 12:00 план — a: добавлен обязательный пункт accept: false\n", "b", "elsewhere"},
		{"pin author", "worker", "- 2020-01-01 12:00 план — a: добавлен обязательный пункт accept: false\n", "a", "elsewhere"},
		{"previous repin author", "worker", "- 2020-01-01 12:00 план — a: добавлен обязательный пункт accept: echo ok\n- 2020-01-01 12:01 план — b: перезакреплён обязательный пункт accept: echo ok → echo ok | true — reason\n", "b", "elsewhere"},
		{"by ignored", "b", "- 2020-01-01 12:00 план — a: добавлен обязательный пункт accept: false\n", "b", "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task, path := repinFixture(t, tc.executor, tc.journal)
			if tc.name == "previous repin author" {
				task.body = strings.Replace(task.body, "- accept: false", "- accept: echo ok | true", 1)
				if err := os.WriteFile(path, []byte(task.body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			before := read(t, path)
			oldSession := session
			session = func() string { return tc.current }
			t.Cleanup(func() { session = oldSession })
			var out, errOut bytes.Buffer
			if code := repinPlanItem(task, options{by: tc.by, repin: "accept: true", why: "reason"}, &out, &errOut); code != exitUsage || read(t, path) != before {
				t.Fatalf("repin was not refused: %d %q", code, errOut.String())
			}
		})
	}
}

func TestRepinRefusedWithoutSession(t *testing.T) {
	task, path := repinFixture(t, "worker", "- 2020-01-01 12:00 план — a: добавлен обязательный пункт accept: false\n")
	before := read(t, path)
	oldSession := session
	session = func() string { return "" }
	t.Cleanup(func() { session = oldSession })
	var out, errOut bytes.Buffer
	if code := repinPlanItem(task, options{by: "b", repin: "accept: true", why: "reason"}, &out, &errOut); code != exitUsage || read(t, path) != before {
		t.Fatalf("repin without session: %d %q", code, errOut.String())
	}
}

func TestRepinNoRenameNoSkipNoBadSyntax(t *testing.T) {
	task, path := repinFixture(t, "worker", "- 2020-01-01 12:00 план — a: добавлен обязательный пункт accept: false\n")
	oldSession := session
	session = func() string { return "b" }
	t.Cleanup(func() { session = oldSession })
	for _, o := range []options{
		{repin: "renamed: true", why: "reason"},
		{repin: "accept: true"},
		{repin: "accept: cd <дерево> && true", why: "reason"},
		{repin: "accept: true", why: "was a → b"},
		{repin: "accept: false", why: "reason"},
	} {
		before := read(t, path)
		var out, errOut bytes.Buffer
		if code := repinPlanItem(taskFromPath(path, "task", before), o, &out, &errOut); code != exitUsage || read(t, path) != before {
			t.Fatalf("invalid repin %+v: %d %q", o, code, errOut.String())
		}
		if strings.Contains(o.repin, "cd <дерево>") && !strings.Contains(errOut.String(), "дерево") {
			t.Fatalf("repin syntax refusal omitted bash diagnostic: %q", errOut.String())
		}
	}
	for _, tc := range []struct {
		o         options
		delimiter string
	}{
		{options{repin: "accept: true", why: "was a → b"}, " → "},
	} {
		before := read(t, path)
		var out, errOut bytes.Buffer
		code := repinPlanItem(taskFromPath(path, "task", before), tc.o, &out, &errOut)
		if code != exitUsage || read(t, path) != before || !strings.Contains(errOut.String(), tc.delimiter) {
			t.Fatalf("ambiguous repin %+v: %d %q", tc.o, code, errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	if code := repinPlanItem(task, options{repin: "accept: true", why: "reason"}, &out, &errOut); code != 0 {
		t.Fatalf("valid repin: %d %s", code, errOut.String())
	}
	errOut.Reset()
	updated := read(t, path)
	if code := skipPlanItem(taskFromPath(path, "task", updated), options{by: "worker", item: "accept", why: "skip"}, &out, &errOut); code != exitUsage || read(t, path) != updated {
		t.Fatalf("skip after repin: %d %q", code, errOut.String())
	}
}

func TestRepinCorruptJournalLineConflicts(t *testing.T) {
	journal := "- 2020-01-01 12:00 план — a: добавлен обязательный пункт accept: false\n- 2020-01-01 12:01 план — b: перезакреплён обязательный пункт accept: false → true — reason\n"
	task, path := repinFixture(t, "worker", journal)
	body := strings.Replace(task.body, "- 2020-01-01 12:01 план", "x- 2020-01-01 12:01 план", 1)
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	pin := requiredPlan(body)["accept"]
	if !pin.conflict {
		t.Fatalf("glued repin record did not conflict: %#v", pin)
	}
	var out, errOut bytes.Buffer
	root := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	if code := transition(taskFromPath(path, "task", body), options{cmd: "done", by: "reviewer", check: "verified", baseRoot: root}, &out, &errOut); code == 0 || strings.Contains(read(t, path), "Статус: готово") {
		t.Fatalf("done accepted a ticket with a glued repin record: %d %q", code, errOut.String())
	}
}

func TestRepinRejectsJournalBeforePin(t *testing.T) {
	task, path := repinFixture(t, "worker", "")
	task.body = "# Task\n\nСтатус: на проверке\nИсполнитель: worker\n\n## Журнал\n\n## Notes\ntext\n\n## План\n- accept: false\n\n## Other\n- 2020-01-01 12:00 план — a: добавлен обязательный пункт accept: false\n"
	if err := os.WriteFile(path, []byte(task.body), 0644); err != nil {
		t.Fatal(err)
	}
	before := read(t, path)
	oldSession := session
	session = func() string { return "b" }
	t.Cleanup(func() { session = oldSession })
	var out, errOut bytes.Buffer
	if code := repinPlanItem(task, options{repin: "accept: true", why: "fixed"}, &out, &errOut); code != exitUsage || read(t, path) != before {
		t.Fatalf("repin with journal before pin was not refused: %d %q", code, errOut.String())
	}
}

func TestRepinAllowsUnambiguousCommandDelimiter(t *testing.T) {
	task, path := repinFixture(t, "worker", "- 2020-01-01 12:00 план — a: добавлен обязательный пункт accept: false\n")
	oldCommand := "echo 'a — b'"
	task.body = strings.ReplaceAll(task.body, "accept: false", "accept: "+oldCommand)
	if err := os.WriteFile(path, []byte(task.body), 0644); err != nil {
		t.Fatal(err)
	}
	oldSession := session
	session = func() string { return "b" }
	t.Cleanup(func() { session = oldSession })
	var out, errOut bytes.Buffer
	if code := repinPlanItem(task, options{repin: "accept: true", why: "fixed"}, &out, &errOut); code != 0 {
		t.Fatalf("repin with unambiguous delimiter in old command: %d %q", code, errOut.String())
	}
	pin := requiredPlan(read(t, path))["accept"]
	if pin.conflict || pin.cmd != "true" {
		t.Fatalf("repinned command not recovered: %#v", pin)
	}
}

func TestRepinChainHandEdits(t *testing.T) {
	head := "# Task\n\nСтатус: на проверке\nИсполнитель: worker\n\n## План\n- accept: c\n\n## Журнал\n"
	for _, tc := range []struct {
		name, journal string
		conflict      bool
	}{
		{"bad old command", "- 2020-01-01 12:00 план — a: добавлен обязательный пункт accept: a\n- 2020-01-01 12:01 план — b: перезакреплён обязательный пункт accept: wrong → c — reason\n", true},
		{"repin without pin", "- 2020-01-01 12:01 план — b: перезакреплён обязательный пункт accept: wrong → c — reason\n", true},
		{"two valid repins", "- 2020-01-01 12:00 план — a: добавлен обязательный пункт accept: a\n- 2020-01-01 12:01 план — b: перезакреплён обязательный пункт accept: a → b — reason\n- 2020-01-01 12:02 план — c: перезакреплён обязательный пункт accept: b → c — reason\n", false},
	} {
		pin := requiredPlan(head + tc.journal)["accept"]
		if pin.conflict != tc.conflict || (!tc.conflict && pin.cmd != "c") {
			t.Errorf("%s: required pin = %#v", tc.name, pin)
		}
	}
}

func repinFixture(t *testing.T, executor, journal string) (task, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "20200101-1200-MEDIUM-task.md")
	body := "# Task\n\nСтатус: на проверке\nИсполнитель: " + executor + "\n\n## Приёмка\ncheck\n\n## План\n- accept: false\n\n## Журнал\n" + journal
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return task{path: path, slug: "task", body: body}, path
}

func taskFromPath(path, slug, body string) task {
	return task{path: path, slug: slug, body: body}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Обходы обязательного пункта правкой текста: пропуск, вписанный руками и ничем не перекрытый; удалённый или
// переименованный раздел плана; та же строка с другой командой. Удалённый пункт возвращается через tk plan.
func TestRequiredPlanItemTextEdits(t *testing.T) {
	const added = "- 2020-01-01 12:00 план — planner: добавлен обязательный пункт accept: ./accept.sh\n"
	head := "# Task\n\nСтатус: на проверке\n\n"
	cases := map[string]string{
		"вписанный пропуск": head + "## План\n- accept: ./accept.sh\n\n## Журнал\n" + added + "- 2020-01-01 12:05 план — worker: пропущен: accept — forged\n",
		"нет раздела":       head + "## Журнал\n" + added,
		"другой заголовок":  head + "## Plan\n- accept: ./accept.sh\n\n## Журнал\n" + added,
		"другая команда":    head + "## План\n- accept: true\n\n## Журнал\n" + added,
		// разметка журнала вокруг нетронутой строки «обязательный»
		"журнал переименован":  head + "## План\n- accept: true\n\n## Journal\n" + added,
		"пустой журнал раньше": head + "## Журнал\n\n## План\n- accept: true\n\n## Журнал\n" + added,
		"заголовок в журнале":  head + "## План\n- accept: true\n\n## Журнал\n## x\n" + added,
		"\\r в заголовке":      head + "## План\n- accept: true\n\n## Журнал\r\n" + added,
		// строка «обязательный» на месте, но склеена с предыдущей другим концом строки
		"\\r вместо \\n перед строкой": head + "## План\n- accept: true\n\n## Журнал\n- 2020-01-01 11:00 план — planner: добавлен пункт digest\r" + added,
		"U+2028 перед строкой":         head + "## План\n- accept: true\n\n## Журнал\n- 2020-01-01 11:00 план — planner: добавлен пункт digest\u2028" + added,
		// вторая строка с другой командой не меняет закреплённую первой
		"вторая строка с другой командой": head + "## План\n- accept: true\n\n## Журнал\n" + added + "- 2020-01-01 12:10 план — night: добавлен обязательный пункт accept: true\n",
		// любой другой символ на месте конца строки или невидимый в начале — запись не теряется, пункт неверен
		"\\v перед строкой":      head + "## План\n- accept: true\n\n## Журнал\n- 2020-01-01 11:00 план — planner: добавлен пункт digest\v" + added,
		"\\f перед строкой":      head + "## План\n- accept: true\n\n## Журнал\n- 2020-01-01 11:00 план — planner: добавлен пункт digest\f" + added,
		"\\x1c перед строкой":    head + "## План\n- accept: true\n\n## Журнал\n- 2020-01-01 11:00 план — planner: добавлен пункт digest\x1c" + added,
		"\\x1d перед строкой":    head + "## План\n- accept: true\n\n## Журнал\n- 2020-01-01 11:00 план — planner: добавлен пункт digest\x1d" + added,
		"\\x1e перед строкой":    head + "## План\n- accept: true\n\n## Журнал\n- 2020-01-01 11:00 план — planner: добавлен пункт digest\x1e" + added,
		"пробел в начале строки": head + "## План\n- accept: true\n\n## Журнал\n " + added,
		"BOM в начале строки":    head + "## План\n- accept: true\n\n## Журнал\n\xef\xbb\xbf" + added,
		// поддельная строка выше настоящей — с другой командой или без команды
		"поддельная строка выше":             head + "## План\n- accept: true\n\n## Журнал\n- 2020-01-01 11:00 план — night: добавлен обязательный пункт accept: true\n" + added,
		"поддельная строка без команды выше": head + "## План\n- accept: true\n\n## Журнал\n- 2020-01-01 11:00 план — night: добавлен обязательный пункт accept\n" + added,
	}
	for name, body := range cases {
		items := planItems(body)
		blocked := false
		for _, it := range items {
			if it.Name == "accept" && !it.Skipped && it.Required {
				blocked = it.Invalid != "" || it.Cmd == "./accept.sh"
			}
		}
		if !blocked {
			t.Errorf("%s: обязательный пункт обойдён: %#v", name, items)
		}
	}
	if items := planItems(cases["другая команда"]); len(items) != 1 || !strings.Contains(items[0].Invalid, "pinned with --required to") {
		t.Errorf("подмена команды не названа: %#v", items)
	}

	dir := filepath.Join(t.TempDir(), "docs", "tasks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "20200101-1200-MEDIUM-task.md")
	body := cases["нет раздела"]
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	for _, required := range []bool{true, false} {
		if code := addPlanItem(task{path: path, slug: "task", body: body}, options{by: "night", add: "accept: true", required: required}, &out, &errOut); code != exitUsage ||
			!strings.Contains(errOut.String(), "pinned") || read(t, path) != body {
			t.Fatalf("re-add with another command (required %v): %d %s", required, code, errOut.String())
		}
		errOut.Reset()
	}
	if code := addPlanItem(task{path: path, slug: "task", body: body}, options{by: "planner", add: "accept: ./accept.sh", required: true}, &out, &errOut); code != 0 {
		t.Fatalf("re-add of a removed required item: %d %s", code, errOut.String())
	}
	if items := planItems(read(t, path)); len(items) != 1 || items[0].Invalid != "" || items[0].Cmd != "./accept.sh" {
		t.Fatalf("after re-add: %#v", items)
	}
}

// Ввод tk, который ложится в тикет, — одна строка при любом конце строки: U+2028 в команде вписал бы строку
// «обязательный» выше настоящей.
func TestPlanInputLineSeparators(t *testing.T) {
	for _, sep := range []string{"\n", "\r", "\v", "\f", "\x1c", "\x00", "\u2028", "\u2029", "\u0085"} {
		for _, args := range [][]string{
			{"plan", "task", "--add", "zzz: true #" + sep + "- x план — night: добавлен обязательный пункт accept: true"},
			{"note", "task", "текст" + sep + "- готово"},
		} {
			if _, err := parseArgs(args); err == nil || !strings.Contains(err.Error(), "one line") {
				t.Errorf("%q: %v", args, err)
			}
		}
	}
}

// Пункт плана исполняет bash по абсолютному пути без файлов старта из окружения: BASH_ENV с exit 0 или
// поддельный bash в PATH не превращают падающую проверку в проходящую.
func TestPlanItemIgnoresShellStartupEnv(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Setenv("BASH_ENV", write("env.sh", "exit 0\n"))
	t.Setenv("ENV", filepath.Join(dir, "env.sh"))
	t.Setenv("BASH_FUNC_exit%%", "() { builtin exit 0; }")
	t.Setenv("SHELLOPTS", "noexec") // команды разбираются, но не исполняются: выход 0
	t.Setenv("BASHOPTS", "nullglob")
	// с SSH_CLIENT и без SHLVL bash читает ~/.bashrc и в -c
	write(".bashrc", "exit 0\n")
	t.Setenv("HOME", dir)
	t.Setenv("SSH_CLIENT", "192.0.2.1 22 22")
	t.Setenv("SHLVL", "0")
	t.Setenv("LD_PRELOAD", filepath.Join(dir, "none.so"))
	t.Setenv("LD_AUDIT", filepath.Join(dir, "none.so"))
	t.Setenv("LD_LIBRARY_PATH", dir)
	t.Setenv("LD_BIND_NOW", "1")
	t.Setenv("GCONV_PATH", dir) // с не-UTF-8 локалью glibc грузит отсюда модуль в сам bash
	t.Setenv("LOCPATH", dir)
	t.Setenv("MALLOC_PERTURB_", "1")
	t.Setenv("TZDIR", dir)
	if err := os.Mkdir(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("bin/bash", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", filepath.Join(dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd := `shopt -q nullglob && exit 5; [ -n "${ENV-}" ] && exit 6; [ -n "${LD_PRELOAD-}${LD_AUDIT-}${LD_LIBRARY_PATH-}${LD_BIND_NOW-}${GCONV_PATH-}${LOCPATH-}${MALLOC_PERTURB_-}${TZDIR-}" ] && exit 4; exit 3`
	if result := runPlanItem(dir, planJSON{Name: "accept", Cmd: cmd}, planTimeout); result.exit != 3 {
		t.Errorf("exit %d, want 3: %+v", result.exit, result)
	}
}

// Время «06:4x» принимается, помечается в строке журнала и даёт подсказку.
func TestJournalTextAcceptsAndMarksGuessedTime(t *testing.T) {
	for _, args := range [][]string{
		{"note", "task", "06:4x найдено"},
		{"note", "task", "в 06:4х найдено"},
		{"note", "task", "найдено (14:5x)"},
		{"done", "task", "--check", "прошло 06:4x"},
		{"reject", "task", "--why", "06:4x не нужно"},
		{"review", "task", "--note", "06:4X сдано"},
		{"note", "task", "в06:4x найдено"},
		{"note", "task", "в 06:4xМСК найдено"},
		{"skip", "task", "accept", "--why", "06:4x упало"},
		{"wait", "task", "--until", "сегодня 11:0x"},
	} {
		o, err := parseArgs(args)
		if err != nil {
			t.Errorf("%q: %v", args, err)
			continue
		}
		o.timeNow = time.Date(2026, 11, 2, 2, 54, 0, 0, time.Local)
		value := o.note
		if value == "" {
			value = o.check
		}
		if value == "" {
			value = o.why
		}
		if value == "" {
			value = o.until
		}
		if markedJournalText(o, value) == value {
			t.Errorf("%q: missing marker", args)
		}
	}
	_, dir := testRunMainRoot(t)
	path := writeRunMainTask(t, dir, "20200101-1200-MED-unmarked.md", "# Task\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	for _, text := range []string{"06:41 найдено", "box 06:4xyz", "шаг 1:4x", "код 99:9x", "6:4x найдено"} {
		var out, errout bytes.Buffer
		if code := runMain([]string{"note", "unmarked", text, "--by", "tester"}, time.Date(2026, 11, 12, 12, 0, 0, 0, time.Local), &out, &errout); code != 0 {
			t.Fatalf("note %q: code=%d stderr=%q", text, code, errout.String())
		}
		if got := lastStderrLine(errout.String()); got != "" {
			t.Errorf("note %q printed hint %q", text, got)
		}
		updated, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(updated), "note — tester: "+text) || strings.Contains(string(updated), "note — tester: "+text+" [время требует проверки]") {
			t.Errorf("note %q marked or missing: %s err=%v", text, updated, err)
		}
	}
}

// «~ЧЧ:ММ» позже метки записи — угаданное будущее время; раньше или в пределах минуты — проходит.
func TestJournalTextMarksApproxFutureTime(t *testing.T) {
	now := time.Date(2026, 11, 2, 2, 54, 30, 0, time.Local)
	_, dir := testRunMainRoot(t)
	path := writeRunMainTask(t, dir, "20200101-1200-MED-approx.md", "# Task\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	for _, c := range []struct {
		text string
		bad  bool
	}{
		{"сдано ~03:00", true},
		{"сдано ~ 03:00", true},
		{"сдано ~02:55", false},
		{"сдано ~02:40", false},
		{"сдано 03:00", false},
		{"вчера ~23:50", false},
		{"31.10 ~03:00 упало", false},
		{"02.11 ~03:00 упало", true},
		{"~02:40~03:00", true},
		{"~03:001", false},
		{"~03:00MSK", true},
		{"~03:00_", true},
		{"03:00 проверка", true},
		{" 03:00: сдано", true},
		{"02.11 03:00 CET сдано", true},
		{"02:55 сдано", false},
		{"02:40 сдано", false},
		{"31.10 03:00 упало", false},
		{"03:00-04:00 окно", false},
		{"03:00–04:00 окно", false},
		{"03:00 - 04:00 окно", false},
		{"сдано ~03:00-04:00", true},
		{" 03:00 проверка", true},
		{"2026-11-02 03:00 проверка", true},
		{"03:00 — сдано", true},
		{"03:00 — 2 проверки", true},
		{"03:00 - 99:99 проверка", true},
		{"03:00 - 04:001 проверка", true},
		{"03:00-4:30 окно", false},
		{"ждёт до 03:00", false},
		{"03:001", false},
	} {
		got := futureApproxTime(c.text, now)
		if (got != "") != c.bad {
			t.Errorf("%q: futureApproxTime = %q, want suspicious=%v", c.text, got, c.bad)
		}
		var out, errout bytes.Buffer
		if code := runMain([]string{"note", "approx", c.text, "--by", "tester"}, now, &out, &errout); code != 0 {
			t.Fatalf("runMain note %q: code=%d stderr=%q", c.text, code, errout.String())
		}
		updated, readErr := os.ReadFile(path)
		marked := strings.Contains(string(updated), "note — tester: "+c.text+" [время требует проверки]")
		if readErr != nil || marked != c.bad {
			t.Errorf("%q: journal marked=%v want %v, err=%v", c.text, marked, c.bad, readErr)
		}
		if c.bad {
			requireHintParts(t, errout.String(), "в note", "похоже на время позже метки записи", "[время требует проверки]")
		} else if got := lastStderrLine(errout.String()); got != "" {
			t.Errorf("%q: unexpected hint %q", c.text, got)
		}
	}
	zone, err := time.LoadLocation("Europe/Belgrade")
	if err != nil {
		t.Fatal(err)
	}
	// день перевода часов: 23 часа в сутках, «00:00» в 12:30 — по стене на 11:30 впереди
	if futureApproxTime("00:00 проверка", time.Date(2026, 3, 29, 12, 30, 0, 0, zone)) == "" {
		t.Error("00:00 at 12:30 on a clock-change day is ahead by wall clock")
	}
	late := time.Date(2026, 11, 2, 23, 58, 0, 0, time.Local)
	if futureApproxTime("прошло ~00:05", late) == "" {
		t.Error("~00:05 at 23:58 is the next day, later than the entry")
	}
}

func TestTimeHintIsLastOnlyAfterSuccessfulCommand(t *testing.T) {
	now := time.Date(2026, 11, 2, 23, 0, 0, 0, time.Local)
	if hint := formatTimeHint(timeHintFacts{field: "note", suspect: "06:4x", kind: "похоже на угаданное время", marked: true}); hint == "" || !strings.Contains(hint, "точное время можно взять из date") {
		t.Fatalf("hint = %q", hint)
	}
	if _, err := parseArgs([]string{"note", "ticket", "one\ntwo"}); err == nil || !strings.Contains(err.Error(), "one line") {
		t.Fatalf("newline was not rejected: %v", err)
	}
	var out, errout bytes.Buffer
	code := runMain([]string{"note"}, now, &out, &errout)
	if code != exitUsage || strings.Contains(errout.String(), "точное время — из date") || !strings.Contains(errout.String(), "tk -h for usage") {
		t.Fatalf("failed command hint: code %d, stderr %q", code, errout.String())
	}
	if got := markedJournalText(options{timeNow: now}, "06:4x"); got != "06:4x [время требует проверки]" {
		t.Fatalf("marked text = %q", got)
	}
	if got := markedJournalText(options{timeNow: now}, "22:41"); got != "22:41" {
		t.Fatalf("exact time was marked: %q", got)
	}
}

func TestGuessedTimeNoteSucceedsWithMarkerAndHint(t *testing.T) {
	root := t.TempDir()
	taskDir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(taskDir, "20200101-1200-MED-task.md")
	body := "# Task\n\nСтатус: открыт\n\n## Что сделать\ntext\n\n## Приёмка\ncheck\n\n## Журнал\n- 2020-01-01 12:00 открыт — author\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
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
	var out, errout bytes.Buffer
	now := time.Now()
	cases := []string{
		"06:4x found",
		"06:4х found",
		"~" + now.Add(10*time.Minute).Format("15:04") + " found",
		now.Add(10*time.Minute).Format("15:04") + " found",
	}
	for _, note := range cases {
		out.Reset()
		errout.Reset()
		code := runMain([]string{"note", "task", note, "--by", "tester"}, now, &out, &errout)
		updated, readErr := os.ReadFile(path)
		lines := strings.Split(strings.TrimSpace(errout.String()), "\n")
		if code != 0 || readErr != nil || !strings.Contains(string(updated), note+" [время требует проверки]") {
			t.Fatalf("note %q: code=%d, body=%s, stderr=%q, err=%v", note, code, updated, errout.String(), readErr)
		}
		if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], "в note") || !strings.Contains(lines[len(lines)-1], "[время требует проверки]") || !strings.Contains(out.String(), "noted") {
			t.Fatalf("hint/output for %q: stdout=%q stderr=%q", note, out.String(), errout.String())
		}
	}
	for _, note := range []string{now.Add(-10*time.Minute).Format("15:04") + " exact", "waiting until 23:59", "17:40-18:00 window"} {
		if got := markedJournalText(options{timeNow: now}, note); got != note || futureApproxTime(note, now) != "" {
			t.Errorf("non-warning text %q got marked/hinted as %q", note, got)
		}
	}
}

func TestMarkedJournalFieldsAndUnmarkedHeaders(t *testing.T) {
	now := time.Date(2026, 11, 11, 23, 0, 0, 0, time.Local)
	o := options{by: "tester", cmd: "done", check: "06:4x check", timeNow: now}
	if got := transitionJournal(o, "готово", 0); !strings.Contains(got, "06:4x check [время требует проверки]") {
		t.Fatalf("done check has no marker: %s", got)
	}
	donePath := filepath.Join(t.TempDir(), "20200101-1200-MED-done.md")
	doneBody := "# Task\n\nСтатус: на проверке\nИсполнитель: worker\n\n## Приёмка\n- check\n\n## Журнал\n"
	if err := os.WriteFile(donePath, []byte(doneBody), 0o644); err != nil {
		t.Fatal(err)
	}
	var doneOut, doneErr bytes.Buffer
	if code := transition(task{path: donePath, slug: "done", body: doneBody}, o, &doneOut, &doneErr); code != 0 {
		t.Fatalf("done --check: code=%d stderr=%q", code, doneErr.String())
	}
	doneUpdated, err := os.ReadFile(donePath)
	if err != nil || !strings.Contains(string(doneUpdated), "06:4x check [время требует проверки]") {
		t.Fatalf("done command journal: %s, %v", doneUpdated, err)
	}
	o.cmd, o.why = "reject", "06:4x reason"
	if got := transitionJournal(o, "отклонён", 0); !strings.Contains(got, "06:4x reason [время требует проверки]") {
		t.Fatalf("transition reason has no marker: %s", got)
	}
	o.cmd, o.until = "wait", "2000-01-01 06:4x"
	journalLine := transitionJournal(o, "ждёт", 0)
	if !strings.Contains(journalLine, "06:4x [время требует проверки]") {
		t.Fatalf("wait journal text has no marker: %s", journalLine)
	}
	waiting := setWaiting("Статус: открыт\n", o.until)
	if !strings.Contains(waiting, "Ждёт: 2000-01-01 06:4x") || strings.Contains(waiting, "Ждёт: 2000-01-01 06:4x [") {
		t.Fatalf("wait header changed: %s", waiting)
	}
	if _, ok := timearg.Deadline(o.until, now); ok {
		t.Fatalf("guessed clock became a deadline: %s", o.until)
	}
	if _, ok := timearg.Deadline("2099-01-01 10:00", now); !ok {
		t.Fatal("exact dated clock stopped being a deadline")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "tasks")
	var out, errout bytes.Buffer
	if code := create(root, dir, options{cmd: "new", id: "header-reason", prio: "MED", by: "tester", why: "06:4x reason"}, &out, &errout); code != 0 {
		t.Fatalf("create: %d, %s", code, errout.String())
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("created files %v, err=%v", files, err)
	}
	created, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil || !strings.Contains(string(created), "Причина: 06:4x reason\n") || strings.Contains(string(created), "Причина: 06:4x reason [") {
		t.Fatalf("reason header changed: %s, %v", created, err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	var waitOut, waitErr bytes.Buffer
	until := "2000-01-01 06:4x"
	code := runMain([]string{"wait", "header-reason", "--until", until, "--by", "tester"}, now, &waitOut, &waitErr)
	lines := strings.Split(strings.TrimSpace(waitErr.String()), "\n")
	if code != 0 || len(lines) != 1 || !strings.Contains(lines[len(lines)-1], "срок не распознан") {
		t.Fatalf("wait result: code=%d, stdout=%q stderr=%q", code, waitOut.String(), waitErr.String())
	}
	updated, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil || !strings.Contains(string(updated), "Ждёт: "+until+"\n") || strings.Contains(string(updated), "Ждёт: "+until+" [") {
		t.Fatalf("wait header changed: %s, %v", updated, err)
	}
	waitingTask, err := find(dir, "header-reason")
	if err != nil {
		t.Fatal(err)
	}
	obj := taskObject(root, waitingTask, "", false)
	if obj.Due != "" || obj.Overdue {
		t.Fatalf("guessed time became JSON due: %#v", obj)
	}
	var jsonOut, jsonErr bytes.Buffer
	if code := list("", options{cmd: "ls", root: root, json: true}, &jsonOut, &jsonErr); code != 0 {
		t.Fatalf("ls --json: code=%d out=%q err=%q", code, jsonOut.String(), jsonErr.String())
	}
	var rows []taskJSON
	if err := json.Unmarshal(jsonOut.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].Due != "" {
		t.Fatalf("ls --json guessed deadline: rows=%#v raw=%q err=%v", rows, jsonOut.String(), err)
	}
	var listOut, listErr bytes.Buffer
	if code := list("", options{cmd: "ls", root: root, due: true}, &listOut, &listErr); code != 0 || strings.Contains(listOut.String(), "header-reason") {
		t.Fatalf("ls --due includes guessed clock: code=%d out=%q err=%q", code, listOut.String(), listErr.String())
	}
}

func TestSkippedPlanReasonIsMarkedAndPlanStaysSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "20200101-1200-MED-task.md")
	body := "# Task\n\nСтатус: открыт\n\n## План\n- accept: true\n\n## Журнал\n- 2020-01-01 12:00 план — tester: добавлен пункт accept: true\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	o := options{by: "tester", item: "accept", why: "06:4x failed", timeNow: time.Now()}
	if code := skipPlanItem(task{path: path, slug: "task", body: body}, o, &out, &errout); code != 0 {
		t.Fatalf("skip: %d, %s", code, errout.String())
	}
	updated, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(updated), "06:4x failed [время требует проверки]") {
		t.Fatalf("skip reason: %s, %v", updated, err)
	}
	items := planItems(string(updated))
	if len(items) != 1 || !items[0].Skipped {
		t.Fatalf("plan item is not skipped: %#v", items)
	}
}

func testRunMainRoot(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
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
	return root, dir
}

func writeRunMainTask(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func lastStderrLine(stderr string) string {
	stderr = strings.TrimRight(stderr, "\r\n")
	if i := strings.LastIndexByte(stderr, '\n'); i >= 0 {
		return stderr[i+1:]
	}
	return stderr
}

func requireHintParts(t *testing.T, stderr string, parts ...string) {
	t.Helper()
	line := lastStderrLine(stderr)
	for _, part := range parts {
		if !strings.Contains(line, part) {
			t.Fatalf("last stderr line %q lacks %q; full stderr: %q", line, part, stderr)
		}
	}
	if strings.Contains(line, "проверьте") || strings.Contains(line, "напишите") {
		t.Fatalf("hint uses imperative phrasing: %q", line)
	}
}

func TestRunMainTimeHintsForJournalCommands(t *testing.T) {
	cases := []struct {
		name, command, status, extra, journal, hintField string
	}{
		{"done check", "done", "на проверке", "Исполнитель: worker\n\n## Приёмка\n- check", "06:4x checked", "--check"},
		{"skip why", "skip", "открыт", "## План\n- accept: true\n\n", "06:4x skipped", "--why"},
		{"review note", "review", "открыт", "Исполнитель: worker\n\n## Приёмка\n- check", "06:4x reviewed", "--note"},
		{"unskip why", "unskip", "открыт", "## План\n- accept: true\n\n", "06:4x returned", "--why"},
		{"repin why", "repin", "открыт", "## План\n- accept: false\n\nИсполнитель: worker", "06:4x repinned", "--why"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, dir := testRunMainRoot(t)
			journal := ""
			if tc.command == "repin" {
				journal = "- 2020-01-01 12:00 план — author: добавлен обязательный пункт accept: false\n"
			}
			if tc.command == "skip" {
				journal = "- 2020-01-01 12:00 план — author: добавлен пункт accept: true\n"
			}
			if tc.command == "unskip" {
				journal = "- 2020-01-01 12:00 план — author: добавлен пункт accept: true\n- 2020-01-01 12:01 план — author: пропущен: accept — skip\n"
			}
			body := fmt.Sprintf("# Task\n\nСтатус: %s\n%s\n\n## Приёмка\n- check\n\n## Журнал\n%s", tc.status, tc.extra, journal)
			writeRunMainTask(t, dir, "20200101-1200-MED-task.md", body)
			oldSession := session
			if tc.command == "repin" {
				session = func() string { return "new-agent" }
				t.Cleanup(func() { session = oldSession })
			}
			args := []string{tc.command, "task"}
			switch tc.command {
			case "done":
				args = append(args, "--check", tc.journal, "--by", "reviewer")
			case "skip":
				args = append(args, "accept", "--why", tc.journal, "--by", "tester")
			case "review":
				args = append(args, "--note", tc.journal, "--by", "reviewer")
			case "unskip":
				args = append(args, "accept", "--why", tc.journal, "--by", "tester")
			case "repin":
				args = []string{"plan", "task", "--repin", "accept: true", "--why", tc.journal, "--by", "reviewer"}
			}
			var out, errout bytes.Buffer
			if code := runMain(args, time.Now(), &out, &errout); code != 0 {
				t.Fatalf("runMain %v: code=%d stdout=%q stderr=%q", args, code, out.String(), errout.String())
			}
			updated, err := os.ReadFile(filepath.Join(dir, "20200101-1200-MED-task.md"))
			if err != nil || !strings.Contains(string(updated), tc.journal+" [время требует проверки]") {
				t.Fatalf("journal text lacks marker: %s, err=%v", updated, err)
			}
			requireHintParts(t, errout.String(), "06:4x", tc.hintField, "[время требует проверки]")
			if tc.command == "skip" {
				items := planItems(string(updated))
				if len(items) != 1 || !items[0].Skipped {
					t.Fatalf("skip did not remain active: %#v", items)
				}
			}
		})
	}
}

func TestRunMainHeaderReasonsStayUnmarked(t *testing.T) {
	for _, operation := range []string{"new", "set", "start"} {
		t.Run(operation, func(t *testing.T) {
			_, dir := testRunMainRoot(t)
			if operation != "new" {
				status := "открыт"
				body := "# Task\n\nСтатус: " + status + "\n\n## Приёмка\n- check\n\n## Журнал\n"
				writeRunMainTask(t, dir, "20200101-1200-MED-"+operation+"-task.md", body)
			}
			args := []string{operation}
			switch operation {
			case "new":
				args = append(args, "new-reason")
			case "set":
				args = append(args, "set-task")
			case "start":
				args = append(args, "start-task")
			}
			args = append(args, "--why", "06:4x причина", "--by", "tester")
			var out, errout bytes.Buffer
			if code := runMain(args, time.Now(), &out, &errout); code != 0 {
				t.Fatalf("runMain %v: code=%d stdout=%q stderr=%q", args, code, out.String(), errout.String())
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 1 {
				t.Fatalf("files=%v err=%v", files, err)
			}
			updated, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
			if err != nil || !strings.Contains(string(updated), "Причина: 06:4x причина\n") || strings.Contains(string(updated), "Причина: 06:4x причина [") {
				t.Fatalf("reason header marked: %s, err=%v", updated, err)
			}
			requireHintParts(t, errout.String(), "«06:4x»", "--why", "Причина:", "без пометки")
		})
	}
}

func TestRunMainFailureAfterParseDoesNotPrintTimeHint(t *testing.T) {
	_, _ = testRunMainRoot(t)
	var out, errout bytes.Buffer
	code := runMain([]string{"note", "missing", "06:4x absent", "--by", "tester"}, time.Now(), &out, &errout)
	if code == 0 || !strings.Contains(errout.String(), "not found") {
		t.Fatalf("missing ticket result: code=%d stderr=%q", code, errout.String())
	}
	if strings.Contains(errout.String(), "точное время") || strings.Contains(errout.String(), "[время требует проверки]") {
		t.Fatalf("failed command printed a time hint: %q", errout.String())
	}
}

func TestRunMainUntilGuessedClocksAndExactFuture(t *testing.T) {
	_, dir := testRunMainRoot(t)
	path := writeRunMainTask(t, dir, "20200101-1200-MED-wait.md", "# Wait\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	forms := []string{
		"12.11 06:4x: итог", "12.11 06:4x, итог", "12.11 06:4x; итог",
		"2026-11-12 06:4x: итог", "12.11 06:4xМСК",
		"12.11 в 06:4x", "12.11 6:4x: итог", "13.11 6:4x итог",
		"13.11 до 06:4x", "13.11 к 06:4x", "13.11 около 06:4x", "13.11 В 06:4x",
	}
	for _, until := range forms {
		var out, errout bytes.Buffer
		if code := runMain([]string{"wait", "wait", "--until", until, "--by", "tester"}, time.Now(), &out, &errout); code != 0 {
			t.Fatalf("wait %q: code=%d stderr=%q", until, code, errout.String())
		}
		requireHintParts(t, errout.String(), "tk ls --due")
		if strings.Count(strings.TrimSpace(errout.String()), "\n") != 0 {
			t.Fatalf("wait %q printed contradictory diagnostics: %q", until, errout.String())
		}
		if !strings.Contains(lastStderrLine(errout.String()), "срок не распознан") {
			t.Fatalf("wait %q lacks deadline explanation: %q", until, errout.String())
		}
		updated, err := os.ReadFile(path)
		journalLines := strings.Split(strings.TrimSpace(string(updated)), "\n")
		lastJournalLine := journalLines[len(journalLines)-1]
		if err != nil || !strings.Contains(string(updated), "Ждёт: ") || strings.Contains(string(updated), "Ждёт: "+until+" [") || !strings.Contains(lastJournalLine, "[время требует проверки]") {
			t.Fatalf("wait field %q changed: %s err=%v", until, updated, err)
		}
		waitTask, err := find(dir, "wait")
		if err != nil {
			t.Fatal(err)
		}
		if obj := taskObject("", waitTask, "", false); obj.Due != "" {
			t.Fatalf("guessed clock %q became due %q", until, obj.Due)
		}
		var dueOut, dueErr bytes.Buffer
		if code := list("", options{cmd: "ls", root: filepath.Dir(filepath.Dir(dir)), due: true}, &dueOut, &dueErr); code != 0 || strings.Contains(dueOut.String(), "wait") {
			t.Fatalf("ls --due %q: code=%d out=%q err=%q", until, code, dueOut.String(), dueErr.String())
		}
	}
	todayClock := "23:30 итог бойца"
	var todayOut, todayErr bytes.Buffer
	if code := runMain([]string{"wait", "wait", "--until", todayClock, "--by", "tester"}, time.Date(2026, 11, 11, 23, 0, 0, 0, time.Local), &todayOut, &todayErr); code != 0 {
		t.Fatalf("same-day exact future wait: %d %q", code, todayErr.String())
	}
	if strings.Contains(todayErr.String(), "время") || strings.Contains(todayErr.String(), "пометкой") {
		t.Fatalf("same-day exact future wait got a hint: %q", todayErr.String())
	}
	todayBody, err := os.ReadFile(path)
	todayStored := "2026-11-11 " + todayClock
	if err != nil || !strings.Contains(string(todayBody), "Ждёт: "+todayStored) || strings.Contains(string(todayBody), todayStored+" [") {
		t.Fatalf("same-day exact future wait marked: %s, %v", todayBody, err)
	}
	exact := time.Now().Add(2*time.Hour).Format("2006-01-02 15:04") + " итог бойца"
	var out, errout bytes.Buffer
	if code := runMain([]string{"wait", "wait", "--until", exact, "--by", "tester"}, time.Now(), &out, &errout); code != 0 {
		t.Fatalf("exact future wait: %d %q", code, errout.String())
	}
	if lastStderrLine(errout.String()) != "" || strings.Contains(errout.String(), "время требует проверки") {
		t.Fatalf("exact future wait got a hint or mark: %q", errout.String())
	}
	updated, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(updated), "Ждёт: "+exact) || strings.Contains(string(updated), exact+" [") {
		t.Fatalf("exact future wait changed: %s, %v", updated, err)
	}
}

func TestRunMainUntilHintReflectsDeadlineParsing(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		name, until, want string
	}{
		{"past-date", "10.11 18:00 итог 06:4x", "срок распознан:"},
		{"tomorrow", "12.11 18:00 итог, прошлый был 06:4x", "срок распознан:"},
		{"deadline-before-guessed", "13.11 18:00 06:4x", "срок распознан:"},
		{"no-deadline", "12.11 в 06:4x", "срок не распознан"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, dir := testRunMainRoot(t)
			path := writeRunMainTask(t, dir, "20200101-1200-MED-wait.md", "# Wait\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
			var out, errout bytes.Buffer
			if code := runMain([]string{"wait", "wait", "--until", tc.until, "--by", "tester"}, now, &out, &errout); code != 0 {
				t.Fatalf("wait: code=%d stderr=%q", code, errout.String())
			}
			hint := lastStderrLine(errout.String())
			if !strings.Contains(hint, tc.want) || !strings.Contains(hint, "«06:4x»") || !strings.Contains(hint, "похоже на угаданное время") || !strings.Contains(hint, "[время требует проверки]") {
				t.Fatalf("unexpected hint: %q", hint)
			}
			updated, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(updated), " [время требует проверки]") {
				t.Fatalf("journal not marked: %s err=%v", updated, err)
			}
			var jsonOut, jsonErr bytes.Buffer
			if code := list("", options{cmd: "ls", root: root, json: true}, &jsonOut, &jsonErr); code != 0 {
				t.Fatalf("ls --json: code=%d out=%q err=%q", code, jsonOut.String(), jsonErr.String())
			}
			var rows []taskJSON
			if err := json.Unmarshal(jsonOut.Bytes(), &rows); err != nil || len(rows) != 1 {
				t.Fatalf("ls --json rows=%#v raw=%q err=%v", rows, jsonOut.String(), err)
			}
			got := rows[0].Due
			if tc.name != "no-deadline" && (got == "" || !strings.Contains(hint, got)) {
				t.Fatalf("hint deadline differs from due: hint=%q due=%q stderr=%q", hint, got, errout.String())
			}
			if tc.name == "no-deadline" && got != "" {
				t.Fatalf("guessed time became due %q", got)
			}
		})
	}
}

func TestRunMainNoteMarksLaterGuessedClock(t *testing.T) {
	_, dir := testRunMainRoot(t)
	path := writeRunMainTask(t, dir, "20200101-1200-MED-later-clock.md", "# Task\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.Local)
	note := "шаг 1:4x, упало в 06:4x"
	var out, errout bytes.Buffer
	if code := runMain([]string{"note", "later-clock", note, "--by", "tester"}, now, &out, &errout); code != 0 {
		t.Fatalf("note: code=%d stderr=%q", code, errout.String())
	}
	updated, err := os.ReadFile(path)
	hint := lastStderrLine(errout.String())
	if err != nil || !strings.Contains(string(updated), note+" [время требует проверки]") || !strings.Contains(hint, "«06:4x»") || strings.Contains(hint, "«1:4x»") || !strings.Contains(hint, "строка журнала дополнена пометкой") {
		t.Fatalf("note mark/hint mismatch: body=%s hint=%q err=%v", updated, hint, err)
	}
}

func TestRunMainUntilOneDigitWithoutDateHintsWithoutMarker(t *testing.T) {
	_, dir := testRunMainRoot(t)
	path := writeRunMainTask(t, dir, "20200101-1200-MED-one-digit.md", "# Task\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	var out, errout bytes.Buffer
	if code := runMain([]string{"wait", "one-digit", "--until", "6:4x итог", "--by", "tester"}, time.Now(), &out, &errout); code != 0 {
		t.Fatalf("wait: code=%d stderr=%q", code, errout.String())
	}
	hint := lastStderrLine(errout.String())
	if !strings.Contains(hint, "«6:4x»") || !strings.Contains(hint, "строка журнала записана без пометки") || !strings.Contains(hint, "срок не распознан") || strings.Contains(hint, "дополнена пометкой") {
		t.Fatalf("unexpected hint: %q", hint)
	}
	if strings.Count(strings.TrimSpace(errout.String()), "\n") != 0 {
		t.Fatalf("multiple deadline messages: %q", errout.String())
	}
	updated, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(updated), "Ждёт: 6:4x итог\n") || strings.Contains(string(updated), "6:4x итог [время требует проверки]") {
		t.Fatalf("one-digit deadline was marked: %s err=%v", updated, err)
	}
}

func TestRunMainUntilExactTimeAfterWordKeepsExistingDeadline(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.Local)
	root, dir := testRunMainRoot(t)
	writeRunMainTask(t, dir, "20200101-1200-MED-exact.md", "# Exact\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	var out, errout bytes.Buffer
	if code := runMain([]string{"wait", "exact", "--until", "13.11 до 18:00", "--by", "tester"}, now, &out, &errout); code != 0 {
		t.Fatalf("wait: code=%d stderr=%q", code, errout.String())
	}
	var jsonOut, jsonErr bytes.Buffer
	if code := list("", options{cmd: "ls", root: root, json: true}, &jsonOut, &jsonErr); code != 0 {
		t.Fatalf("ls --json: code=%d out=%q err=%q", code, jsonOut.String(), jsonErr.String())
	}
	var rows []taskJSON
	if err := json.Unmarshal(jsonOut.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].Due != "2026-11-13T00:00:00+01:00" || lastStderrLine(errout.String()) != "" {
		t.Fatalf("exact time after word changed: rows=%#v stderr=%q err=%v", rows, errout.String(), err)
	}
}

func TestTaskNameParserSupportsCurrentAndLegacyNames(t *testing.T) {
	for _, tc := range []struct {
		name, prio, diff, slug string
	}{
		{"20200101-1000-MEDIUM-legacy-name.md", "MEDIUM", "", "legacy-name"},
		{"20200101-1000-HIGH-ARCH-new-name.md", "HIGH", "ARCH", "new-name"},
	} {
		got, ok := parseTaskName(tc.name)
		if !ok || got.prio != tc.prio || got.diff != tc.diff || got.slug != tc.slug {
			t.Errorf("parseTaskName(%q) = %#v, %v", tc.name, got, ok)
		}
	}
	if got := normalizePrio("MEDIUM"); got != "MED" {
		t.Errorf("normalizePrio(MEDIUM) = %q", got)
	}
}

func TestSetRenameFindsTicketByPriorName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldName := "20200101-1000-MED-old-name.md"
	path := filepath.Join(dir, oldName)
	body := "# Old\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n- 2020-01-01 10:00 открыт — author\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ticket := task{path: path, slug: "old-name", prio: "MED", body: body}
	var out, errout bytes.Buffer
	if code := setTask(ticket, options{prio: "HIGH", why: "urgent", by: "actor"}, &out, &errout); code != 0 {
		t.Fatalf("setTask = %d, stderr %q", code, errout.String())
	}
	newPath := filepath.Join(dir, "20200101-1000-HIGH-old-name.md")
	updatedBody, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updatedBody), "Причина: urgent") || !strings.Contains(string(updatedBody), oldName+" → 20200101-1000-HIGH-old-name.md") {
		t.Fatalf("updated ticket lacks cause or rename journal: %s", updatedBody)
	}
	for _, id := range []string{oldName, strings.TrimSuffix(oldName, ".md")} {
		if got, err := find(dir, id); err != nil || filepath.Base(got.path) != filepath.Base(newPath) {
			t.Errorf("find(%q) = %v, %v", id, got.path, err)
		}
	}
}

func TestLegacyTaskJSONHasNormalizedMetadataFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "20200101-1000-MEDIUM-legacy.md")
	got := taskObject("", task{path: path, slug: "legacy", prio: "MEDIUM", body: "# Legacy\n\nСтатус: открыт\n"}, "", false)
	if got.Prio != "MED" || got.Diff != "" || got.Back != "" || got.Why != "" {
		t.Errorf("legacy task JSON fields: %#v", got)
	}
}

func TestJournalContinuationReadersUseRecordHeaders(t *testing.T) {
	t.Run("rename lookup", func(t *testing.T) {
		body := "# T\n\n## Журнал\n- 2026-01-01 10:00 переименован — tester: old.md → new.md\n      continued\n"
		ticket := task{path: "/repo/docs/tasks/20260101-1000-MED-new.md", slug: "new", body: body}
		if !matchesID(ticket, "old") {
			t.Fatal("rename lookup lost the record with a continuation")
		}
	})
	t.Run("actor regex stops at record header", func(t *testing.T) {
		body := "# T\n\n## Журнал\n- 2026-01-01 10:00 note\n      x — y: continuation\n"
		if actors := journalActors(body); len(actors) != 0 {
			t.Fatalf("continuation supplied a journal actor: %#v", actors)
		}
	})
	t.Run("completion ignores note continuation", func(t *testing.T) {
		body := "# T\n\n## Журнал\n- 2026-01-01 10:00 готово — reviewer: verified\n      verified continuation\n- 2026-01-02 10:00 note — tester: memo\n      готово — fake: not a completion\n"
		checker, check, _ := completion(body)
		if checker != "reviewer" || check != "verified" {
			t.Fatalf("completion = %q %q", checker, check)
		}
	})
	t.Run("opener returns the record value", func(t *testing.T) {
		body := "# T\n\n## Журнал\n- 2026-01-01 10:00 открыт — requester\n      continuation\n"
		if got := opener(body); got != "requester" {
			t.Fatalf("opener = %q", got)
		}
	})
	t.Run("plan skip reason reads a record with continuation", func(t *testing.T) {
		body := "# T\n\n## Журнал\n- 2026-01-01 10:00 план — tester: пропущен: item — reason\n      continuation\n"
		if got, ok := planSkipReason(body, "item"); !ok || got != "reason" {
			t.Fatalf("plan skip reason = %q, %v", got, ok)
		}
	})
}

func TestJournalJSONUsesListFlagAndShowKind(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "20260101-1000-MED-journal.md")
	body := "# Journal\n\nСтатус: открыт\nВид: журнал\n\n## Приёмка\n- check\n\n## Журнал\n- 2026-01-01 10:00 note — tester: first\n      second\n      third\n- 2026-01-02 10:00 note — tester: next\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var listOut, listErr bytes.Buffer
	if code := list(root, options{json: true}, &listOut, &listErr); code != 0 {
		t.Fatalf("list: code %d, stderr %q", code, listErr.String())
	}
	var listRows []map[string]any
	if err := json.Unmarshal(listOut.Bytes(), &listRows); err != nil {
		t.Fatal(err)
	}
	if len(listRows) != 1 || listRows[0]["journal"] != true {
		t.Fatalf("ls JSON = %#v", listRows)
	}
	var showOut, showErr bytes.Buffer
	if code := showJSON(root, task{path: path, slug: "journal", status: "открыт", prio: "MED", body: body}, &showOut, &showErr); code != 0 {
		t.Fatalf("show: code %d, stderr %q", code, showErr.String())
	}
	t.Logf("tk show --json: %s", strings.TrimSpace(showOut.String()))
	var showRow map[string]any
	if err := json.Unmarshal(showOut.Bytes(), &showRow); err != nil {
		t.Fatal(err)
	}
	journal := showRow["journal"].([]any)
	if showRow["kind"] != "журнал" || len(journal) != 2 || journal[0] != "- 2026-01-01 10:00 note — tester: first\n      second\n      third" || journal[1] != "- 2026-01-02 10:00 note — tester: next" {
		t.Fatalf("show JSON = %#v", showRow)
	}
}

func TestLintRequiresReasonOnlyAfterCutoff(t *testing.T) {
	dir := t.TempDir()
	body := "# Ticket\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n- 2020-01-01 10:00 открыт — actor\n"
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"20261103-2359-HIGH-old.md", false},
		{"20261104-0000-HIGH-new.md", true},
	} {
		path := filepath.Join(dir, tc.name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		issues, err := lintTicket(path, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		found := strings.Contains(strings.Join(issues, "\n"), "HIGH without Причина")
		if found != tc.want {
			t.Errorf("lint %s reason issue = %v, want %v: %v", tc.name, found, tc.want, issues)
		}
	}
}

func TestAbsoluteToCreatesAndPrintsTicketPath(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	o, err := parseArgs([]string{"new", "requested", "--to", target, "--back", "request-session", "--diff", "ARCH", "--by", "author"})
	if err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := createAtBase(base, filepath.Join(base, "docs", "tasks"), o, &out, &errout); code != 0 {
		t.Fatalf("new --to: %d %s", code, errout.String())
	}
	path := strings.TrimSpace(out.String())
	if !filepath.IsAbs(path) || filepath.Dir(path) != filepath.Join(target, "docs", "tasks") {
		t.Fatalf("reported path = %q", path)
	}
	body, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(body), "Заказчик: request-session") || !strings.Contains(filepath.Base(path), "-ARCH-requested.md") {
		t.Fatalf("created ticket = %s, %v", body, err)
	}
}

func TestNewRejectsInvalidCommandFlagsAndBack(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"new", "task", "--take"}, "--take: flag only for start"},
		{[]string{"set", "task", "--to", "project"}, "--to: flag only for new"},
		{[]string{"start", "task", "--back", "request"}, "--back: flag only for new or set"},
		{[]string{"ls", "--diff", "DO"}, "--prio/--diff: flag only for new, set, or start"},
		{[]string{"next", "--prio", "HIGH"}, "--prio/--diff: flag only for new, set, or start"},
		{[]string{"show", "task", "--why", "reason"}, "--why: flag only for new, set, start, reject, release, skip, unskip, or plan --repin"},
		{[]string{"new", "task", "--back", "two words"}, "--back must be a name without spaces"},
		{[]string{"new", "task", "--back", "bad\nname"}, "--back must be a name without spaces"},
	} {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			if _, err := parseArgs(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseArgs error = %v, want containing %q", err, tc.want)
			}
		})
	}
	for _, args := range [][]string{{"new", "task", "--prio", "HIGH"}, {"new", "task", "--diff", "NOPE"}} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%q) succeeded", args)
		}
	}
}

func TestSetAndStartInsertWhyWithoutStatus(t *testing.T) {
	for _, command := range []string{"set", "start"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			oldPath := filepath.Join(dir, "20261105-1000-MED-legacy.md")
			body := "# Legacy title\n\n## Что сделать\nwork\n\n## Приёмка\ncheck\n"
			if err := os.WriteFile(oldPath, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			ticket := task{path: oldPath, slug: "legacy", prio: "MED", status: "открыт", body: body}
			var out, errout bytes.Buffer
			o := options{cmd: command, prio: "HIGH", why: "urgent", by: "actor"}
			var code int
			if command == "set" {
				code = setTask(ticket, o, &out, &errout)
			} else {
				code = transition(ticket, o, &out, &errout)
			}
			newPath := filepath.Join(dir, "20261105-1000-HIGH-legacy.md")
			updated, err := os.ReadFile(newPath)
			want := "# Legacy title\nПричина: urgent\n"
			if command == "start" {
				want = "Статус: в работе\nПричина: urgent\n"
			}
			if code != 0 || err != nil || !strings.Contains(string(updated), want) {
				t.Fatalf("%s result=%d body=%q err=%v stderr=%q", command, code, updated, err, errout.String())
			}
		})
	}
}

func TestSetAndStartRequireWhyOnlyForPriorityPromotion(t *testing.T) {
	for _, command := range []string{"set", "start"} {
		for _, tc := range []struct {
			name, oldPrio, newPrio, diff string
			wantCode                     int
		}{
			{name: "same priority", oldPrio: "HIGH", newPrio: "HIGH"},
			{name: "diff only", oldPrio: "HIGH", newPrio: "HIGH", diff: "DO"},
			{name: "downgrade", oldPrio: "HIGH", newPrio: "LOW"},
			{name: "promotion requires reason", oldPrio: "MED", newPrio: "HIGH", wantCode: exitUsage},
		} {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				oldPath := filepath.Join(dir, "20261105-1000-"+tc.oldPrio+"-ticket.md")
				body := "# Ticket\n\nСтатус: открыт\n\n## Приёмка\ncheck\n\n## Журнал\n"
				if command == "start" {
					body = "# Ticket\n\nСтатус: в работе\nИсполнитель: actor\n\n## Приёмка\ncheck\n\n## Журнал\n"
				}
				if err := os.WriteFile(oldPath, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				o := options{cmd: command, prio: tc.newPrio, diff: tc.diff, by: "actor"}
				var out, errout bytes.Buffer
				ticket := task{path: oldPath, slug: "ticket", prio: tc.oldPrio, status: "открыт", body: body}
				var code int
				if command == "start" {
					code = transition(ticket, o, &out, &errout)
				} else {
					code = setTask(ticket, o, &out, &errout)
				}
				if code != tc.wantCode {
					t.Fatalf("code=%d, want %d; stderr=%q", code, tc.wantCode, errout.String())
				}
				if tc.wantCode != 0 {
					if _, err := os.Stat(oldPath); err != nil {
						t.Fatalf("refused promotion changed ticket name: %v", err)
					}
				}
			})
		}
	}
}

func TestStartUpdatesMetadataOnOwnTicket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "20261105-1000-MED-ticket.md")
	body := "# Ticket\n\nСтатус: в работе\nИсполнитель: actor\n\n## Приёмка\ncheck\n\n## Журнал\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	o := options{cmd: "start", prio: "HIGH", why: "urgent", by: "actor"}
	if code := transition(task{path: path, slug: "ticket", prio: "MED", status: "в работе", body: body}, o, &out, &errout); code != 0 {
		t.Fatalf("start own ticket = %d %q", code, errout.String())
	}
	updated, err := os.ReadFile(filepath.Join(dir, "20261105-1000-HIGH-ticket.md"))
	if err != nil || !strings.Contains(string(updated), "Причина: urgent") {
		t.Fatalf("own ticket metadata = %q, %v", updated, err)
	}
}

func TestMetadataWithoutTitleOrStatusRefusesRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "20261105-1000-MED-legacy.md")
	body := "## What\nwork\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	code := setTask(task{path: path, slug: "legacy", prio: "MED", body: body}, options{prio: "HIGH", why: "urgent", by: "actor"}, &out, &errout)
	if code != exitUsage || !strings.Contains(errout.String(), "cannot insert Причина") {
		t.Fatalf("set without insertion point = %d %q", code, errout.String())
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != body {
		t.Fatalf("original ticket changed: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "20261105-1000-HIGH-legacy.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected renamed ticket: %v", err)
	}
}

func TestSetClosedTicketAndNameConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "20200101-1000-MED-closed.md")
	body := "# Closed\n\nСтатус: готово\n\n## Журнал\n- 2020-01-01 10:00 готово — actor: checked\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := setTask(task{path: path, slug: "closed", prio: "MED", body: body}, options{diff: "ARCH", by: "actor"}, &out, &errout); code != 0 {
		t.Fatalf("set closed: %d %s", code, errout.String())
	}
	archived := filepath.Join(dir, "20200101-1000-MED-ARCH-closed.md")
	updated, err := os.ReadFile(archived)
	if err != nil || !strings.Contains(string(updated), "Статус: готово") {
		t.Fatalf("closed status changed: %s %v", updated, err)
	}
	conflict := filepath.Join(dir, "20200101-1000-HIGH-ARCH-closed.md")
	if err := os.WriteFile(conflict, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errout.Reset()
	code := setTask(task{path: archived, slug: "closed", prio: "MED", diff: "ARCH", body: string(updated)}, options{prio: "HIGH", why: "urgent", by: "actor"}, &out, &errout)
	if code != exitUsage || !strings.Contains(errout.String(), "already exists") {
		t.Fatalf("set conflict = %d %q", code, errout.String())
	}
	if got, _ := os.ReadFile(archived); string(got) != string(updated) {
		t.Fatalf("conflict changed source: %q", got)
	}
}

func TestRenameRefusesSymlinkAndFailedRenameLeavesJournalUntouched(t *testing.T) {
	for _, command := range []string{"set", "start"} {
		t.Run(command+"-symlink", func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "target.md")
			link := filepath.Join(dir, "20261105-1000-MED-linked.md")
			body := "# Linked\n\nСтатус: открыт\n\n## Приёмка\ncheck\n\n## Журнал\n"
			if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			var out, errout bytes.Buffer
			o := options{cmd: command, prio: "HIGH", why: "urgent", by: "actor"}
			var code int
			if command == "set" {
				code = setTask(task{path: link, slug: "linked", prio: "MED", body: body}, o, &out, &errout)
			} else {
				code = transition(task{path: link, slug: "linked", prio: "MED", status: "открыт", body: body}, o, &out, &errout)
			}
			if code != exitUsage || !strings.Contains(errout.String(), "symlink") {
				t.Fatalf("rename symlink = %d %q", code, errout.String())
			}
			got, _ := os.ReadFile(target)
			if string(got) != body {
				t.Fatalf("symlink target changed: %q", got)
			}
		})
		t.Run(command+"-rename-error", func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "20261105-1000-MED-ticket.md")
			body := "# Ticket\n\nСтатус: открыт\n\n## Приёмка\ncheck\n\n## Журнал\n"
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			oldRename := renameTaskFile
			renameTaskFile = func(string, string) error { return errors.New("injected rename failure") }
			t.Cleanup(func() { renameTaskFile = oldRename })
			var out, errout bytes.Buffer
			o := options{cmd: command, prio: "HIGH", why: "urgent", by: "actor"}
			var code int
			if command == "set" {
				code = setTask(task{path: path, slug: "ticket", prio: "MED", body: body}, o, &out, &errout)
			} else {
				code = transition(task{path: path, slug: "ticket", prio: "MED", status: "открыт", body: body}, o, &out, &errout)
			}
			got, err := os.ReadFile(path)
			if code != exitUsage || err != nil || string(got) != body || strings.Contains(string(got), "переименован") {
				t.Fatalf("rename failure = %d body=%q err=%v stderr=%q", code, got, err, errout.String())
			}
			if _, err := os.Stat(filepath.Join(dir, "20261105-1000-HIGH-ticket.md")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("new path remains after failed rename: %v", err)
			}
		})
	}
}

func TestClosedFieldIsOnlyAddedToNextJSON(t *testing.T) {
	ticket := task{path: filepath.Join(t.TempDir(), "20200101-1000-MED-ticket.md"), slug: "ticket", prio: "MED", status: "открыт", body: "# Ticket\nСтатус: открыт\n"}
	standard, err := json.Marshal(taskObject("", ticket, "", false))
	if err != nil || bytes.Contains(standard, []byte(`"closed"`)) {
		t.Fatalf("standard task JSON has closed: %s %v", standard, err)
	}
	var out, errout bytes.Buffer
	if code := printCombinedNext("", nextChoice{task: ticket, found: true, gate: queuegate.Scope{Valid: true}}, nil, true, &out, &errout); code != 0 {
		t.Fatalf("next JSON = %d %s", code, errout.String())
	}
	var next map[string]any
	if err := json.Unmarshal(out.Bytes(), &next); err != nil || next["closed"] != true {
		t.Fatalf("next JSON missing closed: %s %v", out.String(), err)
	}
}

func TestNextWaitDeadlinesExcludeFutureTickets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, deadline := range map[string]string{"past": "2000-01-01 10:00", "future": "2099-01-01 10:00"} {
		body := fmt.Sprintf("# %s\n\nСтатус: ждёт\nЖдёт: %s\n\n## Журнал\n", name, deadline)
		path := filepath.Join(dir, "20200101-1000-MED-"+name+".md")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tasks, err := loadTasks(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	candidates := nextCandidates(tasks, "actor", queuegate.Scope{Valid: true})
	if len(candidates) != 1 || candidates[0].slug != "past" {
		t.Fatalf("waiting candidates = %#v", candidates)
	}
}

func TestNextOrdersCandidatesAndAppliesGateStates(t *testing.T) {
	for _, tc := range []struct {
		name       string
		gate       string
		wantSlug   string
		wantClosed bool
		wantDiag   bool
	}{
		{"root gate", `{"at":%q,"live":["project"],"open":[],"open_by_root":{"%s":["MED-DO"]}}`, "med-do", false, false},
		{"project outside configured root", `{"at":%q,"live":["project"],"open":["MED-HUNT"],"open_by_root":{"%s":["MED-DO"]}}`, "med-hunt", false, false},
		{"gate missing", "", "high", false, true},
		{"gate malformed", "{", "high", false, true},
		{"gate stale", `{"at":"2000-01-01T00:00:00Z","open_by_root":{}}`, "high", false, true},
		{"gate future", `{"at":"2099-01-01T00:00:00Z","open_by_root":{}}`, "high", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			root := filepath.Join(home, "project")
			dir := filepath.Join(root, "docs", "tasks")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			write := func(name, status string) {
				t.Helper()
				body := "# " + name + "\n\nСтатус: " + status + "\n\n## Приёмка\n- check\n\n## Журнал\n"
				if status == "в работе" {
					body += "- 2026-11-01 10:00 в работе — me\n"
				}
				if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("20261101-1000-MED-DO-med-do", "открыт")
			write("20261030-1000-MED-HUNT-med-hunt", "открыт")
			if tc.wantDiag {
				write("20261101-1000-HIGH-high", "открыт")
			}
			if tc.gate != "" {
				gateDir := filepath.Join(home, ".local", "state", "queue")
				if err := os.MkdirAll(gateDir, 0o755); err != nil {
					t.Fatal(err)
				}
				gate := tc.gate
				if strings.Contains(gate, "%q") {
					gate = fmt.Sprintf(gate, time.Now().Format(time.RFC3339), func() string {
						if tc.name == "project outside configured root" {
							return filepath.Join(home, "other")
						}
						return root
					}())
				}
				if err := os.WriteFile(filepath.Join(gateDir, "gate.json"), []byte(gate), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var out, errout bytes.Buffer
			if code := nextTask(root, "", dir, options{by: "me", json: true}, &out, &errout); code != 0 {
				t.Fatalf("nextTask = %d, stderr %q", code, errout.String())
			}
			var got struct {
				Slug   string `json:"slug"`
				Closed bool   `json:"closed"`
			}
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Slug != tc.wantSlug || got.Closed != tc.wantClosed {
				t.Errorf("next JSON = slug %q closed %v, want %q %v", got.Slug, got.Closed, tc.wantSlug, tc.wantClosed)
			}
			if strings.Contains(errout.String(), "gate.json") != tc.wantDiag {
				t.Errorf("gate diagnostic = %q, want present %v", errout.String(), tc.wantDiag)
			}
		})
	}
}

// An old MEDIUM ticket gets the current MED spelling whenever tk writes its name.
func TestRenameWritesCurrentPrioSpelling(t *testing.T) {
	body := "# Old\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n- 2020-01-01 10:00 открыт — author\n"
	write := func(t *testing.T) (string, string) {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "20200101-1000-MEDIUM-old.md")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir, path
	}
	const want = "20200101-1000-MED-DO-old.md"
	t.Run("set", func(t *testing.T) {
		dir, path := write(t)
		var out, errout bytes.Buffer
		if code := setTask(task{path: path, slug: "old", prio: "MEDIUM", body: body}, options{diff: "DO", by: "actor"}, &out, &errout); code != 0 {
			t.Fatalf("setTask = %d, stderr %q", code, errout.String())
		}
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("set wrote %q, want %s", out.String(), want)
		}
	})
	t.Run("start", func(t *testing.T) {
		dir, path := write(t)
		got, err := transitionPath(&task{path: path, slug: "old", prio: "MEDIUM", body: body}, options{cmd: "start", diff: "DO", by: "actor"})
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Join(dir, want) {
			t.Errorf("start path %s, want %s", filepath.Base(got), want)
		}
	})
}

// Old names change only on an explicit --prio or --diff.
func TestOldNameStaysWithoutPrioOrDiff(t *testing.T) {
	body := "# Old\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n- 2020-01-01 10:00 открыт — author\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "20200101-1000-MEDIUM-old.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := setTask(task{path: path, slug: "old", prio: "MEDIUM", body: body}, options{why: "cause", by: "actor"}, &out, &errout); code != 0 {
		t.Fatalf("setTask = %d, stderr %q", code, errout.String())
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "Причина: cause") || strings.Contains(string(updated), "переименован") {
		t.Errorf("set --why changed the name or lost the cause: %s", updated)
	}
}

// An old word with no current form is not carried into a written name.
func TestRetagOldWordNeedsPrio(t *testing.T) {
	if name, err := retaggedName("20200101-1000-INFO-old.md", options{diff: "DO"}); err == nil {
		t.Errorf("--diff on INFO wrote %s, want an error", name)
	}
	name, err := retaggedName("20200101-1000-INFO-old.md", options{prio: "LOW", diff: "DO"})
	if err != nil || name != "20200101-1000-LOW-DO-old.md" {
		t.Errorf("--prio LOW --diff DO on INFO = %q, %v", name, err)
	}
}

func TestGateOpenTicketWithoutDifficultyNeedsAllPairs(t *testing.T) {
	for _, tc := range []struct {
		open []string
		want bool
	}{
		{[]string{"MED-DO"}, false},
		{[]string{"MED-DO", "MED-HUNT"}, false},
		{[]string{"MED-DO", "MED-HUNT", "MED-ARCH"}, true},
		{[]string{"MEDIUM-DO", "MEDIUM-HUNT", "MEDIUM-ARCH"}, true},
		{[]string{"LOW-DO", "LOW-HUNT", "LOW-ARCH"}, false},
	} {
		if got := (queuegate.Scope{Valid: true, Live: true, Open: tc.open}).Passes("MED", ""); got != tc.want {
			t.Errorf("Passes(MED) with open %v = %v, want %v", tc.open, got, tc.want)
		}
	}
	closed := queuegate.Scope{Valid: true, Live: true}
	if !closed.Passes("HIGH", "") || !closed.Passes("CRIT", "") {
		t.Error("CRIT and HIGH are always open")
	}
}

func queueProject(t *testing.T, gate string, tickets map[string]string) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "project")
	dir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, status := range tickets {
		body := "# " + name + "\n\nСтатус: " + status + "\n\n## Приёмка\n- check\n\n## Журнал\n- 2026-11-01 10:00 в работе — master\n"
		if status == "в работе" {
			body = strings.Replace(body, "\n\n## Приёмка", "\nИсполнитель: master\n\n## Приёмка", 1)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if gate != "" {
		gateDir := filepath.Join(home, ".local", "state", "queue")
		if err := os.MkdirAll(gateDir, 0o755); err != nil {
			t.Fatal(err)
		}
		content := strings.ReplaceAll(gate, "NOW", time.Now().Format(time.RFC3339))
		var data map[string]any
		if json.Unmarshal([]byte(content), &data) == nil {
			if _, ok := data["open_by_root"]; !ok {
				open, _ := data["open"].([]any)
				data["open_by_root"] = map[string]any{root: open}
				encoded, _ := json.Marshal(data)
				content = string(encoded)
			}
		}
		if err := os.WriteFile(filepath.Join(gateDir, "gate.json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, dir
}

// Without readable gates only what is always open is shown, with the reason.
func TestNextBrokenGateShowsOnlyAlwaysOpen(t *testing.T) {
	for name, gate := range map[string]string{
		"missing":   "",
		"malformed": "{",
		"stale":     `{"at":"2000-01-01T00:00:00Z","live":["project"],"open":["MED-DO"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			root, dir := queueProject(t, gate, map[string]string{"20261101-1000-MED-DO-med": "открыт"})
			var out, errout bytes.Buffer
			if code := nextTask(root, "", dir, options{by: "me"}, &out, &errout); code != exitNone {
				t.Errorf("only MED open: code %d, out %q", code, out.String())
			}
			if !strings.Contains(errout.String(), "gate.json") || !strings.Contains(errout.String(), "CRIT and HIGH") {
				t.Errorf("warning %q", errout.String())
			}
			if err := os.WriteFile(filepath.Join(dir, "20261101-1100-HIGH-high.md"), []byte("# high\n\nСтатус: открыт\nПричина: x\n\n## Приёмка\n- check\n\n## Журнал\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if code := nextTask(root, "", dir, options{by: "me"}, &out, &errout); code != 0 || !strings.Contains(out.String(), "high") {
				t.Errorf("HIGH open: code %d, out %q", code, out.String())
			}
		})
	}
}

// tk done ends with the next ticket chosen as tk next chooses it, for the master
// who owns the closed ticket, and prints nothing when the queue is empty.
func TestDoneHintsNextTicket(t *testing.T) {
	const live = `{"at":"NOW","live":["project"],"open":["MED-DO"]}`
	closing := "20261101-1000-HIGH-closing"
	done := func(t *testing.T, gate string, others map[string]string) string {
		t.Helper()
		tickets := map[string]string{closing: "в работе"}
		maps.Copy(tickets, others)
		root, dir := queueProject(t, gate, tickets)
		var out, errout bytes.Buffer
		o := options{cmd: "done", id: "closing", by: "checker", check: "checked by test", baseRoot: root}
		if code := runLockedCommand(root, dir, o, &out, &errout); code != 0 {
			t.Fatalf("done = %d, stderr %q", code, errout.String())
		}
		return out.String()
	}
	t.Run("empty queue prints only the status", func(t *testing.T) {
		if got := done(t, live, nil); got != "closing готово\n" {
			t.Errorf("output %q", got)
		}
	})
	t.Run("next open ticket", func(t *testing.T) {
		got := done(t, live, map[string]string{"20261101-1100-MED-DO-open-pair": "открыт", "20261101-0900-MED-HUNT-closed-pair": "открыт"})
		lines := strings.Split(strings.TrimSpace(got), "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[1], "следующий: open-pair ") {
			t.Errorf("output %q", got)
		}
	})
	t.Run("the owner's ticket in work comes first", func(t *testing.T) {
		got := done(t, live, map[string]string{"20261101-1100-HIGH-open": "открыт", "20261101-1200-MED-DO-owned": "в работе"})
		if !strings.Contains(got, "следующий: owned ") {
			t.Errorf("output %q", got)
		}
	})
	t.Run("not done prints no hint", func(t *testing.T) {
		root, dir := queueProject(t, live, map[string]string{"20261101-1100-HIGH-other": "открыт", "20261101-1000-HIGH-rejected": "открыт"})
		path := filepath.Join(dir, "20261101-1000-HIGH-rejected.md")
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var out, errout bytes.Buffer
		o := options{cmd: "reject", by: "me", why: "no", baseRoot: root}
		if code := transition(task{path: path, slug: "rejected", prio: "HIGH", status: "открыт", body: string(content)}, o, &out, &errout); code != 0 {
			t.Fatalf("reject = %d, stderr %q", code, errout.String())
		}
		if strings.Contains(out.String(), "следующий") {
			t.Errorf("output %q", out.String())
		}
	})
}

// Gates close the start of new work only: what an agent already began and a wait
// that came due stay visible under a closed pair and under unreadable gates.
func TestNextGateKeepsStartedWork(t *testing.T) {
	const closed = `{"at":"NOW","live":["project"],"open":[]}`
	for name, gate := range map[string]string{"closed pair": closed, "missing": "", "malformed": "{"} {
		t.Run(name, func(t *testing.T) {
			next := func(t *testing.T, status string) (string, int) {
				t.Helper()
				root, dir := queueProject(t, gate, map[string]string{"20261101-1000-MED-DO-subject": status})
				if status == "ждёт" {
					path := filepath.Join(dir, "20261101-1000-MED-DO-subject.md")
					content, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					body := strings.Replace(string(content), "\n\n## Приёмка", "\nЖдёт: 2020-01-01 10:00\n\n## Приёмка", 1)
					if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				var out, errout bytes.Buffer
				code := nextTask(root, "", dir, options{by: "master"}, &out, &errout)
				return out.String(), code
			}
			for _, status := range []string{"в работе", "ждёт"} {
				if out, code := next(t, status); code != 0 || !strings.Contains(out, "subject") {
					t.Errorf("%s: code %d, out %q", status, code, out)
				}
			}
			if out, code := next(t, "открыт"); code != exitNone || strings.Contains(out, "subject") {
				t.Errorf("открыт: code %d, out %q", code, out)
			}
		})
	}
}

// The hint of tk done follows the same rule.
func TestDoneHintKeepsStartedWorkUnderClosedGate(t *testing.T) {
	root, dir := queueProject(t, `{"at":"NOW","live":["project"],"open":[]}`, map[string]string{
		"20261101-1000-HIGH-closing":  "в работе",
		"20261101-1001-MED-DO-begun":  "в работе",
		"20261101-1002-MED-DO-unseen": "открыт",
	})
	var out, errout bytes.Buffer
	o := options{cmd: "done", id: "closing", by: "checker", check: "checked by test", baseRoot: root}
	if code := runLockedCommand(root, dir, o, &out, &errout); code != 0 {
		t.Fatalf("done: code %d, stderr %q", code, errout.String())
	}
	if !strings.Contains(out.String(), "следующий: begun") {
		t.Errorf("hint %q", out.String())
	}
}

// Under a closed pair started work follows CRIT and HIGH and goes ahead of new work;
// under an open pair own work in progress stays ahead of HIGH.
func TestNextStartedWorkOrderUnderGate(t *testing.T) {
	tickets := map[string]string{
		"20261101-1000-MED-DO-begun":  "в работе",
		"20261101-1001-HIGH-high":     "открыт",
		"20261101-1002-CRIT-crit":     "открыт",
		"20261101-1003-MED-HUNT-new":  "открыт",
		"20261101-1004-LOW-ARCH-late": "открыт",
	}
	order := func(t *testing.T, gate string) string {
		t.Helper()
		root, dir := queueProject(t, gate, tickets)
		var got []string
		for {
			choice, err := chooseNext(dir, root, "master")
			if err != nil {
				t.Fatal(err)
			}
			if !choice.found {
				return strings.Join(got, " ")
			}
			got = append(got, choice.task.slug)
			if err := os.Remove(choice.task.path); err != nil {
				t.Fatal(err)
			}
		}
	}
	const allMed = `"MED-DO","MED-HUNT","MED-ARCH"`
	for _, tc := range []struct{ name, gate, want string }{
		{"closed pair", `{"at":"NOW","live":["project"],"open":[]}`, "crit high begun"},
		{"unreadable", "{", "crit high begun"},
		{"missing", "", "crit high begun"},
		{"open pair", `{"at":"NOW","live":["project"],"open":[` + allMed + `]}`, "crit begun high new"},
		{"other pair open", `{"at":"NOW","live":["project"],"open":["MED-HUNT"]}`, "crit high begun new"},
		{"not live", `{"at":"NOW","live":[],"open":[]}`, "crit begun high new late"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := order(t, tc.gate); got != tc.want {
				t.Errorf("order %q, want %q", got, tc.want)
			}
		})
	}
}

// A wait that came due under a closed pair follows HIGH as well.
func TestNextDueWaitOrderUnderClosedGate(t *testing.T) {
	root, dir := queueProject(t, `{"at":"NOW","live":["project"],"open":[]}`, map[string]string{
		"20261101-1000-MED-DO-waited": "ждёт",
		"20261101-1001-HIGH-high":     "открыт",
	})
	path := filepath.Join(dir, "20261101-1000-MED-DO-waited.md")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(content), "\n\n## Приёмка", "\nЖдёт: 2020-01-01 10:00\n\n## Приёмка", 1)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	choice, err := chooseNext(dir, root, "master")
	if err != nil {
		t.Fatal(err)
	}
	if !choice.found || choice.task.slug != "high" {
		t.Errorf("first %q", choice.task.slug)
	}
}

// tk new prints the ticket path on stdout, and scripts read it, so the next step goes to stderr.
func TestNewAndStartNameNextStep(t *testing.T) {
	cases := []struct {
		name, text  string
		want, hasnt string
	}{
		{name: "empty ticket", want: "заполни «Что сделать» и «Приёмка»"},
		{name: "filled ticket", text: "## Что сделать\nwork\n\n## Приёмка\n- check\n", want: "дальше: tk start ", hasnt: "заполни"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "docs", "tasks")
			o := options{cmd: "new", id: "sample", prio: "MED", by: "me", text: tc.text}
			var out, errOut bytes.Buffer
			if code := create(root, dir, o, &out, &errOut); code != 0 {
				t.Fatalf("create = %d: %s", code, errOut.String())
			}
			if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 1 || !strings.HasSuffix(lines[0], "-sample.md") {
				t.Fatalf("stdout must stay the ticket path: %q", out.String())
			}
			if !strings.Contains(errOut.String(), tc.want) || tc.hasnt != "" && strings.Contains(errOut.String(), tc.hasnt) {
				t.Fatalf("hint = %q, want %q", errOut.String(), tc.want)
			}
			tasks, err := load(dir)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("load = %v, %v", tasks, err)
			}
			out.Reset()
			errOut.Reset()
			code := transition(tasks[0], options{cmd: "start", id: "sample", by: "me"}, &out, &errOut)
			if tc.name == "empty ticket" {
				if code != exitNone || !strings.Contains(errOut.String(), "Приёмка") || !strings.Contains(errOut.String(), "tk set sample --text") {
					t.Fatalf("empty start = %d: %s", code, errOut.String())
				}
				return
			}
			if code != 0 {
				t.Fatalf("start = %d: %s", code, errOut.String())
			}
			if strings.TrimSpace(out.String()) != "sample в работе" {
				t.Fatalf("start stdout changed: %q", out.String())
			}
			if !strings.Contains(errOut.String(), "tk done sample --check") || !strings.Contains(errOut.String(), "tk note sample") {
				t.Fatalf("start hint = %q", errOut.String())
			}
		})
	}
}

func TestNewToAndJournalHints(t *testing.T) {
	filled := "# T\n\n## Приёмка\n- check\n\n## Журнал\n"
	if got := hintAfterNew(options{id: "x", to: "elsewhere"}, "/p/x.md", filled); strings.Contains(got, "tk start") {
		t.Fatalf("a ticket for another project must not suggest starting it: %q", got)
	}
	root := t.TempDir()
	var out, errOut bytes.Buffer
	if code := create(root, filepath.Join(root, "docs", "tasks"), options{cmd: "new", id: "log", prio: "MED", by: "me", journal: true}, &out, &errOut); code != 0 {
		t.Fatalf("create = %d: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "tk note log") || strings.Contains(errOut.String(), "tk start") {
		t.Fatalf("a journal ticket takes notes, it is not started: %q", errOut.String())
	}
}

func TestFillHintNamesOnlyEmptySections(t *testing.T) {
	work := "# T\n\n## Что сделать\nwork\n\n## Приёмка\n\n## Журнал\n"
	if got := fillHint(work, "p"); strings.Contains(got, "Что сделать") || !strings.Contains(got, "«Приёмка»") {
		t.Fatalf("hint for empty acceptance = %q", got)
	}
	check := "# T\n\n## Что сделать\n\n## Приёмка\n- check\n\n## Журнал\n"
	if got := fillHint(check, "p"); !strings.Contains(got, "«Что сделать»") || strings.Contains(got, "Приёмка") {
		t.Fatalf("hint for empty work = %q", got)
	}
	if got := fillHint("# T\n\n## Что сделать\nwork\n\n## Приёмка\n- check\n\n## Журнал\n", "p"); got != "" {
		t.Fatalf("hint for a filled ticket = %q", got)
	}
}

func TestRepeatedStartNamesNextStep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "20261104-1000-MED-DO-again.md")
	body := "# T\n\nСтатус: в работе\nИсполнитель: me\n\n## Что сделать\nwork\n\n## Приёмка\n- check\n\n## Журнал\n"
	var out, errOut bytes.Buffer
	code, done := guardStart(task{path: path, slug: "again", status: "в работе", body: body}, options{cmd: "start", by: "me"}, &out, &errOut)
	if code != 0 || !done || strings.TrimSpace(out.String()) != "again в работе" || !strings.Contains(errOut.String(), "tk done again") {
		t.Fatalf("repeat start = %d %v %q %q", code, done, out.String(), errOut.String())
	}
}

// due in tk ls --json is the deadline waitDue parses, so a reader does not parse
// the wait text again; a wait without a parsed deadline leaves it empty.
func TestListJSONDueMatchesWaitDue(t *testing.T) {
	for _, wait := range []string{"2020-01-01T12:00", "2020-01-01 12:00 итог", "31.12 23:59: итог", "10.11 12:00: ответ", "2999-01-01 12:00 CET (x)", "2020-13-45 12:00: срок", "2020-01-01-ответ", "после ответа"} {
		waiting := task{status: "ждёт", body: "Статус: ждёт\nЖдёт: " + wait}
		want := ""
		if at, ok := timearg.Deadline(wait, time.Now()); ok {
			want = at.Format(time.RFC3339)
		}
		obj := taskObject("", waiting, "", false)
		if obj.Due != want {
			t.Errorf("due for %q = %q, want %q", wait, obj.Due, want)
		}
		encoded, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"due"`) != (want != "") {
			t.Errorf("due field presence for %q: %s", wait, encoded)
		}
	}
	if got := taskObject("", task{status: "ждёт", body: "Статус: ждёт\nЖдёт: 31.12 23:59: итог"}, "", false).Due; got == "" {
		t.Error("a day-month deadline should be parsed")
	}
	if taskObject("", task{status: "ждёт", body: "Статус: ждёт\nЖдёт: 2999-01-01 12:00 итог"}, "", false).Overdue {
		t.Error("a deadline ahead is not overdue")
	}
	if got := taskObject("", task{status: "открыт", body: "Статус: открыт\nЖдёт: 2020-01-01 12:00"}, "", false).Due; got != "" {
		t.Errorf("due on a ticket that does not wait = %q", got)
	}
}

// Header fields live above the first section: a body line of the same form is text.
func TestHeaderEditsLeaveBodyLinesAlone(t *testing.T) {
	const body = "# Title\n\nСтатус: в работе\nИсполнитель: gone\nЗаказчик: old\nЖдёт: 2020-01-01\n\n## Что сделать\nИсполнитель: строка тела\nЗаказчик: строка тела\nЖдёт: строка тела\n```\n# comment\n```\n"
	if got := removeExecutor(body); strings.Contains(got, "Исполнитель: gone") || !strings.Contains(got, "Исполнитель: строка тела") {
		t.Errorf("removeExecutor:\n%s", got)
	}
	if got := removeWaiting(body); strings.Contains(got, "Ждёт: 2020-01-01") || !strings.Contains(got, "Ждёт: строка тела") {
		t.Errorf("removeWaiting:\n%s", got)
	}
	got, err := setRequester(body, "fresh")
	if err != nil || !strings.Contains(got, "Заказчик: fresh\nЖдёт") || !strings.Contains(got, "Заказчик: строка тела") || strings.Contains(got, "Заказчик: old") {
		t.Errorf("setRequester, header has it (%v):\n%s", err, got)
	}
	noHeader := strings.Replace(body, "Заказчик: old\n", "", 1)
	got, err = setRequester(noHeader, "fresh")
	if err != nil || !strings.Contains(got, "Статус: в работе\nЗаказчик: fresh\n") || !strings.Contains(got, "Заказчик: строка тела") {
		t.Errorf("setRequester, header lacks it (%v):\n%s", err, got)
	}
	dup := strings.Replace(body, "Заказчик: old\n", "Заказчик: old\nЗаказчик: older\n", 1)
	got, err = setRequester(dup, "fresh")
	if err != nil || strings.Count(got, "Заказчик: fresh") != 1 || strings.Contains(got, "older") || !strings.Contains(got, "Заказчик: строка тела") {
		t.Errorf("setRequester, duplicate in header (%v):\n%s", err, got)
	}
	got, err = setTicketTitle(body, "New")
	if err != nil || !strings.HasPrefix(got, "# New\n") || !strings.Contains(got, "# comment") {
		t.Errorf("setTicketTitle (%v):\n%s", err, got)
	}
	untitled := "Статус: открыт\n\n## Что сделать\n```\n# comment\n```\n"
	if got, err = setTicketTitle(untitled, "New"); err == nil {
		t.Errorf("a comment in the body taken for the title:\n%s", got)
	}
}

// An executor or a requester written below the first section is the ticket's text.
func TestHeaderFieldsReadAndWrittenInHeaderOnly(t *testing.T) {
	const body = "# Title\n\nСтатус: в работе\n\n## Что сделать\nИсполнитель: text-owner\nЗаказчик: text-requester\n"
	if got := executor(body); got != "" {
		t.Errorf("executor read from the body: %q", got)
	}
	if got := headerValue(body, "Заказчик: "); got != "" {
		t.Errorf("requester read from the body: %q", got)
	}
	got := setExecutor(body, "fresh")
	if !strings.Contains(got, "Статус: в работе\nИсполнитель: fresh\n") || !strings.Contains(got, "Исполнитель: text-owner") {
		t.Errorf("setExecutor:\n%s", got)
	}
	if owner := executor(got); owner != "fresh" {
		t.Errorf("executor after setExecutor = %q", owner)
	}
	if owner := executor(removeExecutor(got)); owner != "" {
		t.Errorf("executor after release = %q", owner)
	}
	const header = "# Title\n\nСтатус: в работе\nИсполнитель: real\nЗаказчик: boss\n\n## Что сделать\nИсполнитель: text-owner\n"
	if got := executor(header); got != "real" {
		t.Errorf("executor = %q", got)
	}
	if got := headerValue(header, "Заказчик: "); got != "boss" {
		t.Errorf("requester = %q", got)
	}
	if got := setExecutor(header, "fresh"); !strings.Contains(got, "Исполнитель: fresh\nЗаказчик: boss") || !strings.Contains(got, "Исполнитель: text-owner") {
		t.Errorf("setExecutor over the header line:\n%s", got)
	}
}

func TestNextUnderCritOnlySaysWhy(t *testing.T) {
	root, dir := queueProject(t, `{"at":"NOW","live":["project"],"open":[]}`, map[string]string{"20261101-0900-MED-DO-med": "открыт"})
	var out, errout bytes.Buffer
	if code := nextTask(root, "", dir, options{by: "me"}, &out, &errout); code != exitNone || out.String() != "работы нет\n" {
		t.Fatalf("closed bar without crit_only: code %d, out %q", code, out.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "20261101-1000-HIGH-high.md"), []byte("# high\n\nСтатус: открыт\nПричина: x\n\n## Приёмка\n- check\n\n## Журнал\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	home := filepath.Dir(root)
	gate := fmt.Sprintf(`{"at":%q,"live":["project"],"open_by_root":{%q:["MED-DO"]},"crit_only":{%q:"day off"}}`, time.Now().Format(time.RFC3339), home, home)
	if err := os.WriteFile(filepath.Join(home, ".local", "state", "queue", "gate.json"), []byte(gate), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := nextTask(root, "", dir, options{by: "me"}, &out, &errout); code != exitNone {
		t.Fatalf("HIGH under crit_only: code %d, out %q, err %q", code, out.String(), errout.String())
	}
	if want := "работы нет: " + home + ": day off — новая работа только CRIT\n"; out.String() != want {
		t.Errorf("out %q, want %q", out.String(), want)
	}
	if err := os.WriteFile(filepath.Join(dir, "20261101-1100-CRIT-crit.md"), []byte("# crit\n\nСтатус: открыт\nПричина: x\n\n## Приёмка\n- check\n\n## Журнал\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := nextTask(root, "", dir, options{by: "me"}, &out, &errout); code != 0 || !strings.Contains(out.String(), "crit") {
		t.Errorf("CRIT under crit_only: code %d, out %q", code, out.String())
	}
}

func TestNoteArgumentCounts(t *testing.T) {
	for _, args := range [][]string{
		{"note", "--text", "-", "--by", "tester"},
		{"note", "task", "a", "b", "--by", "tester"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%q) accepted wrong argument count", args)
		}
	}
}

func TestNoteTextFlagRejectsConflictingOrEmptyInput(t *testing.T) {
	if _, err := parseArgs([]string{"note", "task", "legacy", "--text", "-", "--by", "tester"}); err == nil || !strings.Contains(err.Error(), "either TEXT or --text FILE|-") {
		t.Fatalf("mixed note inputs error = %v", err)
	}
	input, err := os.CreateTemp("", "tk-note-empty-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(input.Name())
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	var errout bytes.Buffer
	if code := readTicketText(&options{cmd: "note", text: input.Name()}, &errout); code != exitUsage || !strings.Contains(errout.String(), "note text must not be empty") {
		t.Fatalf("empty note input: code=%d stderr=%q", code, errout.String())
	}
	if err := os.WriteFile(input.Name(), []byte("  \n\t \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	errout.Reset()
	if code := readTicketText(&options{cmd: "note", text: input.Name()}, &errout); code != exitUsage || !strings.Contains(errout.String(), "note text must not be empty") {
		t.Fatalf("whitespace-only note input: code=%d stderr=%q", code, errout.String())
	}
}

func TestNoteTextRejectsForbiddenLineCharsAndNormalizesCRLF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "note.txt")
	for _, value := range []string{"one\nbad\rtwo", "one\nbad\u2028two", "one\nbad\u0001two"} {
		if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
		var errout bytes.Buffer
		o := options{cmd: "note", text: path}
		if code := readTicketText(&o, &errout); code != exitUsage || !strings.Contains(errout.String(), "note text line 2") {
			t.Errorf("input %q: code=%d stderr=%q", value, code, errout.String())
		}
	}
	if err := os.WriteFile(path, []byte("first\nbad\xff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var errout bytes.Buffer
	o := options{cmd: "note", text: path}
	if code := readTicketText(&o, &errout); code != exitUsage || !strings.Contains(errout.String(), "note text line 2") {
		t.Fatalf("invalid UTF-8 note: code=%d stderr=%q", code, errout.String())
	}
	if err := os.WriteFile(path, []byte("one\r\ntwo\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o = options{cmd: "note", text: path}
	if code := readTicketText(&o, &bytes.Buffer{}); code != 0 || o.note != "one\ntwo" {
		t.Fatalf("CRLF note = %q, code=%d", o.note, code)
	}
}

func TestNoteTextTrimsLeadingAndTrailingNewlines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(path, []byte("\n\nfoo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := options{cmd: "note", text: path}
	var errout bytes.Buffer
	if code := readTicketText(&o, &errout); code != 0 || o.note != "foo" {
		t.Fatalf("read note: code=%d note=%q stderr=%q", code, o.note, errout.String())
	}
	if err := os.WriteFile(path, []byte("  \nfoo"), 0o644); err != nil {
		t.Fatal(err)
	}
	o = options{cmd: "note", text: path}
	if code := readTicketText(&o, &errout); code != 0 || o.note != "foo" {
		t.Fatalf("leading blank line with spaces: code=%d note=%q stderr=%q", code, o.note, errout.String())
	}
	if err := os.WriteFile(path, []byte("foo  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o = options{cmd: "note", text: path}
	if code := readTicketText(&o, &errout); code != 0 || o.note != "foo  " {
		t.Fatalf("trailing spaces lost: code=%d note=%q stderr=%q", code, o.note, errout.String())
	}
}

func TestNoteTextTrimsTrailingNewlinesBeforeTimeMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(path, []byte("было в 14:3x\nвторая\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := options{cmd: "note", text: path}
	var errout bytes.Buffer
	if code := readTicketText(&o, &errout); code != 0 {
		t.Fatalf("read note: code=%d stderr=%q", code, errout.String())
	}
	line := journalNoteLine("2026-11-13 12:00", "tester", markedJournalTextFor(o, "note", o.note))
	lines := strings.Split(line, "\n")
	if got := lines[len(lines)-1]; got != "      вторая [время требует проверки]" {
		t.Fatalf("marked final continuation = %q; journal line: %q", got, line)
	}
}

func TestJournalNoteLineUsesSixSpaceContinuation(t *testing.T) {
	input := "first\n## X\n---\n===\n- 2026-11-13 12:00 done — x: y\n1. a\n* a\n```\n~~~\n> цитата\n<div>\n\tтаб\n\nlast"
	got := journalNoteLine("2026-11-13 12:00", "tester", input)
	lines := strings.Split(got, "\n")
	want := []string{
		"- 2026-11-13 12:00 note — tester: first",
		"      ## X",
		"      ---",
		"      ===",
		"      - 2026-11-13 12:00 done — x: y",
		"      1. a",
		"      * a",
		"      ```",
		"      ~~~",
		"      > цитата",
		"      <div>",
		"      \tтаб",
		"      ",
		"      last",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("journal note lines = %#v, want %#v", lines, want)
	}
	var restored []string
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, "      ") {
			t.Fatalf("continuation lacks six-space indent: %q", line)
		}
		restored = append(restored, strings.TrimPrefix(line, "      "))
	}
	if got := strings.Join(append([]string{"first"}, restored...), "\n"); got != input {
		t.Fatalf("restored text = %q, want %q", got, input)
	}
}

func TestNoteReadsMultilineTextFromStdin(t *testing.T) {
	root := t.TempDir()
	taskDir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(taskDir, "20260101-1200-LOW-task.md")
	body := "# Task\n\nСтатус: открыт\n\n## Журнал\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	input, err := os.CreateTemp("", "tk-note-stdin-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(input.Name())
	if _, err := input.WriteString("first line\n## Приёмка\n- 2026-11-13 12:00 note — forged: second line\nthird line\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = oldStdin; _ = input.Close() })
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	var out, errout bytes.Buffer
	code := runMain([]string{"note", "task", "--text", "-", "--by", "tester"}, time.Now(), &out, &errout)
	updated, readErr := os.ReadFile(path)
	if code != 0 || readErr != nil {
		t.Fatalf("note: code=%d stderr=%q err=%v", code, errout.String(), readErr)
	}
	entries := journalRecords(parseTicketBody(string(updated)))
	if len(entries) != 1 || len(entries[0]) != 4 || !strings.HasPrefix(entries[0][0], "- ") || entries[0][1] != "      ## Приёмка" || entries[0][2] != "      - 2026-11-13 12:00 note — forged: second line" || entries[0][3] != "      third line" {
		t.Fatalf("journal records = %#v; body:\n%s", entries, updated)
	}
}
