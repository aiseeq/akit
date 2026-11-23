package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const literalEventTTL = "toDateTime(ts) + INTERVAL 90 DAY DELETE WHERE NOT (kind = 'claude' AND role IN ('user', 'ask'))"

func TestEventTTLInSchema(t *testing.T) {
	var eventsDDL string
	for _, statement := range statements("synthetic") {
		if strings.Contains(statement, "CREATE TABLE IF NOT EXISTS synthetic.events") {
			eventsDDL = statement
			break
		}
	}
	for _, want := range []string{"TTL " + literalEventTTL, "merge_with_ttl_timeout = 604800"} {
		if !strings.Contains(eventsDDL, want) {
			t.Fatalf("events DDL missing %q:\n%s", want, eventsDDL)
		}
	}
}

func TestRetentionSQL(t *testing.T) {
	if got, want := eventTTL(), literalEventTTL; got != want {
		t.Fatalf("event TTL %q, want %q", got, want)
	}
	if got, want := eventRetentionPredicate("ts"), "(toDateTime(ts) + INTERVAL 90 DAY > now() OR (kind = 'claude' AND role IN ('user', 'ask')))"; got != want {
		t.Fatalf("retention predicate %q, want %q", got, want)
	}
	at := time.Date(2026, 11, 23, 12, 34, 56, 0, time.UTC)
	if got, want := eventRetentionPredicateAt("ts", at), "(toDateTime(ts) + INTERVAL 90 DAY > toDateTime('2026-11-23 12:34:56', 'UTC') OR (kind = 'claude' AND role IN ('user', 'ask')))"; got != want {
		t.Fatalf("fixed retention predicate %q, want %q", got, want)
	}
	if got, want := eventWithinRetentionAt("ts", at, eventUsageRebuildDays), "toDateTime(ts) + INTERVAL 89 DAY > toDateTime('2026-11-23 12:34:56', 'UTC')"; got != want {
		t.Fatalf("usage rebuild predicate %q, want %q", got, want)
	}
}

func TestEventRetainedBoundaries(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	cut := now.Add(-90 * 24 * time.Hour)
	kinds := []string{"claude", "sub", "codex"}
	roles := []string{"user", "ask", "peer", "agent", "think", "cmd", "out", "attach", "note"}
	offsets := []time.Duration{-time.Second, -time.Millisecond, 0, time.Millisecond, time.Second}
	for _, kind := range kinds {
		for _, role := range roles {
			for _, offset := range offsets {
				at := cut.Add(offset)
				want := at.Truncate(time.Second).Add(90*24*time.Hour).After(now) || kind == "claude" && (role == "user" || role == "ask")
				if got := eventRetained(at, kind, role, now); got != want {
					t.Errorf("eventRetained(%s, %s, %s) = %t, want %t", at.Format(time.RFC3339Nano), kind, role, got, want)
				}
			}
		}
	}
}

type retentionEvent struct {
	at         time.Time
	kind, role string
}

// retainedByRetentionSQL evaluates the supported retention SQL fragment, including its interval, comparison,
// boolean operator, and exception list. The fake server uses this instead of duplicating the Go retention rule.
var (
	retentionComparisonRE = regexp.MustCompile(`toDateTime\(ts\) \+ INTERVAL (\d+) DAY (>|<=) (?:toDateTime\('([^']+)', 'UTC'\)|now\(\))`)
	retentionExceptionRE  = regexp.MustCompile(`kind = '([^']+)' AND role IN \(([^)]*)\)`)
	retentionRoleRE       = regexp.MustCompile(`'([^']+)'`)
)

func retainedByRetentionSQL(query string, at time.Time, kind, role string) (bool, error) {
	m := retentionComparisonRE.FindStringSubmatch(query)
	if m == nil {
		return false, fmt.Errorf("no supported retention comparison in %q", query)
	}
	days, _ := strconv.Atoi(m[1])
	cutoff := time.Now().UTC().Truncate(time.Second)
	if m[3] != "" {
		var err error
		cutoff, err = time.ParseInLocation(time.DateTime, m[3], time.UTC)
		if err != nil {
			return false, err
		}
	}
	boundary := at.UTC().Truncate(time.Second).Add(time.Duration(days) * 24 * time.Hour)
	within := boundary.After(cutoff)
	if m[2] == "<=" {
		within = !within
	}
	ex := retentionExceptionRE.FindStringSubmatch(query)
	if ex == nil {
		return within, nil
	}
	keptRole := false
	for _, r := range retentionRoleRE.FindAllStringSubmatch(ex[2], -1) {
		keptRole = keptRole || role == r[1]
	}
	kept := kind == ex[1] && keptRole
	switch {
	case strings.Contains(query, " OR "):
		return within || kept, nil
	case strings.Contains(query, " AND "):
		return within && kept, nil
	default:
		return false, fmt.Errorf("no boolean operator for TTL exception in %q", query)
	}
}

func countRetainedSQL(query string, events []retentionEvent) (int, error) {
	n := 0
	for _, e := range events {
		kept, err := retainedByRetentionSQL(query, e.at, e.kind, e.role)
		if err != nil {
			return 0, err
		}
		if kept {
			n++
		}
	}
	return n, nil
}

func checkHarness(t *testing.T, home, path string, data []byte, events []retentionEvent, delay time.Duration) (*checker, *httptest.Server) {
	t.Helper()
	var out bytes.Buffer
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		switch {
		case strings.Contains(string(q), ".files FINAL"):
			fmt.Fprintf(w, "%s\t1\t%d\t%d\t0\t%d\tclaude\n", path, len(data), bytes.Count(data, []byte{'\n'}), bytes.Count(data, []byte{'\n'}))
		case strings.Contains(string(q), ".raw GROUP BY file_id"):
			n := bytes.Count(data, []byte{'\n'})
			fmt.Fprintf(w, "1\t%d\t%d\t%d\n", n, n, n)
		case strings.Contains(string(q), ".events") && strings.Contains(string(q), "GROUP BY file_id"):
			count, err := countRetainedSQL(string(q), events)
			if err != nil {
				t.Errorf("events query is not understood by synthetic server: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if delay > 0 {
				time.Sleep(delay)
			}
			fmt.Fprintf(w, "1\t%d\n", count)
		default:
			t.Errorf("unexpected query: %s", q)
			http.Error(w, "unexpected query", http.StatusBadRequest)
		}
	}))
	srv, err := newServer(ts.URL)
	if err != nil {
		ts.Close()
		t.Fatal(err)
	}
	return &checker{srv: srv, db: "synthetic", home: home, out: &out}, ts
}

func TestCheckFileWithExpiredEvents(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	old := base.Add(-91 * 24 * time.Hour)
	fresh := base.Add(-time.Hour)
	oldText := old.Format("2006-01-02T15:04:05.000Z")
	freshText := fresh.Format("2006-01-02T15:04:05.000Z")
	home := t.TempDir()
	path := claudePath(home)
	data := fmt.Appendf(nil,
		`{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":%q,"message":{"role":"user","content":"kept old user"}}`+"\n"+
			`{"type":"assistant","timestamp":%q,"message":{"id":"old","model":"m","content":[{"type":"text","text":"expired"}],"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n"+
			`{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":%q,"message":{"role":"user","content":"fresh"}}`+"\n", oldText, oldText, freshText)
	write(t, path, data)
	events := []retentionEvent{{old, "claude", "user"}, {old, "claude", "agent"}, {fresh, "claude", "user"}}
	c, ts := checkHarness(t, home, path, data, events, 0)
	defer ts.Close()
	if ok, err := c.check(); err != nil || !ok {
		t.Fatalf("check with expired events: ok=%t err=%v\n%s", ok, err, c.out)
	}
}

func TestReviewCheckBoundaryRace(t *testing.T) {
	cutoff := time.Now().UTC().Truncate(time.Second)
	crossing := cutoff.Add(-90*24*time.Hour + 1500*time.Millisecond)
	home := t.TempDir()
	path := claudePath(home)
	data := fmt.Appendf(nil,
		`{"type":"assistant","timestamp":%q,"message":{"id":"x","model":"m","content":[{"type":"text","text":"about to expire"}],"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n", crossing.Format("2006-01-02T15:04:05.000Z"))
	write(t, path, data)
	c, ts := checkHarness(t, home, path, data, []retentionEvent{{crossing, "claude", "agent"}}, 2*time.Second)
	defer ts.Close()
	c.now = func() time.Time { return cutoff }
	if ok, err := c.check(); err != nil || !ok {
		t.Fatalf("check raced retention boundary: ok=%t err=%v\n%s", ok, err, c.out)
	}
}

func TestCheckUsesTranscriptKind(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	old := now.Add(-91 * 24 * time.Hour)
	path := t.TempDir() + "/session.jsonl"
	data := fmt.Appendf(nil,
		`{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":%q,"message":{"role":"user","content":"old sub user"}}`+"\n", old.Format("2006-01-02T15:04:05.000Z"))
	write(t, path, data)
	c := &checker{out: io.Discard}
	r := []string{path, "1", strconv.Itoa(len(data)), "1", "0", "1", "sub"}
	if err := c.checkFile(path, r, [2]string{"1", "1"}, "0", now); err != nil {
		t.Fatal(err)
	}
	if c.bad != 0 {
		t.Fatal("old user event of a sub file was counted as retained")
	}
}

func TestPurgePreservesExpiredUsageAndRebuildsRecentUsage(t *testing.T) {
	h := newHarness(t)
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	usageRow := func(at time.Time, reply string, out string) []string {
		return []string{"42", at.Format("2006-01-02 15:04:05.000"), "s", "p", "m", reply, "r-" + reply, "1", out, "0", "0"}
	}
	eventRow := func(at time.Time, reply, out string) []string {
		row := make([]string, 20)
		row[0], row[1], row[2], row[3], row[4] = at.Format("2006-01-02 15:04:05.000"), "42", "s", "claude", "p"
		row[5], row[6], row[12], row[13], row[14] = "1", "agent", "m", reply, "r-"+reply
		row[15], row[16], row[17], row[18] = "1", out, "0", "0"
		return row
	}
	oldAt, staleAt, recentAt, ttlLingeringAt := now.Add(-91*24*time.Hour), now.Add(-88*24*time.Hour), now.Add(-10*24*time.Hour), now.Add(-90*24*time.Hour-23*time.Hour)
	h.fake.mu.Lock()
	h.fake.rows["usage"] = [][]string{
		usageRow(oldAt, "old", "3"),
		usageRow(staleAt, "stale-88", "999"),
		usageRow(recentAt, "recent", "999"),
		usageRow(ttlLingeringAt, "ttl-lingering", "4"),
	}
	h.fake.rows["events"] = [][]string{
		eventRow(staleAt, "stale-88", "2"),
		eventRow(recentAt, "recent", "2"),
		eventRow(ttlLingeringAt, "ttl-lingering", "5"),
	}
	h.fake.mu.Unlock()
	l := newLoader(config{home: h.home, db: "tq"}, h.srv, make(chan struct{}), &h.log, newStatus(""))
	l.now = func() time.Time { return now }
	f := &file{id: 42, gen: 1, lines: 1, evLines: 1}
	if err := l.storeFiles([]*file{f}, true, []uint64{0}); err != nil {
		t.Fatal(err)
	}
	if err := l.purge(f, "synthetic test", false); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	rows := append([][]string(nil), h.fake.rows["usage"]...)
	h.fake.mu.Unlock()
	got := map[string][]string{}
	counts := map[string]int{}
	for _, row := range rows {
		got[row[5]] = row
		counts[row[5]]++
	}
	if len(rows) != 4 || len(got) != 4 || got["old"] == nil || got["stale-88"] == nil || got["recent"] == nil || got["ttl-lingering"] == nil || counts["ttl-lingering"] != 1 {
		t.Fatalf("usage after purge: %v", got)
	}
	if got["stale-88"][8] != "2" || got["recent"][8] != "2" {
		t.Fatalf("recent usage was not rebuilt: %v", got)
	}
	if got["ttl-lingering"][8] != "4" {
		t.Fatalf("usage whose event is past TTL boundary changed: %v", got["ttl-lingering"])
	}
	var rebuilt bool
	for _, sent := range h.fake.sentTo("usage") {
		if strings.Contains(sent.query, "SELECT ") {
			rebuilt = true
			if !strings.Contains(sent.query, "INTERVAL 89 DAY") {
				t.Errorf("usage rebuild lacks the 89-day safety margin: %s", sent.query)
			}
			if !strings.Contains(sent.query, eventWithinRetentionAt("ts", now, eventUsageRebuildDays)) {
				t.Errorf("usage rebuild lacks its retention predicate: %s", sent.query)
			}
		}
	}
	if !rebuilt {
		t.Fatal("purge did not rebuild usage from events")
	}
}

func TestPurgeClearsUsageAfterRawRewrite(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	h.fake.mu.Lock()
	h.fake.rows["usage"] = [][]string{{"42", now.Add(-100 * 24 * time.Hour).Format("2006-01-02 15:04:05.000"), "s", "p", "m", "removed", "r", "1", "2", "0", "0"}}
	h.fake.rows["events"] = nil
	h.fake.mu.Unlock()
	l := newLoader(config{home: h.home, db: "tq"}, h.srv, make(chan struct{}), &h.log, newStatus(""))
	f := &file{id: 42, gen: 1, lines: 1, evLines: 1}
	if err := l.storeFiles([]*file{f}, true, []uint64{0}); err != nil {
		t.Fatal(err)
	}
	if err := l.purge(f, "rewind", true); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if len(h.fake.rows["usage"]) != 0 {
		t.Fatalf("rewritten file retained stale usage: %v", h.fake.rows["usage"])
	}
}

func (f *fakeCH) usageFromEvents(query string) ([][]string, error) {
	m := regexp.MustCompile(`FROM \w+\.events WHERE file_id = (\d+)`).FindStringSubmatch(query)
	if m == nil {
		return nil, fmt.Errorf("unrecognized usage rebuild: %s", query)
	}
	id := m[1]
	var out [][]string
	for _, event := range f.rows["events"] {
		if event[1] != id || event[13] == "" {
			continue
		}
		at, err := time.Parse("2006-01-02 15:04:05.000", event[0])
		if err != nil {
			return nil, err
		}
		kept, err := retainedByRetentionSQL(query, at, event[3], event[6])
		if err != nil {
			return nil, err
		}
		if kept {
			out = append(out, []string{event[1], event[0], event[2], event[4], event[12], event[13], event[14], event[15], event[16], event[17], event[18]})
		}
	}
	return out, nil
}

func TestRetentionPredicateAgainstClickHouse(t *testing.T) {
	if os.Getenv("AKIT_CLICKHOUSE_TEST") != "1" {
		t.Skip("set AKIT_CLICKHOUSE_TEST=1 to compare retention SQL with ClickHouse")
	}
	addr := os.Getenv("AKIT_CLICKHOUSE_URL")
	if addr == "" {
		addr = "http://127.0.0.1:18123"
	}
	cut := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	base := cut.Add(-90 * 24 * time.Hour)
	var samples []time.Time
	for ms := -3000; ms <= 3000; ms += 125 {
		samples = append(samples, base.Add(time.Duration(ms)*time.Millisecond))
	}
	samples = append(samples, time.Date(2026, 3, 29, 1, 30, 0, 0, time.UTC), time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC))
	var literals []string
	for _, at := range samples {
		literals = append(literals, "toDateTime64('"+at.Format("2006-01-02 15:04:05.000")+"', 3, 'UTC')")
	}
	query := "SELECT " + eventWithinRetentionAt("ts", cut, eventRetentionDays) + " FROM (SELECT arrayJoin([" + strings.Join(literals, ",") + "]) AS ts) FORMAT TabSeparated"
	u, err := url.Parse(addr)
	if err != nil {
		t.Fatal(err)
	}
	params := u.Query()
	params.Set("readonly", "1")
	params.Set("query", query)
	u.RawQuery = params.Encode()
	resp, err := http.Post(u.String(), "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readonly ClickHouse query: %s: %s", resp.Status, body)
	}
	got := strings.Fields(string(body))
	if len(got) != len(samples) {
		t.Fatalf("ClickHouse returned %d booleans for %d timestamps: %s", len(got), len(samples), body)
	}
	for i, at := range samples {
		want := eventRetained(at.Truncate(time.Millisecond), "codex", "agent", cut)
		if (got[i] == "1") != want {
			t.Errorf("at %s: SQL %s, Go %t", at.Format(time.RFC3339Nano), got[i], want)
		}
	}
}
