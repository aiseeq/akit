package main

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// lossy passes requests to a server; the next lose inserts into raw are applied there, but the loader gets an
// error instead of the answer.
type lossy struct {
	proxy *httputil.ReverseProxy
	lose  atomic.Int32
}

func (p *lossy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Query().Get("query"), ".raw (") && p.lose.Add(-1) >= 0 {
		rec := httptest.NewRecorder()
		p.proxy.ServeHTTP(rec, r)
		http.Error(w, "answer lost", http.StatusBadGateway)
		return
	}
	p.proxy.ServeHTTP(w, r)
}

// liveServer — a ClickHouse server for the integration tests (AKIT_CLICKHOUSE_TEST=1, AKIT_CLICKHOUSE_URL or
// the default port) and a fresh database tlfeed_test_<random>, dropped after the test.
func liveServer(t *testing.T) (*server, string) {
	t.Helper()
	if os.Getenv("AKIT_CLICKHOUSE_TEST") != "1" {
		t.Skip("set AKIT_CLICKHOUSE_TEST=1 to run against a local ClickHouse server")
	}
	addr := os.Getenv("AKIT_CLICKHOUSE_URL")
	if addr == "" {
		addr = "http://127.0.0.1:18123"
	}
	srv, err := newServer(addr)
	if err != nil {
		t.Fatal(err)
	}
	db := fmt.Sprintf("tlfeed_test_%08x", rand.Uint32())
	t.Cleanup(func() {
		if _, err := srv.query(context.Background(), "DROP DATABASE IF EXISTS "+db+" SYNC", time.Minute); err != nil {
			t.Errorf("drop %s: %v", db, err)
		}
	})
	return srv, db
}

func (h *harness) liveLoader(srv *server, db string) *loader {
	h.t.Helper()
	cfg := config{home: h.home, db: db, tick: time.Second, idle: time.Hour, sweep: time.Second, rescan: time.Hour, batch: 32 << 20}
	l := newLoader(cfg, srv, make(chan struct{}), &h.log, newStatus(""))
	l.dirs.w = noWatch{}
	paths, err := l.start()
	if err != nil {
		h.t.Fatal(err)
	}
	if err := l.visitAll(paths); err != nil {
		h.t.Fatal(err)
	}
	return l
}

func scalar(t *testing.T, srv *server, q string) string {
	t.Helper()
	out, err := srv.query(context.Background(), q, time.Minute)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return strings.TrimSpace(string(out))
}

// verifyAll: every file rebuilds from raw byte for byte, and check finds nothing.
func verifyAll(t *testing.T, srv *server, db, home string) {
	t.Helper()
	var out bytes.Buffer
	c := &checker{srv: srv, db: db, home: home, out: &out}
	if ok, err := c.verify(100); err != nil || !ok || !strings.Contains(out.String(), "diff=0 gone=0") {
		t.Fatalf("verify: %v %s", err, out.String())
	}
	out.Reset()
	c = &checker{srv: srv, db: db, home: home, out: &out}
	if ok, err := c.check(); err != nil || !ok {
		t.Fatalf("check: %v %s", err, out.String())
	}
}

// Against a live server: files rebuild from raw byte for byte and their events equal transcript.Read, through
// growth mid-line, a repeated insert, a rewind and a rename; the views fill replies and usage.
func TestLiveServer(t *testing.T) {
	direct, db := liveServer(t)
	target, _ := url.Parse(direct.base)
	loss := &lossy{proxy: httputil.NewSingleHostReverseProxy(target)}
	ts := httptest.NewServer(loss)
	t.Cleanup(ts.Close)
	srv, err := newServer(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, home: t.TempDir()}
	claude := sample(t, "claude.jsonl")
	codex := sample(t, "codex.jsonl")
	path := claudePath(h.home)
	sub := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "agent-a1b2c3d4e5.jsonl")
	rollout := filepath.Join(h.home, ".local", "state", "worker", "job1", "codex", "sessions", "2026", "10", "25",
		"rollout-2026-10-25T10-47-55-01a172ac-0000-7000-8000-000000000001.jsonl")
	half := len(claude) / 2
	write(t, path, claude[:half])
	write(t, sub, claude)
	write(t, rollout, codex)
	l := h.liveLoader(srv, db)
	verifyAll(t, srv, db, h.home)
	if n := scalar(t, srv, "SELECT count() FROM "+db+".raw WHERE loaded_at > now64(3) - 600"); n == "0" {
		t.Fatal("raw rows without loaded_at")
	}

	// growth whose raw insert is applied but its answer lost twice: the loader sends it again with the same
	// token, the server drops the repeats
	appendTo(t, path, claude[half:])
	loss.lose.Store(2)
	if err := l.pass(false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.log.String(), "answer lost") {
		t.Fatalf("no lost answer: %s", h.log.String())
	}
	verifyAll(t, srv, db, h.home)
	f := l.files[path]

	// rewind: cut back and grown past the old size with other lines
	lines := bytes.SplitAfter(claude, []byte{'\n'})
	other := bytes.Join(lines[:4], nil)
	for len(other) <= len(claude) {
		other = append(other, lines[3]...)
	}
	write(t, path, other)
	if err := l.pass(false); err != nil {
		t.Fatal(err)
	}
	verifyAll(t, srv, db, h.home)
	if f.gen == 0 {
		t.Fatalf("gen %d after rewind", f.gen)
	}

	// rename and growth: the same id, the new path, no line twice
	moved := filepath.Join(filepath.Dir(path), "99999999-2222-4333-8444-555555555555.jsonl")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	appendTo(t, moved, lines[2])
	if err := l.pass(false); err != nil {
		t.Fatal(err)
	}
	verifyAll(t, srv, db, h.home)
	if l.files[moved] != f {
		t.Fatal("renamed file got a new id")
	}
	if got := scalar(t, srv, fmt.Sprintf("SELECT path FROM %s.files FINAL WHERE file_id = %d", db, f.id)); got != moved {
		t.Fatalf("files path %q", got)
	}
	if got := scalar(t, srv, fmt.Sprintf("SELECT count() - uniqExact(line) FROM %s.raw WHERE file_id = %d", db, f.id)); got != "0" {
		t.Fatalf("%s lines twice", got)
	}
	if got := scalar(t, srv, "SELECT count() FROM "+db+".replies"); got == "0" {
		t.Fatal("replies empty")
	}

	// a restart loads nothing again
	inserted := scalar(t, srv, "SELECT count() FROM "+db+".raw")
	h.liveLoader(srv, db)
	if got := scalar(t, srv, "SELECT count() FROM "+db+".raw"); got != inserted {
		t.Fatalf("restart inserted again: %s rows, was %s", got, inserted)
	}
	var out bytes.Buffer
	if err := (&checker{srv: srv, db: db, out: &out}).materialize(); err != nil {
		t.Fatal(err)
	}
}

// replyLine — an assistant reply with its usage, as Claude Code writes it.
func replyLine(id string, sec int) string {
	return fmt.Sprintf(`{"type":"assistant","requestId":"r%s","timestamp":"2026-10-25T10:00:%02dZ","message":{"id":"%s","model":"m1",`+
		`"content":[{"type":"text","text":"reply %s"}],"usage":{"input_tokens":5,"output_tokens":9}}}`+"\n", id, sec, id, id)
}

// Against a live server: a rewind that drops replies drops their usage too, and a rename without growth moves
// the files row to the new path, so --check finds the file.
func TestLiveUsageAndRename(t *testing.T) {
	srv, db := liveServer(t)
	h := &harness{t: t, home: t.TempDir()}
	path := claudePath(h.home)
	user := `{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-10-25T10:00:00.000Z","message":{"role":"user","content":"hi"}}` + "\n"
	write(t, path, []byte(user+replyLine("m1", 1)+replyLine("m2", 2)+replyLine("m3", 3)))
	l := h.liveLoader(srv, db)
	q := func(s string) string { return scalar(t, srv, fmt.Sprintf(s, db)) }
	if got := q("SELECT sum(out_tok) FROM %s.usage FINAL"); got != "27" {
		t.Fatalf("usage out_tok %s, want 27", got)
	}
	write(t, path, []byte(user+replyLine("m1", 1)+replyLine("m4", 4)))
	if err := l.pass(false); err != nil {
		t.Fatal(err)
	}
	if got := q("SELECT arrayStringConcat(arraySort(groupArray(reply_id)), ',') FROM %s.usage FINAL"); got != "m1,m4" {
		t.Fatalf("usage after rewind: %s, want m1,m4", got)
	}
	verifyAll(t, srv, db, h.home)

	f := l.files[path]
	moved := filepath.Join(filepath.Dir(path), "99999999-2222-4333-8444-555555555555.jsonl")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := l.pass(false); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, srv, fmt.Sprintf("SELECT path FROM %s.files FINAL WHERE file_id = %d", db, f.id)); got != moved {
		t.Fatalf("files path %s after a rename without growth", got)
	}
	verifyAll(t, srv, db, h.home)
}

// filesGate passes requests to a server and refuses the inserts into files after the first `allow` of them while
// it is on.
type filesGate struct {
	proxy        *httputil.ReverseProxy
	on           atomic.Bool
	allow, seen  atomic.Int32
	refusedFiles atomic.Int32
}

func (p *filesGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Query().Get("query"), ".files (") && p.on.Load() && p.seen.Add(1) > p.allow.Load() {
		p.refusedFiles.Add(1)
		http.Error(w, "files refused", http.StatusBadGateway)
		return
	}
	p.proxy.ServeHTTP(w, r)
}

// Against a live server: a new file whose raw and events are in, the loader stopped before its clean files row;
// the next start, with the file grown and another new file in the same pass, rebuilds both byte for byte.
func TestLiveStopBeforeFilesRow(t *testing.T) {
	direct, db := liveServer(t)
	target, _ := url.Parse(direct.base)
	gate := &filesGate{proxy: httputil.NewSingleHostReverseProxy(target)}
	gate.allow.Store(1) // the dirty row goes
	ts := httptest.NewServer(gate)
	t.Cleanup(ts.Close)
	srv, err := newServer(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, home: t.TempDir()}
	data := sample(t, "claude.jsonl")
	path := claudePath(h.home)
	half := bytes.Index(data, []byte(`"parentUuid":"d"`)) - 1
	write(t, path, data[:half])
	gate.on.Store(true)
	stop := make(chan struct{})
	go func() {
		for gate.refusedFiles.Load() < 1 {
			time.Sleep(time.Millisecond)
		}
		close(stop)
	}()
	cfg := config{home: h.home, db: db, tick: time.Second, idle: time.Hour, sweep: time.Second, rescan: time.Hour, batch: 32 << 20}
	l := newLoader(cfg, srv, stop, &h.log, newStatus(""))
	l.dirs.w = noWatch{}
	l.backoff = time.Millisecond
	paths, err := l.start()
	if err != nil {
		t.Fatal(err)
	}
	if err := l.visitAll(paths); err != errStopped {
		t.Fatalf("first run: %v", err)
	}
	if n := scalar(t, direct, "SELECT count() FROM "+db+".raw"); n == "0" {
		t.Fatal("the first run sent no raw rows")
	}
	gate.on.Store(false)
	appendTo(t, path, data[half:])
	write(t, filepath.Join(filepath.Dir(path), "22222222-2222-4333-8444-555555555555.jsonl"), data)
	h.liveLoader(direct, db)
	verifyAll(t, direct, db, h.home)
	if got := scalar(t, direct, "SELECT count() - uniqExact(file_id, line) FROM "+db+".raw"); got != "0" {
		t.Fatalf("%s raw lines twice", got)
	}
}

// Against a live server: a raw line stored twice is a problem --check reports.
func TestLiveCheckFindsDuplicateRaw(t *testing.T) {
	srv, db := liveServer(t)
	h := &harness{t: t, home: t.TempDir()}
	write(t, claudePath(h.home), sample(t, "claude.jsonl"))
	h.liveLoader(srv, db)
	verifyAll(t, srv, db, h.home)
	scalar(t, srv, fmt.Sprintf("INSERT INTO %s.raw SELECT * FROM %s.raw WHERE line = 2", db, db))
	var out bytes.Buffer
	ok, err := (&checker{srv: srv, db: db, home: h.home, out: &out}).check()
	if err != nil || ok || !strings.Contains(out.String(), "a line is there twice") {
		t.Fatalf("check with a raw line twice: ok %t, %v, %s", ok, err, out.String())
	}
}
