package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcriptdb"
)

const dbSession = "1234abcd-0000-0000-0000-000000000001"

// dbHome — журнал из трёх реплик одной сессии и её транскрипт: время каждой реплики в своей строке файла.
func dbHome(t *testing.T) (home, transcript string) {
	t.Helper()
	home = t.TempDir()
	transcript = filepath.Join(home, ".claude/projects/-p", dbSession+".jsonl")
	write(t, transcript,
		`{"type":"user","timestamp":"2026-10-05T08:07:09.400Z"}`+"\n"+
			`{"type":"user","timestamp":"2026-10-05T08:07:09.440Z","message":{"role":"user","content":"первый ключслово"}}`+"\n"+
			`{"type":"user","timestamp":"2026-10-06T08:00:00.000Z","message":{"role":"user","content":"второй ключслово"}}`+"\n"+
			`{"type":"user","timestamp":"2026-10-07T08:00:00.000Z","message":{"role":"user","content":"третий ключслово"}}`+"\n")
	write(t, filepath.Join(home, ".claude/user-replies.md"),
		"### 2026-10-05T08:07:09.44Z (05.10 10:07 CEST) · ~/x · claude "+dbSession+"\n  первый ключслово\n"+
			"### 2026-10-06T08:00:00Z (06.10 10:00 CEST) · ~/x · claude "+dbSession+"\n  второй ключслово\n"+
			"### 2026-10-07T08:00:00Z (07.10 10:00 CEST) · ~/x · claude "+dbSession+"\n  третий ключслово\n")
	return home, transcript
}

func dbClient(t *testing.T, url string) *transcriptdb.Client {
	t.Helper()
	c, err := transcriptdb.New(url, "tr")
	if err != nil {
		t.Fatal(err)
	}
	return c.WithTool("prior")
}

func replyHeads(out string) []string {
	var heads []string
	for _, l := range sectionOf(out, "реплики") {
		if strings.HasPrefix(l, "### ") {
			heads = append(heads, l)
		}
	}
	return heads
}

// База отвечает номером строки первой реплики (не тем, что в файле: видно, что ответ взят из базы); вторую она
// находит только на нижней границе окна - ряд мог начаться раньше, третьей в базе нет: обе ищутся в файле. Строка
// сервера о транскрипте, о котором не спрашивали, окна не даёт; прогрева строк журнала по файлам при базе нет.
func TestPriorReplyLinesFromDatabase(t *testing.T) {
	home, transcript := dbHome(t)
	first := time.Date(2026, 10, 5, 8, 7, 9, 440_000_000, time.UTC).UnixMilli()
	second := time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC).UnixMilli()
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("readonly") != "1" {
			t.Error("query not readonly")
		}
		body, _ := io.ReadAll(r.Body)
		name := r.URL.Query().Get("log_comment")
		queries = append(queries, name)
		switch name {
		case "prior:reply_events":
			if got := r.URL.Query().Get("param_paths"); got != transcriptdb.Strings([]string{transcript}) {
				t.Errorf("paths %s", got)
			}
			_, _ = fmt.Fprintf(w, "88\t/elsewhere/%s.jsonl\t%d\t3\n77\t%s\t%d\t9\n77\t%s\t%d\t300\n", dbSession, first, transcript, first, transcript, second)
		case "prior:reply_lines":
			if !strings.Contains(string(body), "file_id = {f0:UInt64}") || r.URL.Query().Get("param_f0") != "77" || r.URL.Query().Get("param_b1") != "300" ||
				r.URL.Query().Get("param_a1") != "44" || r.URL.Query().Has("param_f2") {
				t.Errorf("windows: %s %v", body, r.URL.Query())
			}
			_, _ = fmt.Fprint(w, "77\t5\t1,0\n77\t9\t1,0\n77\t44\t0,1\n")
		default:
			t.Errorf("unexpected query %s", name)
		}
	}))
	defer server.Close()
	var stderr bytes.Buffer
	activePriorDB = &priorDB{client: dbClient(t, server.URL), through: time.Now(), stderr: &stderr}
	defer func() { activePriorDB = nil }()
	got := replyHeads(runPrior(t, home, "ключслово"))
	if _, err := os.Stat(cachePath("reply-prewarm.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("reply lines prewarmed from the files with the database: %v", err)
	}
	want := []string{
		"### 2026-10-05T08:07:09.44Z (05.10 10:07 CEST) · ~/x · claude " + dbSession + " · tl 1234abcd:5",
		"### 2026-10-06T08:00:00Z (06.10 10:00 CEST) · ~/x · claude " + dbSession + " · tl 1234abcd:3",
		"### 2026-10-07T08:00:00Z (07.10 10:00 CEST) · ~/x · claude " + dbSession + " · tl 1234abcd:4",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("заголовки:\n%s", strings.Join(got, "\n"))
	}
	if strings.Join(queries, ",") != "prior:reply_events,prior:reply_lines" {
		t.Errorf("queries %v", queries)
	}
	if !strings.Contains(stderr.String(), "prior: данные актуальны на") {
		t.Errorf("freshness line missing: %q", stderr.String())
	}
}

// Сервер отвечает ошибкой - предупреждение в stderr и строки из файлов: выдача та же, что без базы.
func TestPriorDatabaseServerFailureFallsBackToFiles(t *testing.T) {
	home, _ := dbHome(t)
	want := runPrior(t, home, "ключслово")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "synthetic unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	var stderr bytes.Buffer
	activePriorDB = &priorDB{client: dbClient(t, server.URL), stderr: &stderr}
	defer func() { activePriorDB = nil }()
	got := runPrior(t, home, "ключслово")
	if got != want {
		t.Errorf("fallback output differs:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(stderr.String(), "reading files instead") || strings.Contains(stderr.String(), "данные актуальны") {
		t.Errorf("stderr %q", stderr.String())
	}
	if activePriorDB != nil {
		t.Error("backend kept after a failed query")
	}
}

// Запуск на базе без находок в разделе реплик: запросов к базе нет, а строка свежести есть, как у tl.
func TestPriorDatabaseNoRepliesPrintsFreshness(t *testing.T) {
	home, _ := dbHome(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the database is asked without reply hits")
		http.Error(w, "synthetic", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	var stderr bytes.Buffer
	activePriorDB = &priorDB{client: dbClient(t, server.URL), through: time.Now(), stderr: &stderr}
	defer func() { activePriorDB = nil }()
	if got := sectionOf(runPrior(t, home, "несуществующееслово"), "реплики"); len(got) == 0 || got[len(got)-1] != "(нет)" {
		t.Errorf("replies section %q", got)
	}
	if strings.Count(stderr.String(), "prior: данные актуальны на") != 1 {
		t.Errorf("freshness line: %q", stderr.String())
	}
}

func TestPriorBackendHealth(t *testing.T) {
	home, stateHome := t.TempDir(), t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("TL_BACKEND", "db")
	t.Setenv("TL_CLICKHOUSE_URL", "http://127.0.0.1:9")
	stateDir := filepath.Join(stateHome, "akit", "tlfeed")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeState := func(heartbeat time.Time) {
		state, err := json.Marshal(transcriptdb.State{Heartbeat: heartbeat.Format(time.RFC3339Nano), DBThrough: time.Now(), Home: home})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "state.json"), state, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var stderr bytes.Buffer
	writeState(time.Now())
	if priorBackend(home, &stderr) == nil {
		t.Fatalf("healthy state not accepted: %s", stderr.String())
	}
	writeState(time.Now().Add(-time.Hour))
	if got := priorBackend(home, &stderr); got != nil {
		t.Fatal("stale loader state accepted")
	}
	if !strings.Contains(stderr.String(), "reading files instead") {
		t.Fatalf("fallback warning missing: %s", stderr.String())
	}
	t.Setenv("TL_BACKEND", "files")
	stderr.Reset()
	if priorBackend(home, &stderr) != nil || stderr.Len() > 0 {
		t.Fatalf("files backend opened the database: %q", stderr.String())
	}
	t.Setenv("TL_BACKEND", "dbx")
	if priorBackend(home, &stderr) != nil || !strings.Contains(stderr.String(), `prior: TL_BACKEND="dbx" is not files or db; reading files`) {
		t.Fatalf("unknown backend: %q", stderr.String())
	}
}
