package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
