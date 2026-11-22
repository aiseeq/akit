package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
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
		return fmt.Errorf("clickhouse request: %w", err)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("clickhouse: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, rerr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if rerr != nil {
			msg = append(msg, " (answer cut: "+rerr.Error()+")"...)
		}
		first, _, _ := strings.Cut(strings.TrimSpace(string(msg)), "\n")
		return &chError{status: resp.StatusCode, code: resp.Header.Get("X-ClickHouse-Exception-Code"), msg: shorten(first, 300)}
	}
	if out == nil {
		out = io.Discard
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("clickhouse answer: %w", err)
	}
	return nil
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
