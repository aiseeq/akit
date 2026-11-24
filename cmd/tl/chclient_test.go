package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Rows come unescaped; an error the server writes after the first rows is an error, not rows.
func TestCHClientAnswer(t *testing.T) {
	const tag = "abcdefghijklmnop"
	body := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Query().Get("readonly") != "1" || r.URL.Query().Get("param_x") != `a\tb` {
			t.Errorf("request %s %s", r.Method, r.URL.RawQuery)
		}
		w.Header().Set("X-ClickHouse-Exception-Tag", tag)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c, err := newCHClient(srv.URL, "tr")
	if err != nil {
		t.Fatal(err)
	}
	q := chQuery{Name: "t", SQL: "SELECT 1", Params: map[string]string{"x": chString("a\tb")}}
	body = "1\tline\\none\\ttab\\\\\n2\t\n"
	rows, err := c.SelectTSV(t.Context(), q)
	if err != nil || len(rows) != 2 || rows[0][1] != "line\none\ttab\\" || rows[1][1] != "" {
		t.Errorf("rows %q, %v", rows, err)
	}
	body = "1\tx\n\r\n__exception__\r\n" + tag + "\r\nCode: 241. DB::Exception: Memory limit exceeded\n60 " + tag + "\r\n__exception__\r\n"
	var serverErr *chServerError
	if _, err := c.SelectTSV(t.Context(), q); !errors.As(err, &serverErr) || !strings.Contains(err.Error(), "Memory limit") {
		t.Errorf("an error after rows: %v", err)
	}
	// text about such a block inside a row is a row
	body = "1\t__exception__\\r\\n" + tag + "\n"
	if rows, err := c.SelectTSV(t.Context(), q); err != nil || len(rows) != 1 {
		t.Errorf("%q %v", rows, err)
	}
	srv.Close()
	if _, err := c.SelectTSV(t.Context(), q); !errors.Is(err, errUnavailable) {
		t.Errorf("a closed server: %v", err)
	}
	if _, err := newCHClient("127.0.0.1:1", "tr"); err == nil {
		t.Error("an address without scheme accepted")
	}
	if _, err := newCHClient("http://127.0.0.1:1", "tr; DROP"); err == nil {
		t.Error("a database name with SQL accepted")
	}
}
