package transcriptdb

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSelectSendsSettingsParamsAndToolName(t *testing.T) {
	var comments []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		body, _ := io.ReadAll(r.Body)
		comments = append(comments, q.Get("log_comment"))
		if q.Get("readonly") != "1" || q.Get("database") != "db1" || q.Get("max_threads") != "2" || q.Get("param_v") != `a\tb` ||
			string(body) != "SELECT {v:String} FORMAT TabSeparated" {
			t.Errorf("request %s %q", r.URL.RawQuery, body)
		}
		io.WriteString(w, "a\\tb\tx\\\\y\n\\N\t\\n\n")
	}))
	defer s.Close()
	c, err := New(s.URL, "db1")
	if err != nil {
		t.Fatal(err)
	}
	named := c.WithTool("mytool")
	for _, cl := range []*Client{c, named} {
		rows, err := cl.SelectTSV(context.Background(), Query{Name: "q1", SQL: "SELECT {v:String}", Params: map[string]string{"v": String("a\tb")},
			Settings: url.Values{"max_threads": {"2"}, "readonly": {"0"}}})
		if err != nil || len(rows) != 2 || rows[0][0] != "a\tb" || rows[0][1] != `x\y` || rows[1][0] != "" || rows[1][1] != "\n" {
			t.Fatalf("rows %q, %v", rows, err)
		}
	}
	if strings.Join(comments, ",") != "transcriptdb:q1,mytool:q1" || c.DB() != "db1" {
		t.Fatalf("log_comment %v", comments)
	}
}

func TestEachTSVStopsOnCallerError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "1\n2\n3\n")
	}))
	defer s.Close()
	c, _ := New(s.URL, "tr")
	stop := errors.New("enough")
	var seen int
	err := c.EachTSV(context.Background(), Query{Name: "n", SQL: "SELECT 1"}, func([]string) error {
		seen++
		if seen == 2 {
			return stop
		}
		return nil
	})
	if err != stop || seen != 2 {
		t.Fatalf("err %v after %d rows", err, seen)
	}
}

func TestSelectExceptionInsideAnswer(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-ClickHouse-Exception-Tag", "tag1")
		w.Header().Set("X-ClickHouse-Exception-Code", "241")
		io.WriteString(w, "1\n__exception__\r\ntag1\nCode: 241. Memory limit exceeded\n")
	}))
	defer s.Close()
	c, _ := New(s.URL, "tr")
	_, err := c.Select(context.Background(), "n", "SELECT 1", nil)
	var server *ServerError
	if !errors.As(err, &server) || !strings.Contains(err.Error(), "Memory limit") || !IsSourceError(err) {
		t.Fatalf("exception block: %v", err)
	}
}

func TestSelectUnavailable(t *testing.T) {
	c, err := New("http://127.0.0.1:9", "tr")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Select(ctx, "n", "SELECT 1", nil); !errors.Is(err, ErrUnavailable) || !IsSourceError(err) {
		t.Fatalf("no server: %v", err)
	}
}

func TestNewRejectsBadAddress(t *testing.T) {
	for _, tc := range [][2]string{{"127.0.0.1:18123", "tr"}, {"http://h:1", "tr;drop"}, {"http://h:1", ""}} {
		if _, err := New(tc[0], tc[1]); err == nil {
			t.Errorf("New(%q, %q) accepted", tc[0], tc[1])
		}
	}
	t.Setenv("TL_CLICKHOUSE_URL", "")
	t.Setenv("TL_CLICKHOUSE_DB", "")
	if c, err := NewFromEnv(); err != nil || c.DB() != "tr" || c.base != "http://127.0.0.1:18123/" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
}

func TestParamEncoding(t *testing.T) {
	if got := Strings([]string{`a'b`, `c\d`}); got != `['a\'b','c\\d']` {
		t.Errorf("Strings = %s", got)
	}
	if got := Uints([]uint64{1, 22}); got != "[1,22]" {
		t.Errorf("Uints = %s", got)
	}
	if got := Time(time.Date(2026, 1, 2, 3, 4, 5, 6e6, time.FixedZone("x", -3600))); got != "2026-01-02 04:04:05.006" {
		t.Errorf("Time = %s", got)
	}
	for _, s := range []string{"plain", "tab\tnl\ncr\rbs\\nul\x00"} {
		if got := TSVUnescape(TSVEscape(s)); got != s {
			t.Errorf("TSV round trip %q -> %q", s, got)
		}
	}
}
