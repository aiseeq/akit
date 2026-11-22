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
	if f.gen != 1 {
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
