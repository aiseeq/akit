package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults of the database the transcript loader fills; TL_CLICKHOUSE_URL and TL_CLICKHOUSE_DB change them.
const (
	defaultClickHouseURL = "http://127.0.0.1:18123"
	defaultClickHouseDB  = "tr"
)

// chClient is the ClickHouse HTTP interface, standard library only. Every request is read-only: readonly=1 goes
// with the settings of each query, so the server refuses anything that writes.
type chClient struct {
	base string
	db   string
	hc   *http.Client
}

func newCHClientFromEnv() (*chClient, error) {
	base := os.Getenv("TL_CLICKHOUSE_URL")
	if base == "" {
		base = defaultClickHouseURL
	}
	db := os.Getenv("TL_CLICKHOUSE_DB")
	if db == "" {
		db = defaultClickHouseDB
	}
	return newCHClient(base, db)
}

func newCHClient(base, db string) (*chClient, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("bad TL_CLICKHOUSE_URL %q: want http://host:port", base)
	}
	if db == "" || strings.Trim(db, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") != "" {
		return nil, fmt.Errorf("bad TL_CLICKHOUSE_DB %q: want a plain name", db)
	}
	return &chClient{base: strings.TrimRight(base, "/") + "/", db: db, hc: &http.Client{}}, nil
}

// errUnavailable marks an answer that did not come: no connection, a refused or cut one, or the time ran out.
var errUnavailable = errors.New("database unavailable")

// chServerError is an error the server answered with: the first line of its message.
type chServerError struct {
	status int
	code   string
	msg    string
}

func (e *chServerError) Error() string {
	return fmt.Sprintf("clickhouse: HTTP %d, code %s: %s", e.status, e.code, e.msg)
}

// chQuery is one read: the statement with {name:Type} parameters, their values and the settings of this query.
// name goes to log_comment, so the query is found in system.query_log by it.
type chQuery struct {
	name     string
	sql      string
	params   map[string]string
	settings url.Values
}

// selectTSV runs a query whose answer is TabSeparated and returns its rows of unescaped fields, read as they come.
func (c *chClient) selectTSV(ctx context.Context, q chQuery) ([][]string, error) {
	params := url.Values{}
	maps.Copy(params, q.settings)
	params.Set("readonly", "1")
	params.Set("database", c.db)
	params.Set("log_comment", "tl:"+q.name)
	for name, value := range q.params {
		params.Set("param_"+name, value)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"?"+params.Encode(), strings.NewReader(q.sql+" FORMAT TabSeparated"))
	if err != nil {
		return nil, fmt.Errorf("clickhouse request: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, rerr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if rerr != nil {
			msg = append(msg, " (answer cut: "+rerr.Error()+")"...)
		}
		first, _, _ := strings.Cut(strings.TrimSpace(string(msg)), "\n")
		return nil, &chServerError{status: resp.StatusCode, code: resp.Header.Get("X-ClickHouse-Exception-Code"), msg: first}
	}
	var rows [][]string
	br := bufio.NewReaderSize(resp.Body, 1<<20)
	// An error after the first rows comes inside the answer, the status 200 already sent: a block between lines
	// "__exception__\r" with the tag of the header. A row cannot hold a bare \r: TabSeparated escapes it.
	tag := resp.Header.Get("X-ClickHouse-Exception-Tag")
	for {
		line, rerr := br.ReadString('\n')
		if tag != "" && line == "__exception__\r\n" {
			return nil, exceptionBlock(br, tag, resp.Header.Get("X-ClickHouse-Exception-Code"))
		}
		if line = strings.TrimSuffix(line, "\n"); line != "" && line != "\r" {
			fields := strings.Split(line, "\t")
			for i := range fields {
				fields[i] = tsvUnescape(fields[i])
			}
			rows = append(rows, fields)
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			if isTransportError(rerr) || ctx.Err() != nil {
				return nil, fmt.Errorf("%w: answer cut: %w", errUnavailable, rerr)
			}
			return nil, fmt.Errorf("clickhouse answer: %w", rerr)
		}
	}
	return rows, nil
}

// exceptionBlock reads the error the server wrote after the rows it had sent.
func exceptionBlock(br *bufio.Reader, tag, code string) error {
	e := &chServerError{status: http.StatusOK, code: code, msg: "an exception block cut or with another tag"}
	got, err := br.ReadString('\n')
	if err != nil || strings.TrimRight(got, "\r\n") != tag {
		return e
	}
	if msg, err := br.ReadString('\n'); err == nil || msg != "" {
		e.msg = strings.TrimSpace(msg)
	}
	return e
}

func isTransportError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded)
}

// tsvUnescape undoes the escaping of a TabSeparated field: \t \n \\ and the rest of the C escapes.
func tsvUnescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 == len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'a':
			b.WriteByte('\a')
		case 'v':
			b.WriteByte('\v')
		case '0':
			b.WriteByte(0)
		case 'N':
			// \N is NULL; no column read here is Nullable
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// tsvEscape is the escaping of a TabSeparated field, for the test server that answers like ClickHouse.
func tsvEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`, "\x00", `\0`, "\b", `\b`, "\f", `\f`, "\a", `\a`, "\v", `\v`)
	return r.Replace(s)
}

// chString is a String parameter value: the server reads it escaped as a TabSeparated field.
func chString(s string) string { return tsvEscape(s) }

// chStrings and chUints are Array(String) and Array(UInt64) parameter values in the text form ClickHouse reads.
func chStrings(values []string) string { return chArrayParam(values, chQuote) }

func chUints(values []uint64) string {
	return chArrayParam(values, func(v uint64) string { return strconv.FormatUint(v, 10) })
}

func chArrayParam[T any](values []T, literal func(T) string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(literal(v))
	}
	b.WriteByte(']')
	return b.String()
}

// chQuote is a string literal inside an array parameter: quotes and backslashes escaped.
func chQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// chTime is a DateTime64(3, 'UTC') parameter value.
func chTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.000")
}
