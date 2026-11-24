package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/session"
)

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

func TestDBFindSkipsGoneNamedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone-00000000-0000-4000-8000-000000000001.jsonl")
	d := &dbPath{e: env{home: t.TempDir()}, files: map[string]*dbFile{path: {path: path, name: "gone-session", session: "gone0000"}}}
	if _, err := d.findAfterLive("gone-session", nil, nil); err == nil || strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("gone file participated in name lookup: %v", err)
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
