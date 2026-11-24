package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcript"
)

const (
	testProject = "-w-p"
	testSession = "11111111-2222-4333-8444-555555555555"
)

type passWatcher struct {
	drains   int
	events   []watchEvent
	overflow bool
	during   func()
}

func (*passWatcher) add(string) {}
func (w *passWatcher) drain() ([]watchEvent, bool) {
	w.drains++
	if w.drains == 2 {
		if w.during != nil {
			w.during()
		}
		return w.events, w.overflow
	}
	return nil, false
}
func (*passWatcher) active() bool   { return true }
func (*passWatcher) complete() bool { return true }
func (*passWatcher) close()         {}

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
	l, paths := h.started(stop)
	if err := l.visitAll(paths); err != nil {
		h.t.Fatal(err)
	}
	return l
}

// started — a new loader that has read its state and listed the transcripts; the paths are to visit.
func (h *harness) started(stop chan struct{}) (*loader, []string) {
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
	return l, paths
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
	gen1 := f.gen
	if f.id != id || gen1 == 0 {
		t.Fatalf("id %d gen %d after rewind, want id %d and a new gen", f.id, f.gen, id)
	}
	if len(h.fake.deletes) != 4 { // events, replies, usage, raw
		t.Fatalf("deletes %v", h.fake.deletes)
	}
	raws := h.fake.sentTo("raw")
	if last := raws[len(raws)-1]; last.token != fmt.Sprintf("%d:1:g%d", id, gen1) {
		t.Fatalf("reload token %q", last.token)
	}
	if !strings.Contains(h.log.String(), "rewind") {
		t.Fatalf("log %q", h.log.String())
	}
	// shorter than loaded: the same
	write(t, path, bytes.Join(lines[:3], nil))
	h.pass(l)
	if f := h.same(l, path); f.gen <= gen1 {
		t.Fatalf("gen %d after the second rewind, was %d", f.gen, gen1)
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

// A loader stopped at any point of sending a batch — after the dirty files row, after raw, after events — and
// started again: what went past the stored lines is deleted and loaded again, nothing is lost or twice, for a file
// seen before and for a new one, alone in its pass or with another new file.
func TestRestartAfterStopInBatch(t *testing.T) {
	for _, failing := range []string{"raw", "events", "files"} {
		for _, fresh := range []bool{false, true} {
			for _, other := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/new-%t/other-%t", failing, fresh, other), func(t *testing.T) {
					restartAfterStop(t, failing, fresh, other)
				})
			}
		}
	}
}

func restartAfterStop(t *testing.T, failing string, fresh, other bool) {
	h := newHarness(t)
	data := sample(t, "claude.jsonl")
	path := claudePath(h.home)
	half := bytes.Index(data, []byte(`"parentUuid":"d"`)) - 1
	stop := make(chan struct{})
	var l *loader
	var paths []string
	arm := func() {
		h.fake.set(func(f *fakeCH) {
			f.fail[failing] = 1 << 30
			if failing == "files" {
				f.pass["files"] = 1 // the dirty row goes, the clean one does not
			}
		})
	}
	if fresh {
		write(t, path, data[:half])
		arm()
		l, paths = h.started(stop)
	} else {
		write(t, path, data[:half])
		l = h.loader(stop)
		appendTo(t, path, data[half:len(data)-40])
		arm()
	}
	go func() {
		for h.fake.triedFailing(failing) == 0 {
			time.Sleep(time.Millisecond)
		}
		close(stop)
	}()
	if fresh {
		if err := l.visitAll(paths); err != errStopped {
			t.Fatalf("first run: %v, want errStopped", err)
		}
	} else if err := l.pass(false); err != errStopped {
		t.Fatalf("pass: %v, want errStopped", err)
	}
	h.fake.set(func(f *fakeCH) { f.fail[failing], f.pass["files"] = 0, 0 })
	if fresh {
		appendTo(t, path, data[half:])
	} else {
		appendTo(t, path, data[len(data)-40:])
	}
	var second string
	if other {
		second = filepath.Join(h.home, ".claude", "projects", testProject, "22222222-2222-4333-8444-555555555555.jsonl")
		write(t, second, data)
	}
	l2 := h.loader(nil)
	h.same(l2, path)
	if other {
		h.same(l2, second)
	}
	if again := h.loader(nil); len(again.files) == 0 {
		t.Fatal("no files after a third start")
	}
	h.same(l2, path)
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
	want := map[string]any{"heartbeat": "2026-11-22T12:00:01.000Z", "db_through": "", "home": "", "last_row_at": "2026-11-22T12:00:00.000Z", "lag_bytes": 123.0,
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

func TestStateDBThroughOnlyMovesOnZeroLag(t *testing.T) {
	s := newStatus("")
	t0 := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	s.completedPass(t0, 9)
	if !s.dbThrough.IsZero() {
		t.Fatalf("db_through advanced with lag: %v", s.dbThrough)
	}
	s.completedPass(t0, 0)
	if !s.dbThrough.Equal(t0) {
		t.Fatalf("db_through %v, want %v", s.dbThrough, t0)
	}
	s.completedPass(t0.Add(time.Second), 1)
	if !s.dbThrough.Equal(t0) {
		t.Fatalf("db_through advanced after failed boundary: %v", s.dbThrough)
	}
}

func TestStateBoundaryResetsWhenHomeChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	oldHome, newHome := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	old := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	data, _ := json.Marshal(stateFile{Home: oldHome, DBThrough: stamp(old)})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	st := newStatus(path)
	st.forHome(newHome)
	if !st.dbThrough.IsZero() || st.home != newHome {
		t.Fatalf("home change retained boundary %v, home %q", st.dbThrough, st.home)
	}
	data, _ = json.Marshal(stateFile{Home: newHome, DBThrough: stamp(old)})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	st = newStatus(path)
	st.forHome(newHome)
	if !st.dbThrough.Equal(old) {
		t.Fatalf("same home lost boundary %v, want %v", st.dbThrough, old)
	}
}

func TestFailedFullListingDoesNotAdvanceBoundaryAndRetries(t *testing.T) {
	h := newHarness(t)
	data := sample(t, "claude.jsonl")
	path := claudePath(h.home)
	write(t, path, data)
	st := newStatus("")
	old := time.Now().Add(-time.Hour)
	st.completedPass(old, 0)
	stop := make(chan struct{})
	badHome := h.home + "["
	l := newLoader(config{home: badHome, db: "tq", tick: time.Second, idle: time.Hour, sweep: time.Hour, rescan: time.Hour,
		batch: 32 << 20}, h.srv, stop, &h.log, st)
	l.dirs.w = noWatch{}
	if err := l.pass(true); err != nil {
		t.Fatal(err)
	}
	if !l.needsFullList || !st.dbThrough.Equal(old) {
		t.Fatalf("failed listing: needsFullList=%t boundary=%v", l.needsFullList, st.dbThrough)
	}
	l.cfg.home = h.home
	l.dirs = newDirs(h.home, l.logf)
	l.dirs.w = noWatch{}
	if err := l.pass(false); err != nil {
		t.Fatal(err)
	}
	if l.needsFullList || l.files[path] == nil {
		t.Fatalf("retry did not list sources: needsFullList=%t files=%v", l.needsFullList, l.files)
	}
}

func TestCodexCWDStoredInFilesRow(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".local", "state", "worker", "task", "codex", "sessions", "2026", "11", "24", "rollout-test.jsonl")
	write(t, path, []byte(`{"type":"session_meta","payload":{"id":"test","cwd":"/work/akit"}}`+"\n"))
	f := &file{id: 1, gen: 2}
	if err := f.describe(home, transcript.Source{Path: path, ID: "test"}); err != nil {
		t.Fatal(err)
	}
	var row bytes.Buffer
	fileRow(&row, f, 3, false)
	rows := tsvRows(row.Bytes())
	if len(rows) != 1 {
		t.Fatalf("files row count %d", len(rows))
	}
	if got := rows[0][fileCol["cwd"]]; got != "/work/akit" {
		t.Fatalf("stored cwd column %q", got)
	}
	loaded, _, _, err := parseFileRow(home, rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if loaded.cwd != "/work/akit" {
		t.Fatalf("stored cwd %q", loaded.cwd)
	}
}

func TestRestoreMetadataPersistsLegacyCWD(t *testing.T) {
	l := &loader{}
	f := &file{src: transcript.Source{Path: "/p/rollout.jsonl"}, cwd: "/work/akit"}
	row := make([]string, strings.Count(fileColumns, ",")+1)
	if !l.restoreMetadata(f, row) {
		t.Fatal("legacy row with empty cwd was not marked for update")
	}
}

func TestParseFileRowUsesStoredCWDWithoutReadingRollout(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".local", "state", "codex", "sessions", "rollout-missing.jsonl")
	row := make([]string, strings.Count(fileColumns, ",")+1)
	for i := range row {
		row[i] = "0"
	}
	set := func(name, value string) { row[fileCol[name]] = value }
	set("file_id", "7")
	set("path", path)
	set("mtime", "2026-11-24 00:00:00.000")
	set("first_ts", "1970-01-01 00:00:00.000")
	set("last_ts", "1970-01-01 00:00:00.000")
	set("session", "rollout")
	set("cwd", "/work/akit")
	parsed, _, _, err := parseFileRow(home, row)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.cwd != "/work/akit" || parsed.metadataErr != nil {
		t.Fatalf("stored cwd=%q metadata error=%v", parsed.cwd, parsed.metadataErr)
	}
	row[fileCol["cwd"]] = ""
	parsed, _, _, err = parseFileRow(home, row)
	if err != nil {
		t.Fatalf("unreadable legacy rollout prevented loader recovery: %v", err)
	}
	if parsed.metadataErr == nil {
		t.Fatal("missing legacy rollout did not report its cwd read failure for logging")
	}
}

func TestLoadKnownSummarizesMissingCodexRollouts(t *testing.T) {
	h := newHarness(t)
	var rows [][]string
	for i := 1; i <= 3; i++ {
		path := filepath.Join(h.home, ".local", "state", "worker", "task", "codex", "sessions", "2026", "11", "24", fmt.Sprintf("rollout-missing-%d.jsonl", i))
		f := &file{id: uint64(i), gen: 1, kind: "codex", src: transcript.Source{Path: path, ID: fmt.Sprintf("missing-%d", i)}}
		var b bytes.Buffer
		fileRow(&b, f, 1, false)
		rows = append(rows, tsvRows(b.Bytes())[0])
	}
	h.fake.mu.Lock()
	h.fake.rows["files"] = rows
	h.fake.mu.Unlock()
	l := newLoader(config{home: h.home, db: "tq"}, h.srv, make(chan struct{}), &h.log, newStatus(""))
	l.dirs = newDirs(h.home, l.logf)
	l.dirs.w = noWatch{}
	l.backoff = time.Millisecond
	if err := l.loadKnown(); err != nil {
		t.Fatal(err)
	}
	log := h.log.String()
	if strings.Count(log, "restore codex cwd") != 1 || !strings.Contains(log, "3 transcript(s) unavailable") || !strings.Contains(log, "rollout-missing-1.jsonl") {
		t.Fatalf("missing rollout report was not summarized: %q", log)
	}
}

func TestPassDrainsEventsArrivingDuringVisit(t *testing.T) {
	h := newHarness(t)
	l := h.loader(nil)
	path := filepath.Join(h.home, ".claude", "projects", testProject, "new-session.jsonl")
	write(t, path, sample(t, "claude.jsonl"))
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	clock := base
	l.now = func() time.Time { return clock }
	l.st.completedPass(base.Add(-time.Hour), 0)
	l.dirs.w = &passWatcher{events: []watchEvent{{dir: filepath.Dir(path), name: filepath.Base(path)}}, during: func() { clock = base.Add(time.Second) }}
	if err := l.pass(false); err != nil {
		t.Fatal(err)
	}
	if l.files[path] == nil {
		t.Fatal("file announced during the pass was not visited")
	}
	l.st.mu.Lock()
	through := l.st.dbThrough
	l.st.mu.Unlock()
	if !through.Equal(base) {
		t.Fatalf("freshness boundary %v, want pre-drain boundary %v", through, base)
	}
}

func TestPassOverflowForcesListingAndKeepsBoundary(t *testing.T) {
	h := newHarness(t)
	l := h.loader(nil)
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return base }
	l.st.completedPass(base.Add(-time.Hour), 0)
	l.dirs.w = &passWatcher{overflow: true}
	if err := l.pass(false); err != nil {
		t.Fatal(err)
	}
	if !l.needsFullList {
		t.Fatal("watcher overflow did not require a full listing")
	}
	l.st.mu.Lock()
	through := l.st.dbThrough
	l.st.mu.Unlock()
	if !through.Equal(base.Add(-time.Hour)) {
		t.Fatalf("overflow advanced freshness boundary to %v", through)
	}
}

func TestInotifyDrainReadsPendingFDEventsSynchronously(t *testing.T) {
	dir := t.TempDir()
	w, err := newWatcher(func(string, ...any) {})
	if err != nil {
		t.Fatalf("create inotify watcher: %v", err)
	}
	defer w.close()
	w.add(dir)
	write(t, filepath.Join(dir, "ready.jsonl"), []byte("line\n"))
	events, lost := w.drain()
	if lost {
		t.Fatal("inotify reported an overflow")
	}
	if len(events) == 0 {
		t.Fatal("drain left the kernel fd unread")
	}
	for _, event := range events {
		if event.name == "ready.jsonl" {
			return
		}
	}
	t.Fatalf("events %v do not contain the pending file", events)
}

// The state file of a follow run: lag is zero once the server confirmed every complete line; while the server is
// down, the file shows the error and the bytes waiting for it.
func TestStateOfLoader(t *testing.T) {
	h := newHarness(t)
	data := sample(t, "claude.jsonl")
	path := claudePath(h.home)
	write(t, path, data[:len(data)-30])
	l := h.loader(nil)
	state := filepath.Join(t.TempDir(), "state.json")
	l.st = newStatus(state)
	l.report()
	if lag, n := l.lag(); lag != 0 || n != 1 {
		t.Fatalf("lag %d files %d after a pass", lag, n)
	}
	stop, beatDone := make(chan struct{}), make(chan struct{})
	go l.st.beat(time.Millisecond, stop, beatDone, time.Now, func(string, ...any) {})
	l.stop = stop
	h.fake.set(func(f *fakeCH) { f.down = true })
	appendTo(t, path, data[len(data)-30:])
	want := int64(len(data) - bytes.LastIndexByte(data[:len(data)-30], '\n') - 1) // the last line, now complete
	passDone := make(chan error)
	go func() { passDone <- l.pass(false) }()
	var got stateFile
	deadline := time.Now().Add(10 * time.Second)
	for got.LagBytes != want || !strings.Contains(got.Error, "insert") {
		if time.Now().After(deadline) {
			t.Fatalf("state file while the server is down: %+v, want lag_bytes %d and the error", got, want)
		}
		time.Sleep(time.Millisecond)
		if raw, err := os.ReadFile(state); err == nil {
			_ = json.Unmarshal(raw, &got)
		}
	}
	close(stop)
	if err := <-passDone; err != errStopped {
		t.Fatalf("pass %v", err)
	}
	<-beatDone
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
	if len(got) != 14 || !strings.HasPrefix(got[0], "CREATE DATABASE IF NOT EXISTS tq") {
		t.Fatalf("%d statements: %q", len(got), got)
	}
	for _, s := range got {
		if strings.HasPrefix(s, "CREATE TABLE IF NOT EXISTS tq.usage") && !strings.Contains(s, "max_bytes_to_merge_at_max_space_in_pool = 524288") {
			t.Fatalf("usage table lacks the merge cap: %q", s)
		}
		// ALTER migrations run on every start and are idempotent.
		idempotent := strings.Contains(s, "IF NOT EXISTS") || s == "ALTER TABLE tq.files MODIFY COLUMN gen UInt64" || s == "ALTER TABLE tq.files MODIFY COLUMN mtime DateTime64(3, 'UTC')" ||
			s == "ALTER TABLE tq.files MODIFY SETTING max_bytes_to_merge_at_max_space_in_pool = 524288" ||
			s == "ALTER TABLE tq.usage MODIFY SETTING max_bytes_to_merge_at_max_space_in_pool = 524288"
		if strings.Contains(s, "{db}") || strings.Contains(s, "--") || !idempotent {
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

// A rename without growth: the files row gets the new path, though no line is new.
func TestRenameWithoutGrowth(t *testing.T) {
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
	h.pass(l)
	if f := l.files[moved]; f == nil || f.id != id {
		t.Fatal("not followed under the new path")
	}
	if files := h.fake.latestFiles(); len(files) != 1 || files[0][1] != moved {
		t.Fatalf("files row %v, want path %s", files, moved)
	}
}

// Growth by an incomplete line only: the files row records the new size; no parser is built for it.
func TestGrowthByPartialLine(t *testing.T) {
	h := newHarness(t)
	data := sample(t, "claude.jsonl")
	path := claudePath(h.home)
	write(t, path, data)
	l := h.loader(nil)
	h.same(l, path)
	l.cfg.idle = time.Nanosecond // the parser goes
	h.pass(l)
	appendTo(t, path, []byte(`{"partial`))
	h.pass(l)
	files := h.fake.latestFiles()
	if want := strconv.Itoa(len(data) + len(`{"partial`)); files[0][8] != want {
		t.Fatalf("files size %s, file %s", files[0][8], want)
	}
	if l.files[path].follow != nil {
		t.Fatal("a parser built for an incomplete line")
	}
}

// A known worker task starts a session on another day: the rollout in the new day folder is found within a few
// passes, with inotify and by polling alone.
func TestNewDayFolderInKnownTask(t *testing.T) {
	for _, useInotify := range []bool{false, true} {
		t.Run(fmt.Sprintf("inotify-%t", useInotify), func(t *testing.T) {
			h := newHarness(t)
			data := sample(t, "codex.jsonl")
			month := filepath.Join(h.home, ".local", "state", "worker", "job1", "codex", "sessions", "2026", "11")
			first := filepath.Join(month, "21", "rollout-2026-11-21T23-52-00-01a172ac-0000-7000-8000-000000000001.jsonl")
			write(t, first, data)
			l, paths := h.started(nil)
			if useInotify {
				w, err := newWatcher(l.logf)
				if err != nil {
					t.Fatal(err)
				}
				defer w.close()
				l.dirs.w = w
				l.dirs.known, l.dirs.order, l.dirs.on, l.dirs.polled = map[string]time.Time{}, nil, map[string]bool{}, nil
				for _, f := range l.all {
					l.dirs.addFileDir(f.src.Path, f.mtime)
				}
				for _, p := range paths {
					l.dirs.addFileDir(p, time.Time{})
				}
				l.dirs.watchClaude()
				l.cfg.sweep = time.Hour // inotify alone
			}
			if err := l.visitAll(paths); err != nil {
				t.Fatal(err)
			}
			l.cfg.idle = time.Nanosecond // the first session is quiet: its parser and folders are not polled
			h.pass(l)
			second := filepath.Join(month, "22", "rollout-2026-11-22T04-38-00-01a172ac-0000-7000-8000-000000000002.jsonl")
			write(t, second, data)
			deadline := time.Now().Add(5 * time.Second)
			for passes := 0; l.files[second] == nil || passes < 3; passes++ {
				if time.Now().After(deadline) {
					t.Fatalf("rollout in a new day folder of a known task not found after %d passes", passes)
				}
				h.pass(l)
				time.Sleep(time.Millisecond)
			}
			h.same(l, second)
		})
	}
}

// A file that cannot be read when it is first seen is looked at again on the next pass, though its size and time
// did not change.
func TestUnreadableFileLookedAgain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file without permissions")
	}
	h := newHarness(t)
	data := sample(t, "claude.jsonl")
	path := claudePath(h.home)
	write(t, path, data)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	l := h.loader(nil)
	if n := len(h.fake.sentTo("raw")); n != 0 {
		t.Fatalf("%d raw inserts from an unreadable file", n)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	h.pass(l)
	h.same(l, path)
}

// A parser released after idle is built again without parsing the lines whose events are loaded: the events of
// the new lines are the same, bad lines keep their count.
func TestParserRebuiltSkipsLoadedLines(t *testing.T) {
	h := newHarness(t)
	data := sample(t, "claude.jsonl")
	path := claudePath(h.home)
	half := bytes.Index(data, []byte(`"parentUuid":"j"`)) - 1 // past the broken line: it is among the skipped
	write(t, path, data[:half])
	l := h.loader(nil)
	l.cfg.idle = time.Nanosecond
	h.pass(l)
	appendTo(t, path, data[half:])
	h.pass(l)
	f := h.same(l, path)
	if meta, _ := transcript.Read(path, nil, func(transcript.Event) {}); f.bad != meta.Bad {
		t.Fatalf("bad %d, transcript.Read %d", f.bad, meta.Bad)
	}
}

// An empty --state keeps no state file; without --state the file is in the state folder of the home.
func TestStateFlag(t *testing.T) {
	home := t.TempDir()
	o, err := parse([]string{"--home", home, "--state", ""})
	if err != nil || o.cfg.state != "" {
		t.Fatalf("--state '': state %q, err %v", o.cfg.state, err)
	}
	o, err = parse([]string{"--home", home})
	if err != nil || o.cfg.state != defaultStatePath(home) {
		t.Fatalf("no --state: state %q, err %v", o.cfg.state, err)
	}
}

// A loader stopped while the server still holds its insert: the server applies it only after the next start has
// cleaned up and loaded the file again. The late rows are turned away by the generation, nothing is twice.
func TestLateInsertOfStoppedLoader(t *testing.T) {
	for _, table := range []string{"raw", "events"} {
		for _, fresh := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/new-%t", table, fresh), func(t *testing.T) {
				h := newHarness(t)
				data := sample(t, "claude.jsonl")
				path := claudePath(h.home)
				half := bytes.Index(data, []byte(`"parentUuid":"d"`)) - 1
				stop := make(chan struct{})
				write(t, path, data[:half])
				var l *loader
				var paths []string
				if fresh {
					l, paths = h.started(stop)
				} else {
					l = h.loader(stop)
					appendTo(t, path, data[half:])
				}
				h.fake.set(func(f *fakeCH) { f.hold[table] = 1 << 30 }) // every try is held, to come late
				go func() {
					for h.fake.triedFailing(table) == 0 {
						time.Sleep(time.Millisecond)
					}
					close(stop)
				}()
				var err error
				if fresh {
					err = l.visitAll(paths)
				} else {
					err = l.pass(false)
				}
				if err != errStopped {
					t.Fatalf("first run: %v, want errStopped", err)
				}
				h.fake.set(func(f *fakeCH) { f.hold[table] = 0 })
				if fresh {
					appendTo(t, path, data[half:])
				}
				l2 := h.loader(nil)
				if h.fake.kills != 1 {
					t.Fatalf("%d KILL QUERY at the start after a stop in a batch, want 1", h.fake.kills)
				}
				h.fake.sendLate()
				h.same(l2, path)
			})
		}
	}
}

// A rollout whose first message events come while it has no parser (after a restart or an idle release): the
// rebuilt parser sees them in the whole file, so the events of the earlier lines, made without them, are made again.
func TestMessageEventsBeganWithoutParser(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart-%t", restart), func(t *testing.T) {
			h := newHarness(t)
			path := filepath.Join(h.home, ".local", "state", "worker", "job1", "codex", "sessions", "2026", "10", "25",
				"rollout-2026-10-25T10-47-55-01a172ac-0000-7000-8000-000000000001.jsonl")
			write(t, path, sample(t, "codex.jsonl"))
			l := h.loader(nil)
			l.cfg.idle = time.Nanosecond // the parser goes
			h.pass(l)
			appendTo(t, path, []byte(`{"timestamp":"2026-10-25T10:51:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"second question","images":[]}}`+"\n"+
				`{"timestamp":"2026-10-25T10:51:05.000Z","type":"event_msg","payload":{"type":"agent_message","message":"second answer"}}`+"\n"))
			if restart {
				l = h.loader(nil)
			} else {
				h.pass(l)
			}
			h.same(l, path)
			if !strings.Contains(h.log.String(), "message events began past the stored events") {
				t.Fatalf("log %q", h.log.String())
			}
		})
	}
}

// Only worker tasks written to lately have their session folders watched; an old task that starts a session on a
// new day is found by the sweep of the folders not watched, and from then on its folders are watched.
func TestOldWorkerTaskFoldersPolled(t *testing.T) {
	h := newHarness(t)
	data := sample(t, "codex.jsonl")
	task := func(name, day string) (string, string) {
		dir := filepath.Join(h.home, ".local", "state", "worker", name, "codex", "sessions", "2026", "11")
		return filepath.Join(dir, day, "rollout-2026-11-"+day+"T10-00-00-01a172ac-0000-7000-8000-00000000000"+name[len(name)-1:]+".jsonl"),
			filepath.Join(dir, day)
	}
	oldFile, oldDay := task("old1", "01")
	newFile, newDay := task("new2", "22")
	write(t, oldFile, data)
	write(t, newFile, data)
	long := time.Now().Add(-10 * 24 * time.Hour)
	for _, p := range []string{oldFile, oldDay, filepath.Dir(oldDay)} {
		if err := os.Chtimes(p, long, long); err != nil {
			t.Fatal(err)
		}
	}
	h.loader(nil) // the files table now has both, the old one with its old mtime
	l, paths := h.started(nil)
	w, err := newWatcher(l.logf)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	l.dirs.w = w
	l.dirs.known, l.dirs.order, l.dirs.on, l.dirs.polled = map[string]time.Time{}, nil, map[string]bool{}, nil
	for _, f := range l.all {
		l.dirs.addFileDir(f.src.Path, f.mtime)
	}
	l.dirs.watchClaude()
	if err := l.visitAll(paths); err != nil {
		t.Fatal(err)
	}
	if !l.dirs.on[newDay] || !l.dirs.on[filepath.Dir(newDay)] {
		t.Fatalf("folders of the recent task not watched: %v", l.dirs.on)
	}
	if l.dirs.on[oldDay] || l.dirs.on[filepath.Dir(oldDay)] {
		t.Fatalf("folders of the old task watched")
	}
	l.cfg.sweep = l.cfg.tick // every folder not watched is swept each pass
	resumed, _ := task("old1", "23")
	write(t, resumed, data)
	// the sweep of all known folders, one a pass here, polls next the one past the changed month folder
	l.dirs.next = slices.Index(l.dirs.order, filepath.Dir(oldDay))
	h.pass(l)
	h.same(l, resumed)
	if !l.dirs.on[filepath.Dir(resumed)] {
		t.Fatal("the folders of the resumed task are not watched")
	}
}

// A loader stopped after a files row took the grown size of a file but before its new lines were loaded (here a
// rollout whose message events began: its events are made again after that row): the next start finds the file
// as large as the row says and still loads the lines past ingested.
func TestStoppedAfterSizeStoredBeforeLines(t *testing.T) {
	h := newHarness(t)
	data := sample(t, "claude.jsonl")
	path := claudePath(h.home)
	half := bytes.Index(data, []byte(`"parentUuid":"d"`)) - 1
	write(t, path, data[:half])
	l := h.loader(nil)
	appendTo(t, path, data[half:])
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f := l.files[path]
	f.size, f.mtime, f.inode = st.Size(), st.ModTime(), transcript.Inode(st)
	l.batch.touch(f)
	if err := l.flush(); err != nil {
		t.Fatal(err)
	}
	h.same(h.loader(nil), path)
}

// A second loader on a database another one works on (follow, --once or --materialize) ends at once with code 2
// and the pid of the holder, whatever its --state and --home; --check and --verify take no lock. Without
// XDG_RUNTIME_DIR the lock is in the temporary folder.
func TestLockOneLoaderPerDatabase(t *testing.T) {
	for _, runtimeDir := range []bool{true, false} {
		t.Run(fmt.Sprintf("runtime-dir-%t", runtimeDir), func(t *testing.T) {
			h := newHarness(t)
			write(t, claudePath(h.home), sample(t, "claude.jsonl"))
			if runtimeDir {
				t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			} else {
				t.Setenv("XDG_RUNTIME_DIR", "")
				t.Setenv("TMPDIR", t.TempDir())
			}
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			// the first loader: its own state file and home
			cfg := config{home: h.home, db: "tq", state: filepath.Join(t.TempDir(), "a", "state.json"), tick: time.Second,
				idle: time.Hour, sweep: time.Second, rescan: time.Hour, batch: 32 << 20, lock: lockDir()}
			first := newLoader(cfg, h.srv, make(chan struct{}), io.Discard, newStatus(""))
			if err := first.lock(); err != nil {
				t.Fatal(err)
			}
			otherHome := t.TempDir()
			write(t, claudePath(otherHome), sample(t, "claude.jsonl"))
			base := []string{"--url", h.srv.base, "--db", "tq"}
			seconds := [][]string{
				{"--home", otherHome, "--state", filepath.Join(t.TempDir(), "b", "state.json")},
				{"--home", h.home, "--state", ""},
				{"--home", otherHome}, // the default state file
			}
			want := fmt.Sprintf("another loader works on this database: pid %d holds", os.Getpid())
			for _, second := range seconds {
				for _, mode := range [][]string{{"--once"}, {"--materialize"}, {"--tick", "1ms"}} {
					args := slices.Concat(base, second, mode)
					var out, errOut bytes.Buffer
					if code := run(args, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), want) {
						t.Fatalf("%q with the lock held: exit %d, %q", args, code, errOut.String())
					}
				}
			}
			if len(h.fake.inserts) != 0 {
				t.Fatalf("a second loader wrote: %d inserts", len(h.fake.inserts))
			}
			if _, err := os.Stat(seconds[0][3]); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a second loader wrote its state file: %v", err)
			}
			var out, errOut bytes.Buffer
			if code := run(slices.Concat(base, seconds[0], []string{"--check"}), &out, &errOut); strings.Contains(errOut.String(), "another loader") {
				t.Fatalf("--check took the lock: exit %d, %q", code, errOut.String())
			}
			first.lockFile.Close()
			out.Reset()
			errOut.Reset()
			if code := run(slices.Concat(base, seconds[0], []string{"--once"}), &out, &errOut); code != 0 {
				t.Fatalf("--once after the lock is free: exit %d, %q", code, errOut.String())
			}
		})
	}
}

// A loader waits on its insert, alive, while another one starts on the same database (as with a lock in another
// folder): the second takes the files on with a new generation, the insert of the first is turned away, and the
// first stops instead of writing a clean files row over them.
func TestTwinLoaderInsertTurnedAway(t *testing.T) {
	for _, table := range []string{"raw", "events"} {
		t.Run(table, func(t *testing.T) {
			h := newHarness(t)
			data := sample(t, "claude.jsonl")
			path := claudePath(h.home)
			half := bytes.Index(data, []byte(`"parentUuid":"d"`)) - 1
			write(t, path, data[:half])
			l1 := h.loader(nil)
			appendTo(t, path, data[half:])
			p := h.fake.pauseNext(table)
			errc := make(chan error, 1)
			go func() { errc <- l1.pass(false) }()
			select {
			case <-p.arrived:
			case err := <-errc:
				t.Fatalf("first loader ended before its %s insert: %v", table, err)
			case <-time.After(time.Minute):
				t.Fatalf("no %s insert of the first loader within a minute", table)
			}
			h.loader(nil)
			close(p.release)
			if err := <-errc; err == nil || !strings.Contains(err.Error(), "newer generation") {
				t.Fatalf("first loader after its insert was turned away: %v", err)
			}
			h.same(h.loader(nil), path)
		})
	}
}

// Two loaders on one database with no lock between them (a lock folder each), both knowing a file that is then
// cut back: each takes a generation of its own, so whichever comes second to the file stops with an error at its
// first write and the other leaves the tables complete — no row lost, none twice.
func TestTwoLoadersWithoutLock(t *testing.T) {
	for _, at := range []string{"files", "raw"} {
		t.Run("first-waits-on-"+at, func(t *testing.T) {
			h := newHarness(t)
			data := sample(t, "claude.jsonl")
			path := claudePath(h.home)
			write(t, path, data)
			l1 := h.loader(nil)
			l2 := h.loader(nil)
			half := bytes.Index(data, []byte(`"parentUuid":"d"`)) - 1
			write(t, path, data[:half]) // cut back: both rewind it
			p := h.fake.pauseNext(at)
			errc := make(chan error, 1)
			go func() { errc <- l1.pass(false) }()
			select {
			case <-p.arrived:
			case err := <-errc:
				t.Fatalf("first loader ended before its %s insert: %v", at, err)
			case <-time.After(time.Minute):
				t.Fatalf("no %s insert of the first loader within a minute", at)
			}
			err2 := l2.pass(false)
			close(p.release)
			err1 := <-errc
			kept, err := l2, err1 // the first waited on its dirty row: the second took the file first
			if at == "raw" {
				// the first had its new generation stored: the second finds it taken
				kept, err = l1, err2
				if err1 != nil {
					t.Fatalf("the loader that took the file first: %v", err1)
				}
			} else if err2 != nil {
				t.Fatalf("the loader that took the file first: %v", err2)
			}
			if err == nil || !strings.Contains(err.Error(), "another loader works on") {
				t.Fatalf("the loader second to the file: %v, want it stopped", err)
			}
			h.same(kept, path)
		})
	}
}

// The same with the two files rows of the new generations both checked against the stored one before either is
// written (as two inserts running at once on the server). The row of the higher ver wins, here the second's; as
// the generations differ, the deletes of the first pass by the second's rows and the first stops at its clean row.
func TestTwoLoadersWithoutLockAtOnce(t *testing.T) {
	h := newHarness(t)
	data := sample(t, "claude.jsonl")
	path := claudePath(h.home)
	write(t, path, data)
	l1 := h.loader(nil)
	l2 := h.loader(nil)
	half := bytes.Index(data, []byte(`"parentUuid":"d"`)) - 1
	write(t, path, data[:half])
	p := h.fake.pauseNext("files")
	p.early = true
	errc := make(chan error, 1)
	go func() { errc <- l1.pass(false) }()
	select {
	case <-p.arrived:
	case err := <-errc:
		t.Fatalf("first loader ended before its files insert: %v", err)
	case <-time.After(time.Minute):
		t.Fatal("no files insert of the first loader within a minute")
	}
	if err := l2.pass(false); err != nil {
		t.Fatalf("second loader: %v", err)
	}
	close(p.release)
	if err := <-errc; err == nil || !strings.Contains(err.Error(), "another loader works on") {
		t.Fatalf("first loader, its row older: %v, want it stopped", err)
	}
	h.same(l2, path)
}

// TestStoreFilesManyFitsURI: a startup rewrites the metadata of thousands of known rows; the generation guard of
// one insert lists every file, so the rows go in batches whose query fits the server's URI limit.
func TestStoreFilesManyFitsURI(t *testing.T) {
	h := newHarness(t)
	l, _ := h.started(nil)
	h.fake.mu.Lock()
	h.fake.maxURI = 32 << 10 // one batch is about 21 KiB, all three about 63 KiB
	h.fake.mu.Unlock()
	const n = 3 * filesBatch
	fs := make([]*file, n)
	gens := make([]uint64, n)
	for i := range fs {
		fs[i] = &file{id: 1_000_000_000_000 + uint64(i), kind: "codex", gen: 7}
		fs[i].src.Path = filepath.Join(h.home, fmt.Sprintf("r%05d.jsonl", i))
		gens[i] = 7
	}
	done := make(chan error, 1)
	go func() { done <- l.storeFilesKeepSize(fs, gens) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("store %d files: %v", n, err)
		}
	case <-time.After(20 * time.Second): // a refused insert is retried without end
		t.Fatalf("store %d files: no answer in 20 s, the insert is refused", n)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if got := len(h.fake.rows["files"]); got != n {
		t.Fatalf("files rows %d, want %d", got, n)
	}
}
