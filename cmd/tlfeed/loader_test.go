package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcript"
)

const (
	testProject = "-w-p"
	testSession = "11111111-2222-4333-8444-555555555555"
)

// sample — a synthetic transcript of the transcript package's tests.
func sample(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "transcript", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func claudePath(home string) string {
	return filepath.Join(home, ".claude", "projects", testProject, testSession+".jsonl")
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
}

type harness struct {
	t    *testing.T
	home string
	fake *fakeCH
	srv  *server
	log  bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, home: t.TempDir()}
	h.fake, h.srv = newFake(t)
	return h
}

// loader — a new loader over the same home and server, as after a restart; started, first pass done.
func (h *harness) loader(stop chan struct{}) *loader {
	h.t.Helper()
	cfg := config{home: h.home, db: "tq", tick: time.Second, idle: time.Hour, sweep: time.Second, rescan: time.Hour, batch: 32 << 20}
	if stop == nil {
		stop = make(chan struct{})
	}
	l := newLoader(cfg, h.srv, stop, &h.log, newStatus(""))
	l.dirs.w = noWatch{}
	l.backoff = time.Millisecond
	paths, err := l.start()
	if err != nil {
		h.t.Fatal(err)
	}
	if err := l.visitAll(paths); err != nil {
		h.t.Fatal(err)
	}
	return l
}

func (h *harness) pass(l *loader) {
	h.t.Helper()
	if err := l.pass(false); err != nil {
		h.t.Fatal(err)
	}
}

// readEvents — events of the first n lines of a file as transcript.Read gives them, in the fake's form.
func readEvents(t *testing.T, path string, lines int) []string {
	t.Helper()
	var out []string
	if _, err := transcript.Read(path, nil, func(e transcript.Event) {
		if e.Line <= lines {
			out = append(out, fmt.Sprintf("%d %s %s: %s", e.Line, e.Role, e.Who, e.Shown()))
		}
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// same: raw rebuilds the complete lines of the file, no line twice, events equal transcript.Read.
func (h *harness) same(l *loader, path string) *file {
	h.t.Helper()
	f := l.files[path]
	if f == nil {
		h.t.Fatalf("%s not followed", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatal(err)
	}
	complete := data[:bytes.LastIndexByte(data, '\n')+1]
	got, dup := h.fake.rebuilt(f.id)
	if got != string(complete) || dup {
		h.t.Fatalf("raw rebuilds %d bytes (duplicates %t), file has %d complete", len(got), dup, len(complete))
	}
	lines := bytes.Count(complete, []byte{'\n'})
	want := readEvents(h.t, path, lines)
	if ev := h.fake.events(f.id); strings.Join(ev, "\n") != strings.Join(want, "\n") {
		h.t.Fatalf("events:\n%s\ntranscript.Read:\n%s", strings.Join(ev, "\n"), strings.Join(want, "\n"))
	}
	if f.lines != lines || f.ingested != int64(len(complete)) {
		h.t.Fatalf("state lines %d ingested %d, file %d lines %d bytes", f.lines, f.ingested, lines, len(complete))
	}
	return f
}

// A file that grows in the middle of a line: only complete lines go, the rest waits; each insert carries the
// token file_id:first line, and the rows equal the file and transcript.Read.
func TestGrowsMidLine(t *testing.T) {
	h := newHarness(t)
	data := sample(t, "claude.jsonl")
	path := claudePath(h.home)
	ends := bytes.SplitAfter(data, []byte{'\n'})
	cut := len(ends[0]) + len(ends[1]) + len(ends[2]) + 40 // 40 bytes into line 4
	write(t, path, data[:cut])
	l := h.loader(nil)
	f := h.same(l, path)
	raws := h.fake.sentTo("raw")
	if len(raws) != 1 || raws[0].token != fmt.Sprintf("%d:1", f.id) || len(raws[0].rows) != 3 {
		t.Fatalf("first insert: %+v", raws)
	}
	if raws[0].settings["async_insert"] != "0" {
		t.Fatalf("insert settings %v", raws[0].settings)
	}
	h.pass(l) // nothing new: nothing sent
	if n := len(h.fake.sentTo("raw")); n != 1 {
		t.Fatalf("idle pass sent %d raw inserts", n)
	}
	appendTo(t, path, data[cut:])
	h.pass(l)
	h.same(l, path)
	raws = h.fake.sentTo("raw")
	if len(raws) != 2 || raws[1].token != fmt.Sprintf("%d:4", f.id) {
		t.Fatalf("second insert token %q, want %d:4", raws[len(raws)-1].token, f.id)
	}
	// lines 1 and 2 have no events: the first events insert starts at line 3
	if evs := h.fake.sentTo("events"); len(evs) != 2 || evs[0].token != fmt.Sprintf("%d:3", f.id) || evs[1].token != fmt.Sprintf("%d:4", f.id) {
		t.Fatalf("events inserts %d, first token %q", len(evs), evs[0].token)
	}
	files := h.fake.latestFiles()
	if len(files) != 1 || files[0][1] != path || files[0][2] != "claude" || files[0][3] != testProject || files[0][4] != "11111111" {
		t.Fatalf("files row %v", files)
	}
	if f.id>>22 != uint64(time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC).Unix()) {
		t.Fatalf("file id %d: high bits are not the first event's second", f.id)
	}
}

// A file cut back and grown past its old size: the loaded bytes changed, so its rows are deleted and it is
// loaded again under the same id, with tokens of the new generation; the new files row comes before the delete.
func TestRewind(t *testing.T) {
	h := newHarness(t)
	data := sample(t, "claude.jsonl")
	path := claudePath(h.home)
	write(t, path, data)
	l := h.loader(nil)
	id := h.same(l, path).id
	lines := bytes.SplitAfter(data, []byte{'\n'})
	other := bytes.Join(lines[:4], nil)
	for len(other) <= len(data) {
		other = append(other, lines[3]...)
	}
	write(t, path, other)
	h.pass(l)
	f := h.same(l, path)
	if f.id != id || f.gen != 1 {
		t.Fatalf("id %d gen %d after rewind, want id %d gen 1", f.id, f.gen, id)
	}
	if len(h.fake.deletes) != 3 {
		t.Fatalf("deletes %v", h.fake.deletes)
	}
	raws := h.fake.sentTo("raw")
	if last := raws[len(raws)-1]; last.token != fmt.Sprintf("%d:1:g1", id) {
		t.Fatalf("reload token %q", last.token)
	}
	if !strings.Contains(h.log.String(), "rewind") {
		t.Fatalf("log %q", h.log.String())
	}
	// shorter than loaded: the same
	write(t, path, bytes.Join(lines[:3], nil))
	h.pass(l)
	if f := h.same(l, path); f.gen != 2 {
		t.Fatalf("gen %d after the second rewind", f.gen)
	}
}

// A renamed file that grows keeps its id: the files row gets the new path, no line is loaded twice.
func TestRename(t *testing.T) {
	h := newHarness(t)
	data := sample(t, "claude.jsonl")
	path := claudePath(h.home)
	write(t, path, data)
	l := h.loader(nil)
	id := h.same(l, path).id
	moved := filepath.Join(h.home, ".claude", "projects", testProject, "99999999-2222-4333-8444-555555555555.jsonl")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	appendTo(t, moved, bytes.SplitAfter(data, []byte{'\n'})[2])
	h.pass(l)
	f := h.same(l, moved)
	if f.id != id || l.files[path] != nil {
		t.Fatalf("id %d (was %d), old path still followed: %t", f.id, id, l.files[path] != nil)
	}
	files := h.fake.latestFiles()
	if len(files) != 1 || files[0][1] != moved || files[0][4] != "99999999" {
		t.Fatalf("files rows %v", files)
	}
	if len(h.fake.deletes) != 0 {
		t.Fatalf("rename deleted rows: %v", h.fake.deletes)
	}
}

// The server applied an insert but its answer was lost, then it was down: the loader sends the same rows with
// the same token until it gets an answer, and the rows are not doubled; the error is in the state meanwhile.
func TestRetryKeepsToken(t *testing.T) {
	h := newHarness(t)
	data := sample(t, "codex.jsonl")
	path := filepath.Join(h.home, ".local", "state", "worker", "job1", "codex", "sessions", "2026", "10", "25",
		"rollout-2026-10-25T10-47-55-01a172ac-0000-7000-8000-000000000001.jsonl")
	write(t, path, data)
	h.fake.set(func(f *fakeCH) { f.lose["raw"], f.fail["events"] = 1, 2 })
	l := h.loader(nil)
	f := h.same(l, path)
	raws := h.fake.sentTo("raw")
	if len(raws) != 2 || raws[0].token != raws[1].token || raws[0].token != fmt.Sprintf("%d:1", f.id) {
		t.Fatalf("raw inserts %+v", raws)
	}
	if f.kind != "codex" || f.project != "worker:job1" {
		t.Fatalf("kind %q project %q", f.kind, f.project)
	}
	if !strings.Contains(h.log.String(), "insert events") || !strings.Contains(h.log.String(), "ok again") {
		t.Fatalf("log %q", h.log.String())
	}
	if l.errText != "" {
		t.Fatalf("error kept after success: %q", l.errText)
	}
}

// After a restart the loader reads its state from the files table: only lines past it are inserted, and the
// parser, rebuilt from the start of the file, gives the events of the new lines as transcript.Read does.
func TestRestart(t *testing.T) {
	h := newHarness(t)
	data := sample(t, "claude.jsonl")
	path := claudePath(h.home)
	half := bytes.Index(data, []byte(`"parentUuid":"d"`)) - 1
	write(t, path, data[:half])
	h.loader(nil)
	appendTo(t, path, data[half:])
	l := h.loader(nil)
	f := h.same(l, path)
	raws := h.fake.sentTo("raw")
	first := bytes.Count(data[:half], []byte{'\n'}) + 1
	if len(raws) != 2 || raws[1].token != fmt.Sprintf("%d:%d", f.id, first) {
		t.Fatalf("after restart raw inserts %d, last token %q", len(raws), raws[len(raws)-1].token)
	}
	// a third start with nothing new sends nothing
	h.loader(nil)
	if n := len(h.fake.sentTo("raw")); n != 2 {
		t.Fatalf("restart without new lines sent %d raw inserts", n)
	}
}

// A loader stopped between the raw and files inserts: the next one takes the lines already in raw, sends their
// events only if they are missing, and loses nothing.
func TestRestartAfterCrashBetweenInserts(t *testing.T) {
	for _, failing := range []string{"events", "files"} {
		t.Run(failing, func(t *testing.T) {
			h := newHarness(t)
			data := sample(t, "claude.jsonl")
			path := claudePath(h.home)
			half := bytes.Index(data, []byte(`"parentUuid":"d"`)) - 1
			write(t, path, data[:half])
			stop := make(chan struct{})
			l := h.loader(stop)
			appendTo(t, path, data[half:])
			h.fake.set(func(f *fakeCH) { f.fail[failing] = 1 << 30 })
			go func() {
				for len(h.fake.sentTo("raw")) < 2 {
					time.Sleep(10 * time.Millisecond)
				}
				close(stop)
			}()
			if err := l.pass(false); err != errStopped {
				t.Fatalf("pass: %v, want errStopped", err)
			}
			h.fake.set(func(f *fakeCH) { f.fail[failing] = 0 })
			l2 := h.loader(nil)
			h.same(l2, path)
			if !strings.Contains(h.log.String(), "lines in raw past files, taken") {
				t.Fatalf("log %q", h.log.String())
			}
		})
	}
}

// Many files in one pass go in one insert per table; the token lists every file, and a long list is hashed.
func TestTokenOfManyFiles(t *testing.T) {
	h := newHarness(t)
	data := sample(t, "claude.jsonl")
	for i := range 40 {
		write(t, filepath.Join(h.home, ".claude", "projects", testProject, fmt.Sprintf("%08d-2222-4333-8444-555555555555.jsonl", i)), data)
	}
	h.loader(nil)
	raws := h.fake.sentTo("raw")
	if len(raws) != 1 || !strings.HasPrefix(raws[0].token, "sha256:") {
		t.Fatalf("raw inserts %d, token %q", len(raws), raws[0].token)
	}
	if got := token([]string{"1:1", "2:5:g1"}); got != "1:1,2:5:g1" {
		t.Fatalf("token %q", got)
	}
}

// A subagent transcript that appears next to an active session is found by polling, without inotify.
func TestNewSubagentFoundByPolling(t *testing.T) {
	h := newHarness(t)
	data := sample(t, "claude.jsonl")
	path := claudePath(h.home)
	write(t, path, data)
	l := h.loader(nil)
	appendTo(t, path, bytes.SplitAfter(data, []byte{'\n'})[2]) // the session is active: its parser is held
	h.pass(l)
	sub := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "agent-a1b2c3d4e5.jsonl")
	write(t, sub, data)
	h.pass(l)
	f := h.same(l, sub)
	if f.kind != "sub" || f.src.ParentID != "11111111" || f.src.ID != "a1b2c3d4" {
		t.Fatalf("subagent %+v", f.src)
	}
	newProject := filepath.Join(h.home, ".claude", "projects", "-other", "22222222-2222-4333-8444-555555555555.jsonl")
	write(t, newProject, data)
	h.pass(l)
	h.same(l, newProject)
}

// A parser is released after idle and rebuilt from the file start when it grows again.
func TestParserReleased(t *testing.T) {
	h := newHarness(t)
	data := sample(t, "claude.jsonl")
	path := claudePath(h.home)
	half := bytes.Index(data, []byte(`"parentUuid":"d"`)) - 1
	write(t, path, data[:half])
	l := h.loader(nil)
	l.cfg.idle = time.Nanosecond
	h.pass(l)
	if l.files[path].follow != nil {
		t.Fatal("parser kept after idle")
	}
	appendTo(t, path, data[half:])
	h.pass(l)
	h.same(l, path)
}

// The state file: every field, rewritten atomically (no temporary file left), heartbeat moving while idle, and
// last_row_at kept from the previous run.
func TestStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := newStatus(path)
	t0 := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	s.rowsLoaded(t0)
	s.set(123, 4)
	s.setError("insert raw: down")
	if err := s.write(t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"heartbeat": "2026-11-22T12:00:01.000Z", "last_row_at": "2026-11-22T12:00:00.000Z", "lag_bytes": 123.0,
		"files": 4.0, "error": "insert raw: down", "pid": float64(os.Getpid())}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("state %v, want %v", got, want)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	beats, third := 0, make(chan struct{})
	now := func() time.Time {
		if beats++; beats == 3 {
			close(third)
		}
		return t0.Add(time.Duration(beats) * time.Minute)
	}
	go s.beat(time.Millisecond, stop, done, now, func(string, ...any) {})
	<-third // two ticks without any load
	close(stop)
	<-done
	raw, _ = os.ReadFile(path)
	if err := json.Unmarshal(raw, &got); err != nil || got["heartbeat"] == "2026-11-22T12:00:01.000Z" {
		t.Fatalf("heartbeat did not move while idle: %s", raw)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("files next to the state: %v", entries)
	}
	if again := newStatus(path); !again.lastRow.Equal(t0) {
		t.Fatalf("last_row_at not kept over a restart: %v", again.lastRow)
	}
}

// The state of a follow run: lag is zero once the server confirmed every complete line, and the error shows
// while the server is down.
func TestStateOfLoader(t *testing.T) {
	h := newHarness(t)
	data := sample(t, "claude.jsonl")
	path := claudePath(h.home)
	write(t, path, data[:len(data)-30])
	l := h.loader(nil)
	l.report()
	if lag, n := l.lag(); lag != 0 || n != 1 {
		t.Fatalf("lag %d files %d after a pass", lag, n)
	}
	stop := make(chan struct{})
	l.stop = stop
	h.fake.set(func(f *fakeCH) { f.down = true })
	appendTo(t, path, data[len(data)-30:])
	go func() {
		for l.st.snapshotErr() == "" {
			time.Sleep(5 * time.Millisecond)
		}
		close(stop)
	}()
	if err := l.pass(false); err != errStopped {
		t.Fatalf("pass %v", err)
	}
	want := int64(len(data) - bytes.LastIndexByte(data[:len(data)-30], '\n') - 1) // the last line, now complete
	if lag, _ := l.lag(); lag != want {
		t.Fatalf("lag %d while the server is down, want %d", lag, want)
	}
	if e := l.st.snapshotErr(); !strings.Contains(e, "insert raw") {
		t.Fatalf("state error %q", e)
	}
}

func TestArgs(t *testing.T) {
	for _, args := range [][]string{{"--db", "a-b"}, {"--beat", "2m"}, {"--once", "--check"}, {"x"}, {"--tick", "0"}, {"--url", "nohost"}} {
		if code := run(args, io.Discard, io.Discard); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
	var out bytes.Buffer
	if code := run([]string{"-h"}, &out, io.Discard); code != 0 || !strings.Contains(out.String(), "--once") {
		t.Fatalf("-h: %d %q", code, out.String())
	}
}

func TestStatements(t *testing.T) {
	got := statements("tq")
	if len(got) != 8 || !strings.HasPrefix(got[0], "CREATE DATABASE IF NOT EXISTS tq") {
		t.Fatalf("%d statements: %q", len(got), got)
	}
	for _, s := range got {
		if strings.Contains(s, "{db}") || strings.Contains(s, "--") || !strings.Contains(s, "IF NOT EXISTS") {
			t.Fatalf("statement %q", s)
		}
	}
}

func TestEscape(t *testing.T) {
	in := "a\tb\\c\nd\re\x00f\xff"
	var b bytes.Buffer
	escapeString(&b, in)
	if unescape(b.String()) != in || strings.ContainsAny(b.String(), "\t\n\r") {
		t.Fatalf("escape %q", b.String())
	}
}
