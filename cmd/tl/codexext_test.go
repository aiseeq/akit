package main

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcriptdb"
)

// extRolloutRows — a Codex rollout outside worker: a user message and one priced token report.
func extRolloutRows(cwd, text string) []string {
	return []string{
		`{"timestamp":"2026-10-25T10:20:00Z","type":"session_meta","payload":{"id":"x","cwd":"` + cwd + `"}}`,
		`{"timestamp":"2026-10-25T10:20:01Z","type":"turn_context","payload":{"cwd":"` + cwd + `","model":"gpt-5"}}`,
		`{"timestamp":"2026-10-25T10:20:02Z","type":"event_msg","payload":{"type":"user_message","message":"` + text + `"}}`,
		`{"timestamp":"2026-10-25T10:20:03Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"output_tokens":700,"total_tokens":1700},"last_token_usage":{"input_tokens":1000,"output_tokens":700,"total_tokens":1700}}}}`,
	}
}

// A rollout under a Codex root outside worker is in no form of tl without --codex-ext, --codex included; with it
// the listing shows it and --usage counts it. The files and the database answer alike.
func TestCodexExtOnlyWithFlag(t *testing.T) {
	e, paths, _ := dbCorpus(t)
	ext := filepath.Join(e.home, ".codex", "sessions", "2026", "10", "25", "rollout-2026-10-25T10-20-00-0199bbbb-0000-7000-8000-000000000001.jsonl")
	writeWorkFile(t, ext, extRolloutRows("/work/extproj", "внешний запуск"))
	ch := newFakeCH(t, append(paths, ext), map[string]int{})
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	run := func(args ...string) string {
		t.Helper()
		e.backend = backendFiles
		codeF, outF, errF := runTL(t, e, args...)
		e.backend = backendDB
		codeD, outD, errD := runTL(t, e, args...)
		if strings.Contains(errD, "instead") {
			t.Errorf("%q: the database path fell back: %s", args, errD)
		}
		if codeF != codeD || outF != outD {
			t.Errorf("%q: files exit %d, db exit %d\nfiles:\n%s%s\ndb:\n%s%s", args, codeF, codeD, outF, errF, outD, errD)
		}
		return outF
	}
	for _, args := range [][]string{
		{"--all", "--role", "all", "--max", "0"},
		{"--all", "--codex", "--role", "all", "--max", "0"},
		{"--all", "--codex", "--grep", "внешний", "--max", "0"},
		{"--all", "--codex", "--project", "extproj", "--role", "all", "--max", "0"},
	} {
		if out := run(args...); strings.Contains(out, "внешний запуск") || strings.Contains(out, ".codex/sessions") {
			t.Errorf("%q shows the rollout outside worker:\n%s", args, out)
		}
	}
	if out := run("--all", "--codex-ext", "--role", "all", "--max", "0"); !strings.Contains(out, "внешний запуск") || !strings.Contains(out, "~/.codex/sessions/") {
		t.Errorf("--codex-ext does not show the rollout by its path:\n%s", out)
	}
	if out := run("--all", "--codex-ext", "--project", "extproj", "--role", "all", "--max", "0"); !strings.Contains(out, "внешний запуск") {
		t.Errorf("--codex-ext --project does not match the rollout's cwd:\n%s", out)
	}
	// a session id names worker rollouts and Claude sessions only; a rollout outside worker is named by its path
	for _, backend := range []string{backendFiles, backendDB} {
		e.backend = backend
		if code, out, _ := runTL(t, e, "0199bbbb", "--role", "all"); code == 0 || strings.Contains(out, "внешний запуск") {
			t.Errorf("%s: a session id finds the rollout outside worker: exit %d\n%s", backend, code, out)
		}
	}
	if out := run(ext, "--role", "all"); !strings.Contains(out, "внешний запуск") {
		t.Errorf("the path of the rollout outside worker does not read it:\n%s", out)
	}
	without := run("--all", "--codex", "--usage")
	with := run("--all", "--codex", "--codex-ext", "--usage")
	if strings.Contains(without, "rollout-2026-10-25T10-20-00") || !strings.Contains(with, "rollout-2026-10-25T10-20-00") ||
		!strings.Contains(with, "rollouts outside worker") {
		t.Errorf("--usage: without --codex-ext\n%s\nwith it\n%s", without, with)
	}
}

func TestCodexExtNeedsAll(t *testing.T) {
	if _, err := parseArgs([]string{"--codex-ext"}, fakeHome(t).now); err == nil || !strings.Contains(err.Error(), "--codex-ext") {
		t.Fatalf("--codex-ext without --all: %v", err)
	}
}

// The files query of --all leaves codex-ext rows on the server without --codex-ext, and the selection of the rows
// that come drops them too: either alone keeps them out of the answer.
func TestDBAllFilesLeaveCodexExtToFlag(t *testing.T) {
	now := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	ext := &dbFile{kind: transcriptdb.KindCodexExt, mtime: now}
	for _, on := range []bool{false, true} {
		d := &dbPath{opts: options{all: true, codexExt: on}, e: env{now: now}}
		if got := strings.Contains(d.allFilePredicate().sql, "kind != 'codex-ext'"); got == on {
			t.Errorf("--codex-ext %t: files query excludes codex-ext %t", on, got)
		}
		if got := d.selectedAll(ext, now.Add(-time.Hour)); got != on {
			t.Errorf("--codex-ext %t: codex-ext row selected %t", on, got)
		}
	}
}
