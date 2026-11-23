package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEventTTLInSchema(t *testing.T) {
	var eventsDDL string
	for _, statement := range statements("synthetic") {
		if strings.Contains(statement, "CREATE TABLE IF NOT EXISTS synthetic.events") {
			eventsDDL = statement
			break
		}
	}
	for _, want := range []string{"TTL " + eventTTL(), "merge_with_ttl_timeout = 604800"} {
		if !strings.Contains(eventsDDL, want) {
			t.Fatalf("events DDL missing %q:\n%s", want, eventsDDL)
		}
	}
}

func TestCheckFileWithExpiredEvents(t *testing.T) {
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -eventRetentionDays-1).Format("2006-01-02T15:04:05.000Z")
	fresh := now.Add(-time.Hour).Format("2006-01-02T15:04:05.000Z")
	home := t.TempDir()
	path := claudePath(home)
	data := fmt.Appendf(nil,
		`{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":%q,"message":{"role":"user","content":"kept old user"}}`+"\n"+
			`{"type":"assistant","timestamp":%q,"message":{"id":"old","model":"m","content":[{"type":"text","text":"expired"}],"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n"+
			`{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":%q,"message":{"role":"user","content":"fresh"}}`+"\n", old, old, fresh)
	write(t, path, data)
	var out bytes.Buffer
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		switch {
		case strings.Contains(string(q), ".files FINAL"):
			fmt.Fprintf(w, "%s\t1\t%d\t3\t0\t3\tclaude\n", path, len(data))
		case strings.Contains(string(q), ".raw GROUP BY file_id"):
			io.WriteString(w, "1\t3\t3\t3\n")
		case strings.Contains(string(q), ".events") && strings.Contains(string(q), "GROUP BY file_id"):
			if !strings.Contains(string(q), eventRetentionPredicate("ts")) {
				t.Errorf("events count query lacks retention condition: %s", q)
			}
			io.WriteString(w, "1\t2\n")
		default:
			t.Errorf("unexpected query: %s", q)
			http.Error(w, "unexpected query", http.StatusBadRequest)
		}
	}))
	t.Cleanup(ts.Close)
	srv, err := newServer(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := &checker{srv: srv, db: "synthetic", home: home, out: &out}
	if ok, err := c.check(); err != nil || !ok {
		t.Fatalf("check with expired events: ok=%t err=%v\n%s", ok, err, out.String())
	}
}

func TestPurgePreservesExpiredUsage(t *testing.T) {
	h := newHarness(t)
	h.fake.mu.Lock()
	h.fake.rows["usage"] = [][]string{
		{"42", time.Now().AddDate(0, 0, -eventRetentionDays-1).Format("2006-01-02 15:04:05.000"), "s", "p", "m", "old", "r-old", "1", "2", "0", "0"},
		{"42", time.Now().Add(-time.Hour).Format("2006-01-02 15:04:05.000"), "s", "p", "m", "new", "r-new", "1", "2", "0", "0"},
	}
	h.fake.mu.Unlock()
	l := newLoader(config{home: h.home, db: "tq"}, h.srv, make(chan struct{}), &h.log, newStatus(""))
	f := &file{id: 42, gen: 1}
	if err := l.storeFiles([]*file{f}, true, []uint64{0}); err != nil {
		t.Fatal(err)
	}
	if err := l.purge(f, "synthetic test"); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	rows := h.fake.rows["usage"]
	if len(rows) != 1 || rows[0][5] != "old" {
		t.Fatalf("usage after purge: %v; want only expired row", rows)
	}
}
