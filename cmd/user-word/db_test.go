package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/internal/transcriptdb"
)

// healthyLoader — state.json свежего загрузчика для home.
func healthyLoader(t *testing.T, home string, heartbeat time.Time) {
	t.Helper()
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	stateDir := filepath.Join(stateHome, "akit", "tlfeed")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(transcriptdb.State{Heartbeat: heartbeat.Format(time.RFC3339Nano), DBThrough: time.Now(), Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeDB — фейк сервера: answer по телу запроса возвращает строки TabSeparated.
func fakeDB(t *testing.T, answer func(sql string, params map[string]string) string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("readonly") != "1" {
			t.Error("query not readonly")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		params := map[string]string{}
		for name, values := range r.URL.Query() {
			if after, ok := strings.CutPrefix(name, "param_"); ok {
				params[after] = values[0]
			}
		}
		_, _ = fmt.Fprint(w, answer(string(body), params))
	}))
	t.Cleanup(server.Close)
	t.Setenv("TL_CLICKHOUSE_URL", server.URL)
	t.Setenv("TL_BACKEND", "db")
}

func TestUserWordDatabaseLinesAndSearch(t *testing.T) {
	defer func() { activeWordDB = nil }()
	e := testEnv(t)
	session := writeTranscript(t, e.home, "-home-x-proj", "e5e5f6f6-0000", human("2026-10-05T08:07:09Z", "on disk"))
	gone := filepath.Join(filepath.Dir(session), "gone.jsonl")
	healthyLoader(t, e.home, time.Now())
	fakeDB(t, func(sql string, params map[string]string) string {
		switch {
		case strings.HasPrefix(sql, "SELECT toString(events.line)"):
			if params["path"] != session {
				return ""
			}
			return "7\t1791187629440\tuser\talpha decision\n8\t1791187689440\tagent\tcontext response\n"
		case strings.HasPrefix(sql, "SELECT files.path, toString(events.line)"):
			if params["terms"] != "['lpha']" {
				t.Errorf("terms %q", params["terms"])
			}
			// Файла нет на диске, суб-агент и сессия не из папки проектов - не находки, как у transcripts.
			return gone + "\t5\t1791187629440\tuser\talpha decision\n" +
				filepath.Join(filepath.Dir(session), "e5e5f6f6-0000", "subagents", "agent-1.jsonl") + "\t6\t1791187629440\tuser\talpha decision\n" +
				"/elsewhere/x.jsonl\t6\t1791187629440\tuser\talpha decision\n" +
				session + "\t7\t1791187629440\tuser\talpha decision\n"
		}
		t.Errorf("unexpected query %s", sql)
		return ""
	})
	var stderr bytes.Buffer
	initWordDB(e, &stderr)
	lines, err := readLines(session, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || !lines[0].user || lines[0].no != 7 || lines[0].text != "alpha decision" || lines[1].text != "context response" {
		t.Fatalf("database lines: %#v", lines)
	}
	hits, _, err := dbFindHits(options{frag: "alpha"}, e)
	if err != nil || len(hits) != 1 || hits[0].no != 7 || hits[0].path != session {
		t.Fatalf("database --search hits: %#v, %v", hits, err)
	}
	if strings.Count(stderr.String(), "данные актуальны на") != 1 {
		t.Fatalf("freshness line: %q", stderr.String())
	}
	// Транскрипта нет в базе - он читается из файла, с предупреждением.
	if _, err := readLines(filepath.Join(e.home, "missing.jsonl"), nil); err == nil {
		t.Fatal("a missing file read without error")
	}
	if activeWordDB != nil || !strings.Contains(stderr.String(), "reading files instead") {
		t.Fatalf("no fallback for a transcript missing in the database: %q", stderr.String())
	}
}

// --brief через базу: с диска читается только транскрипт, который назвала база (во втором та же реплика, и через
// файлы слово неоднозначно), а заметка о дне без реплики берёт наличие записей из базы.
func TestUserWordBriefReadsOnlyNamedTranscripts(t *testing.T) {
	defer func() { activeWordDB = nil }()
	e := testEnv(t)
	named := writeTranscript(t, e.home, "-home-x-proj", "a1a1b2b2-0000",
		agent("2026-10-31T09:58:00Z", "Что с кэшем?"),
		human("2026-10-31T09:59:00Z", "оставь кэш сборки"))
	writeTranscript(t, e.home, "-home-x-proj", "c3c3d4d4-0000",
		human("2026-10-31T10:30:00Z", "оставь кэш сборки"))
	b := writeBrief(t, t.TempDir(), "Кэш (пользователь 31.10: «оставь кэш сборки»).\nПрочее (пользователь 30.10: «чего не было»).\n")
	t.Setenv("TL_BACKEND", "files")
	_, files, _ := runUW(t, e, "--brief", b, "--until", "2026-10-31 12:00")
	if !strings.Contains(files, "ambiguous: 2 lines") || !strings.Contains(files, "no transcript records on marker day") {
		t.Fatalf("files output:\n%s", files)
	}
	healthyLoader(t, e.home, time.Now())
	fakeDB(t, func(sql string, params map[string]string) string {
		switch {
		case strings.HasPrefix(sql, "SELECT DISTINCT files.path"):
			if params["s1"] != "2026-10-31 00:00:00.000" || !strings.Contains(params["t1"], "'ставь'") {
				t.Errorf("candidate params %v", params)
			}
			return named + "\n/elsewhere/not-a-session.jsonl\n"
		case strings.Contains(sql, "countIf"):
			return "1\t1\n"
		}
		t.Errorf("unexpected query %s", sql)
		return ""
	})
	code, out, errOut := runUW(t, e, "--brief", b, "--until", "2026-10-31 12:00")
	if code != exitNone || strings.Contains(out, "ambiguous:") || !strings.Contains(out, "оставь кэш сборки") ||
		!strings.Contains(out, "no matching user line on marker day") {
		t.Fatalf("database output (code %d):\n%s", code, out)
	}
	if !strings.Contains(errOut, "данные актуальны на") || strings.Contains(errOut, "reading files instead") {
		t.Fatalf("stderr %q", errOut)
	}
}

func TestUserWordStaleLoaderReadsFiles(t *testing.T) {
	defer func() { activeWordDB = nil }()
	e := testEnv(t)
	healthyLoader(t, e.home, time.Now().Add(-time.Hour))
	t.Setenv("TL_BACKEND", "db")
	var stderr bytes.Buffer
	initWordDB(e, &stderr)
	if activeWordDB != nil || !strings.Contains(stderr.String(), "reading files instead") {
		t.Fatalf("stale state fallback: active=%v stderr=%q", activeWordDB, stderr.String())
	}
}

func TestUserWordDatabaseServerFailureFallsBackToFiles(t *testing.T) {
	e := testEnv(t)
	path := writeTranscript(t, e.home, "-home-x-synthetic", "fallback", human("2026-10-05T08:07:09Z", "fallback phrase"))
	healthyLoader(t, e.home, time.Now())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "synthetic unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	t.Setenv("TL_CLICKHOUSE_URL", server.URL)
	t.Setenv("TL_BACKEND", "db")
	opts, err := parseArgs([]string{path, "fallback phrase"})
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := run(opts, e, &out, &stderr); code != 0 || !strings.Contains(out.String(), "fallback phrase") || !strings.Contains(stderr.String(), "reading files instead") {
		t.Fatalf("fallback=(%d,%q,%q)", code, out.String(), stderr.String())
	}
}
