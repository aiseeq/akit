package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// server is the ClickHouse HTTP interface: one POST per statement, the standard library only.
type server struct {
	base string
	hc   *http.Client
}

func newServer(base string) (*server, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("bad --url %q: want http://host:port", base)
	}
	return &server{base: strings.TrimRight(base, "/") + "/", hc: &http.Client{}}, nil
}

// chError is an error the server answered with; its text is the first line of the server's message.
type chError struct {
	status int
	code   string
	msg    string
}

func (e *chError) Error() string {
	return fmt.Sprintf("clickhouse: HTTP %d, code %s: %s", e.status, e.code, e.msg)
}

// post runs one statement. With body, the statement goes in the URL and body is its data (an INSERT); without,
// the statement is the body. The answer is copied to out when given. timeout 0 — no limit.
func (s *server) post(ctx context.Context, query string, body io.Reader, settings url.Values, out io.Writer, timeout time.Duration) error {
	_, err := s.send(ctx, query, body, settings, out, timeout)
	return err
}

// insert runs an INSERT and returns how many rows the server wrote, from its X-ClickHouse-Summary: the rows a
// WHERE of the INSERT SELECT let through, a repeat dropped by the deduplication token counted as written.
func (s *server) insert(ctx context.Context, query string, body io.Reader, settings url.Values, timeout time.Duration) (int64, error) {
	h, err := s.send(ctx, query, body, settings, nil, timeout)
	if err != nil {
		return 0, err
	}
	var sum struct {
		Written string `json:"written_rows"`
	}
	raw := h.Get("X-ClickHouse-Summary")
	if err := json.Unmarshal([]byte(raw), &sum); err != nil {
		return 0, fmt.Errorf("clickhouse: insert answered without a summary of written rows (%q): %w", shorten(raw, 100), err)
	}
	n, err := strconv.ParseInt(sum.Written, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("clickhouse: written_rows %q: %w", sum.Written, err)
	}
	return n, nil
}

func (s *server) send(ctx context.Context, query string, body io.Reader, settings url.Values, out io.Writer, timeout time.Duration) (http.Header, error) {
	params := url.Values{}
	maps.Copy(params, settings)
	if body == nil {
		body = strings.NewReader(query)
	} else {
		params.Set("query", query)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"?"+params.Encode(), body)
	if err != nil {
		return nil, fmt.Errorf("clickhouse request: %w", err)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, rerr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if rerr != nil {
			msg = append(msg, " (answer cut: "+rerr.Error()+")"...)
		}
		first, _, _ := strings.Cut(strings.TrimSpace(string(msg)), "\n")
		return nil, &chError{status: resp.StatusCode, code: resp.Header.Get("X-ClickHouse-Exception-Code"), msg: shorten(first, 300)}
	}
	if out == nil {
		out = io.Discard
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		return nil, fmt.Errorf("clickhouse answer: %w", err)
	}
	return resp.Header, nil
}

// query runs a statement and returns its answer.
func (s *server) query(ctx context.Context, q string, timeout time.Duration) ([]byte, error) {
	var b bytes.Buffer
	err := s.post(ctx, q, nil, nil, &b, timeout)
	return b.Bytes(), err
}

// tsvRows splits a TabSeparated answer into rows of unescaped fields.
func tsvRows(data []byte) [][]string {
	var out [][]string
	for l := range strings.SplitSeq(strings.TrimSuffix(string(data), "\n"), "\n") {
		if l == "" {
			continue
		}
		fields := strings.Split(l, "\t")
		for i := range fields {
			fields[i] = unescape(fields[i])
		}
		out = append(out, fields)
	}
	return out
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

var errStopped = errors.New("stopped")
