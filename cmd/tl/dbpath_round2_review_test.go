package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/akit/session"
	"github.com/aiseeq/akit/transcript"
)

func TestDBFilePredicateCarriesProjectCWDAndLiveSession(t *testing.T) {
	project := &dbPath{opts: options{all: true, codex: true, project: "akit"}}
	w := project.allFilePredicate()
	if !strings.Contains(w.sql, "position(cwd, {project:String}) > 0") {
		t.Fatalf("codex project predicate omits stored cwd: %s", w.sql)
	}

	home := t.TempDir()
	stat, err := proc.ReadStat("/proc", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	alive := session.Info{Pid: os.Getpid(), SessionID: "0199aaaa-0000-7000-8000-000000000001", ProcStart: stat.Start, Name: "live-name"}
	registry := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(registry, 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(alive)
	if err := os.WriteFile(filepath.Join(registry, fmt.Sprintf("%d.json", alive.Pid)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	d := &dbPath{opts: options{args: []string{"live-name"}}, e: env{home: home}}
	w, err = d.sessionFilePredicate()
	if err != nil {
		t.Fatal(err)
	}
	if w.params["find_id_0"] != alive.SessionID || !strings.Contains(w.sql, "startsWith(session") {
		t.Fatalf("live name predicate = SQL %q params %v", w.sql, w.params)
	}
}

func TestDBShortLinksUseNamesOutsideFilteredFiles(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	extra := writeReviewClaude(t, e, "-home-x-other", "01a20453-0000-4000-8000-000000000099", userRecord("2026-11-24T10:00:00.000Z", "outside"))
	extra2 := writeReviewClaude(t, e, "-home-x-other", "01a20453-0000-4000-8000-000000000098", userRecord("2026-11-24T10:00:00.000Z", "outside two"))
	ch := newFakeCH(t, append(paths, extra, extra2), unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	c, err := newCHClient(srv.URL, "tr")
	if err != nil {
		t.Fatal(err)
	}
	d := &dbPath{c: c, ctx: context.Background()}
	names, err := d.linkNames()
	if err != nil {
		t.Fatal(err)
	}
	want := sourceLinkIDs([]source{{Path: extra, ID: "01a20453"}}, names, e.home, nil)[extra]
	if want == "01a20453:" || !strings.Contains(want, "-") {
		t.Fatalf("link id %q did not disambiguate with the full database names", want)
	}
}

func TestDBResultUsesNamesOutsideFilteredFiles(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	extra := writeReviewClaude(t, e, "-home-x-other", "1a2b3c4d-1111-4111-8111-111111111111",
		userRecord("2026-10-25T10:00:00.000Z", "outside project"))
	ch := newFakeCH(t, append(paths, extra), unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	args := []string{"--all", "--project", "-home-x-proj", "--role", "all", "--max", "0"}
	e.backend = backendFiles
	wantCode, wantOut, _ := runTL(t, e, args...)
	e.backend = backendDB
	code, out, stderr := runTL(t, e, args...)
	if code != wantCode || out != wantOut || strings.Contains(stderr, "reading files instead") {
		t.Fatalf("filtered link ids differ: files=(%d,%q), db=(%d,%q,%q)", wantCode, wantOut, code, out, stderr)
	}
}

func TestDBResultAvoidsLiveSessionLinkNames(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	stat, err := proc.ReadStat("/proc", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	info := session.Info{Pid: os.Getpid(), SessionID: "0199aaaa-0000-7000-8000-000000000001", ProcStart: stat.Start, Name: sessionID[:8]}
	registry := filepath.Join(e.home, ".claude", "sessions")
	if err := os.MkdirAll(registry, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry, fmt.Sprintf("%d.json", info.Pid)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	args := []string{"--all", "--role", "all", "--max", "0"}
	e.backend = backendFiles
	wantCode, wantOut, _ := runTL(t, e, args...)
	e.backend = backendDB
	code, out, stderr := runTL(t, e, args...)
	if code != wantCode || out != wantOut || strings.Contains(stderr, "reading files instead") {
		t.Fatalf("live-name links differ: files=(%d,%q), db=(%d,%q,%q)", wantCode, wantOut, code, out, stderr)
	}
}

func TestDBCodexProjectUsesRolloutCWD(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	rollout := filepath.Join(e.home, ".local", "state", "worker", "w2", "codex", "sessions", "2026", "11", "24",
		"rollout-2026-11-24T10-00-00-0199cccc-0000-7000-8000-000000000003.jsonl")
	writeWorkFile(t, rollout, []string{
		`{"timestamp":"2026-11-24T10:00:00.000Z","type":"session_meta","payload":{"id":"0199cccc","cwd":"/work/akit"}}`,
		`{"timestamp":"2026-11-24T10:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"codex project match"}]}}`,
	})
	probeServe(t, append(paths, rollout), unloaded)
	probeCompare(t, e, "codex cwd project", "--all", "--codex", "--project", "akit", "--role", "all", "--max", "0")
}

func TestDBFindPrioritizesLiveNames(t *testing.T) {
	home := t.TempDir()
	projects := filepath.Join(home, ".claude", "projects", "-home-akit")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	d := &dbPath{e: env{home: home}, files: map[string]*dbFile{}}
	for i := range 119 {
		path := filepath.Join(projects, fmt.Sprintf("%08x-0000-4000-8000-%012x.jsonl", i+1, i+1))
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		d.files[path] = &dbFile{path: path, name: "lead", session: filepath.Base(path)[:8]}
	}
	for _, name := range []string{"lead", "akit-master"} {
		id := "00000077"
		path := filepath.Join(projects, id+"-0000-4000-8000-000000000077.jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		d.files[path] = &dbFile{path: path, name: name, session: id}
		got, err := d.findAfterLive(name, map[string]session.Info{name: {SessionID: filepath.Base(path)[:len(filepath.Base(path))-6]}}, nil)
		if err != nil || got != path {
			t.Errorf("find %q = %q, %v; want live path %q", name, got, err, path)
		}
	}
	for _, name := range []string{"lead", "akit-master"} {
		if _, err := d.findAfterLive(name, nil, nil); err == nil || strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("non-live name %q was matched as a transcript name: %v", name, err)
		}
	}
}

func TestDBFindSkipsGoneIDPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone-00000000-0000-4000-8000-000000000001.jsonl")
	d := &dbPath{e: env{home: t.TempDir()}, files: map[string]*dbFile{path: {path: path, name: "gone-session", session: "gone0000"}}}
	if got, err := d.findAfterLive("gone0000", nil, nil); err != nil || got != path {
		t.Fatalf("gone file lookup = %q, %v; want database path %q", got, err, path)
	}
}

func TestDBFindRejectsShortIDsAndUnloadedLiveSessions(t *testing.T) {
	d := &dbPath{files: map[string]*dbFile{}}
	if _, err := d.findAfterLive("abc", nil, nil); err == nil || !strings.Contains(err.Error(), "at least 6") {
		t.Fatalf("short id error = %v", err)
	}
	_, err := d.findAfterLive("live-name", map[string]session.Info{"live-name": {SessionID: "0199aaaa-0000-7000-8000-000000000001"}}, nil)
	if !errors.Is(err, errLiveSessionNotLoaded) || !strings.Contains(err.Error(), "not loaded") {
		t.Fatalf("unloaded live session error = %v", err)
	}
}

func TestDBUnloadedLiveSessionFallsBackToFiles(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	writeWorkFile(t, paths[0], []string{userRecord("2026-10-25T10:00:00.000Z", "synthetic live session")})
	stat, err := proc.ReadStat("/proc", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	info := session.Info{Pid: os.Getpid(), SessionID: sessionID, ProcStart: stat.Start, Name: "active-fixture"}
	registry := filepath.Join(e.home, ".claude", "sessions")
	if err := os.MkdirAll(registry, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry, fmt.Sprintf("%d.json", info.Pid)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	ch := newFakeCH(t, paths, unloaded)
	ch.files = ch.files[1:]
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	e.backend = backendFiles
	wantCode, wantOut, _ := runTL(t, e, "active-fixture", "--role", "all")
	e.backend = backendDB
	code, out, stderr := runTL(t, e, "active-fixture", "--role", "all")
	if code != wantCode || code != 0 || out != wantOut || !strings.Contains(stderr, "not loaded in the database") || !strings.Contains(stderr, "reading files instead") {
		t.Fatalf("live fallback: files=(%d,%q), db=(%d,%q,%q)", wantCode, wantOut, code, out, stderr)
	}
}

func TestDBTranscriptMissingFromFilesFallsBackForAllSessionForms(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	e.sessionID = sessionID
	stat, err := proc.ReadStat("/proc", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	info := session.Info{Pid: os.Getpid(), SessionID: sessionID, ProcStart: stat.Start, Name: "active-fixture"}
	registry := filepath.Join(e.home, ".claude", "sessions")
	if err := os.MkdirAll(registry, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry, fmt.Sprintf("%d.json", info.Pid)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	ch := newFakeCH(t, paths, unloaded)
	ch.files = ch.files[1:]
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)

	for _, args := range [][]string{
		{sessionID[:8], "--role", "all"},
		{paths[0], "--role", "all"},
		{"--role", "all"},
	} {
		e.backend = backendFiles
		wantCode, wantOut, _ := runTL(t, e, args...)
		e.backend = backendDB
		code, out, warning := runTL(t, e, args...)
		if wantCode != 0 || code != 0 || out != wantOut || !strings.Contains(warning, "reading files instead") {
			t.Errorf("args %q: files=(%d,%q), db=(%d,%q,%q)", args, wantCode, wantOut, code, out, warning)
		}
	}
}

func TestDBRetentionBoundaryFallsBackAt89Days(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	path := "/p/session.jsonl"
	src := source{Path: path}
	files := map[string]*dbFile{path: {path: path, first: now.Add(-90 * 24 * time.Hour)}}
	if !dbRetentionExceeded(options{}, []source{src}, files, now) {
		t.Fatal("old session stayed on the database path")
	}
	files[path].first = now.Add(-88 * 24 * time.Hour)
	if dbRetentionExceeded(options{}, []source{src}, files, now) {
		t.Fatal("recent session fell back")
	}
	if !dbRetentionExceeded(options{since: now.Add(-90 * 24 * time.Hour)}, nil, nil, now) {
		t.Fatal("old --since window stayed on the database path")
	}
}

func TestDBRetentionTreatsZeroFirstTimestampAsUnknown(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	path := "/p/empty.jsonl"
	if dbRetentionExceeded(options{}, []source{{Path: path}}, map[string]*dbFile{path: {path: path}}, now) {
		t.Fatal("unknown first timestamp caused retention fallback")
	}
}

func TestDBEmptyFilesRowKeepsZeroFirstTimestampUnknown(t *testing.T) {
	e := fakeHome(t)
	now := time.Now()
	path := filepath.Join(e.home, ".claude", "projects", "-home-empty", "empty-00000000-0000-4000-8000-000000000001.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	probeServe(t, []string{path}, nil)
	c, err := newCHClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	d := &dbPath{c: c, ctx: context.Background(), opts: options{all: true}, e: env{home: e.home, now: now}}
	if err := d.loadFiles(); err != nil {
		t.Fatal(err)
	}
	if !d.files[path].first.IsZero() || dbRetentionExceeded(options{}, []source{{Path: path}}, d.files, now) {
		t.Fatalf("empty files first=%v triggered retention fallback", d.files[path].first)
	}
}

func probeServe(t *testing.T, paths []string, unloaded map[string]int) {
	t.Helper()
	ch := newFakeCH(t, paths, unloaded)
	for _, f := range ch.files {
		if f.loaded.Year() < 2000 { // fake: no loaded line gives no load time
			f.loaded = time.Now()
		}
	}
	srv := httptest.NewServer(ch)
	t.Cleanup(srv.Close)
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
}

// A codex rollout with a line that is not JSON and message events: the stored bad lines are parsed again.
func TestProbeCodexBadLineWithMessageEvents(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	rollout := filepath.Join(e.home, ".local", "state", "worker", "w1", "codex", "sessions", "2026", "10", "25",
		"rollout-2026-10-25T11-00-00-0199bbbb-0000-7000-8000-000000000002.jsonl")
	writeWorkFile(t, rollout, []string{
		`{"timestamp":"2026-10-25T11:00:00.000Z","type":"session_meta","payload":{"id":"0199bbbb","cwd":"/w"}}`,
		`{"timestamp":"2026-10-25T11:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"бриф sub"}]}}`,
		`not json at all`,
		`{"timestamp":"2026-10-25T11:00:02.000Z","type":"event_msg","payload":{"type":"user_message","message":"привет sub"}}`,
		`{"timestamp":"2026-10-25T11:00:03.000Z","type":"event_msg","payload":{"type":"agent_message","message":"ответ sub"}}`,
	})
	probeServe(t, append(paths, rollout), unloaded)
	probeCompare(t, e, "codex bad + message events", "--all", "--codex", "--role", "all", "--max", "0")
	probeCompare(t, e, "codex bad + message events grep", "--all", "--codex", "--grep", "sub", "--max", "0")
}

// Raw line accounting includes a bad line after the last stored event line.
func TestProbeBadRawLinePastLastEvent(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	p := writeReviewClaude(t, e, "-home-x-proj", "4b4b4b4b-0000-4000-8000-000000000008",
		userRecord("2026-10-25T10:20:00.000Z", "stored event sub"),
		`not json needle`)
	ch := newFakeCH(t, append(paths, p), unloaded)
	for _, f := range ch.files {
		if f.path == p {
			f.evLines = 1
		}
	}
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	probeCompare(t, e, "bad raw line after last event", "--all", "--grep", "needle", "--max", "0")
}

// A Claude transcript with a bad line among records, no grep and grep.
func TestProbeClaudeBadLines(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	p := writeReviewClaude(t, e, "-home-x-proj", "4a4a4a4a-0000-4000-8000-000000000008",
		userRecord("2026-10-25T10:20:00.000Z", "первая sub"),
		`{"broken": `,
		`{"type":"user","timestamp":"bad","message":{"role":"user","content":"плохое время sub"}}`,
		userRecord("2026-10-25T10:20:02.000Z", "третья"))
	probeServe(t, append(paths, p), unloaded)
	probeCompare(t, e, "bad no grep", "--all", "--role", "all", "--max", "0")
	probeCompare(t, e, "bad grep sub", "--all", "--grep", "sub", "--max", "0")
	probeCompare(t, e, "bad grep broken", "--all", "--grep", "broken", "--max", "0")
	probeCompare(t, e, "bad session", "4a4a4a4a", "--role", "all")
}

// The home folder has a part named subagents: --no-subagents must keep the main transcripts.
func TestProbeHomeNamedSubagents(t *testing.T) {
	e := fakeHome(t)
	e.home = filepath.Join(e.home, "subagents", "h")
	statePath := filepath.Join(os.Getenv("XDG_STATE_HOME"), "akit", "tlfeed", "state.json")
	state := dbState{Heartbeat: time.Now().Format(time.RFC3339Nano), DBThrough: time.Now(), Home: e.home}
	raw, _ := json.Marshal(state)
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p := writeReviewClaude(t, e, "-home-x-proj", "5a5a5a5a-0000-4000-8000-000000000008",
		userRecord("2026-10-25T10:20:00.000Z", "главная сессия sub"))
	ch := newFakeCH(t, []string{p}, map[string]int{})
	for _, f := range ch.files {
		f.kind = "claude"
		f.parent = ""
	}
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	probeCompare(t, e, "home/subagents --no-subagents", "--all", "--no-subagents", "--grep", "sub", "--max", "0")
}

// The file cannot be opened although stat works: files path reports it, db path must not fail the whole call.
func TestProbeUnreadableStored(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads anything")
	}
	e, paths, unloaded := dbCorpus(t)
	p := writeReviewClaude(t, e, "-home-x-proj", "6a6a6a6a-0000-4000-8000-000000000008",
		userRecord("2026-10-25T10:20:00.000Z", "закрытый sub"))
	probeServe(t, append(paths, p), unloaded)
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	e.backend = backendDB
	code, out, stderr := runTL(t, e, "--all", "--grep", "закрытый", "--max", "0")
	if code != 0 || !strings.Contains(out, "закрытый sub") || strings.Contains(stderr, "reading files instead") {
		t.Fatalf("unreadable disk transcript affected database answer: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

// The loader keeps the words of the user in Claude Code sessions past the event retention: a search of them alone
// stays on the database at any depth, while any other role or a Codex rollout still falls back.
func TestDBRetentionKeepsClaudeUserWords(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	old := now.Add(-100 * 24 * time.Hour)
	claude := "/p/session.jsonl"
	codex := "/w/rollout-2026-08-01T10-00-00-0199aaaa-0000-7000-8000-000000000001.jsonl"
	sub := "/p/session/subagents/agent-a1.jsonl"
	files := map[string]*dbFile{claude: {path: claude, first: old}, codex: {path: codex, first: old}, sub: {path: sub, first: old}}
	userWords := map[transcript.Role]bool{transcript.User: true, transcript.Ask: true}
	for _, tc := range []struct {
		name    string
		opts    options
		sources []source
		want    bool
	}{
		{"user words of an old session", options{roles: userWords}, []source{{Path: claude}}, false},
		{"user words of an old window", options{roles: userWords, since: old}, nil, false},
		{"user and agent of an old session", options{roles: map[transcript.Role]bool{transcript.User: true, transcript.Agent: true}}, []source{{Path: claude}}, true},
		{"user words of an old rollout", options{roles: userWords, codex: true}, []source{{Path: codex}}, true},
		{"user words of an old window with codex", options{roles: userWords, codex: true, since: old}, nil, true},
		{"user words of an old window with codex-ext", options{roles: userWords, codexExt: true, since: old}, nil, true},
		{"no role filter", options{}, []source{{Path: claude}}, true},
		{"user words of an old subagent", options{roles: userWords}, []source{{Path: sub, ParentID: "session"}}, false},
		{"agent of an old subagent", options{roles: map[transcript.Role]bool{transcript.Agent: true}}, []source{{Path: sub, ParentID: "session"}}, true},
	} {
		if got := dbRetentionExceeded(tc.opts, tc.sources, files, now); got != tc.want {
			t.Errorf("%s: dbRetentionExceeded = %t, want %t", tc.name, got, tc.want)
		}
	}
}
