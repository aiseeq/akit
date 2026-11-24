package transcriptdb

import (
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the zone of the test does not depend on the machine
)

func TestUsageRepliesFromFinalTable(t *testing.T) {
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	until := from.Add(24 * time.Hour)
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q := r.URL.Query()
		if !strings.Contains(string(body), "FROM tr.usage FINAL") || q.Get("param_from") != "2026-03-01 00:00:00.000" ||
			q.Get("param_until") != "2026-03-02 00:00:00.000" || q.Get("log_comment") != "stub:usage" {
			t.Errorf("usage query %s %s", body, r.URL.RawQuery)
		}
		io.WriteString(w, "1772326800123\t7\tsess-1\t-p\tmodel-a\tmsg_1\treq_1\t10\t20\t30\t40\n"+
			"1772330400000\t8\tsess-2\tworker:t\tmodel-b\tmsg_2\t\t1\t2\t3\t4\n")
	})
	var got []UsageReply
	if err := c.Usage(t.Context(), from, until, func(u UsageReply) error { got = append(got, u); return nil }); err != nil {
		t.Fatal(err)
	}
	want := []UsageReply{
		{At: time.UnixMilli(1772326800123).UTC(), FileID: 7, Session: "sess-1", Project: "-p", Model: "model-a", ReplyID: "msg_1", RequestID: "req_1", Tokens: Tokens{10, 20, 30, 40}},
		{At: time.UnixMilli(1772330400000).UTC(), FileID: 8, Session: "sess-2", Project: "worker:t", Model: "model-b", ReplyID: "msg_2", Tokens: Tokens{1, 2, 3, 4}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Usage:\n%+v\nwant\n%+v", got, want)
	}
	stop := errors.New("stop")
	if err := c.Usage(t.Context(), from, until, func(UsageReply) error { return stop }); err != stop {
		t.Fatalf("the caller's error changed: %v", err)
	}
}

func TestUsageBadRowIsSourceError(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "x\t1\n") })
	if err := c.Usage(t.Context(), time.Time{}, time.Now(), func(UsageReply) error { return nil }); !IsSourceError(err) {
		t.Fatalf("bad row: %v", err)
	}
	bad := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Code: 60. Unknown table", http.StatusNotFound)
	})
	if _, err := bad.UsageTotals(t.Context(), time.Time{}, time.Now(), time.UTC); !IsSourceError(err) || !strings.Contains(err.Error(), "Unknown table") {
		t.Fatalf("server error: %v", err)
	}
}

// Quarter hours of UTC fold into the local days of the zone: across midnight and across a daylight saving change.
func TestUsageTotalsByLocalDay(t *testing.T) {
	loc, err := time.LoadLocation("America/St_Johns") // UTC-3:30, -2:30 in summer
	if err != nil {
		t.Fatal(err)
	}
	q := func(s string) string {
		at, err := time.Parse(time.DateTime, s)
		if err != nil {
			t.Fatal(err)
		}
		return strconv.FormatInt(at.Unix(), 10)
	}
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "toStartOfFifteenMinutes(ts)") || !strings.Contains(string(body), "FROM tr.usage FINAL") {
			t.Errorf("totals query %s", body)
		}
		// 03:15 UTC on 8 March is 23:45 of 7 March local; 03:30 UTC is 00:00 of 8 March
		io.WriteString(w, q("2026-03-08 03:15:00")+"\tm1\tp1\t2\t1\t2\t3\t4\n"+
			q("2026-03-08 03:30:00")+"\tm1\tp1\t1\t10\t20\t30\t40\n"+
			q("2026-03-08 20:00:00")+"\tm1\tp1\t1\t100\t200\t300\t400\n"+
			q("2026-03-08 21:00:00")+"\tm2\tp1\t1\t5\t5\t5\t5\n"+
			// after the change to summer time (05:30 UTC on 8 March): 02:30 UTC on 9 March is 00:00 of 9 March local
			q("2026-03-09 02:15:00")+"\tm1\tp1\t1\t7\t7\t7\t7\n"+
			q("2026-03-09 02:30:00")+"\tm1\tp1\t1\t9\t9\t9\t9\n")
	})
	got, err := c.UsageTotals(t.Context(), time.Time{}, time.Now(), loc)
	if err != nil {
		t.Fatal(err)
	}
	want := []UsageTotal{
		{Day: "2026-03-07", Model: "m1", Project: "p1", Replies: 2, Tokens: Tokens{1, 2, 3, 4}},
		{Day: "2026-03-08", Model: "m1", Project: "p1", Replies: 3, Tokens: Tokens{117, 227, 337, 447}},
		{Day: "2026-03-08", Model: "m2", Project: "p1", Replies: 1, Tokens: Tokens{5, 5, 5, 5}},
		{Day: "2026-03-09", Model: "m1", Project: "p1", Replies: 1, Tokens: Tokens{9, 9, 9, 9}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UsageTotals:\n%+v\nwant\n%+v", got, want)
	}
}
