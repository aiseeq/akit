// Package transcriptdb reads agent transcripts from the ClickHouse database the tlfeed loader fills: files (one row
// per transcript, its newest version under FINAL), raw (the lines of a file as they are on disk) and usage (token
// usage of the model replies). The client is read-only: every query goes with readonly=1.
//
// A tool picks the database by TL_BACKEND, db by default (SourceFromEnv, Backend), or explicitly (Connect, New); the address is
// TL_CLICKHOUSE_URL and TL_CLICKHOUSE_DB. Lines of a file come from raw only when the database holds all of them in
// order: a gap, a cut answer or a file not loaded to its end is a SourceError, and Open reads such a file from disk.
package transcriptdb

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

// Client is the ClickHouse HTTP interface, standard library only. Every request is read-only: readonly=1 goes
// with the settings of each query, so the server refuses anything that writes.
type Client struct {
	base string
	db   string
	tool string
	hc   *http.Client
}

// defaultTool tags the queries of a client no tool has named (WithTool).
const defaultTool = "transcriptdb"

// NewFromEnv creates a client from TL_CLICKHOUSE_URL and TL_CLICKHOUSE_DB.
func NewFromEnv() (*Client, error) {
	base := os.Getenv("TL_CLICKHOUSE_URL")
	if base == "" {
		base = defaultClickHouseURL
	}
	db := os.Getenv("TL_CLICKHOUSE_DB")
	if db == "" {
		db = defaultClickHouseDB
	}
	return New(base, db)
}

// New creates a client for the given HTTP endpoint and database name.
func New(base, db string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("bad TL_CLICKHOUSE_URL %q: want http://host:port", base)
	}
	if db == "" || strings.Trim(db, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") != "" {
		return nil, fmt.Errorf("bad TL_CLICKHOUSE_DB %q: want a plain name", db)
	}
	// Idle connections are kept for parallel readers (several files at once), not for two as by default.
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: dialTimeout}).DialContext,
		ResponseHeaderTimeout: headerTimeout, IdleConnTimeout: 90 * time.Second, MaxIdleConnsPerHost: 16}
	return &Client{base: strings.TrimRight(base, "/") + "/", db: db, tool: defaultTool, hc: &http.Client{Transport: tr}}, nil
}

// Ping asks the server's /ping; nil — it answered OK.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"ping", nil)
	if err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	if err := resp.Body.Close(); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ping: %s", resp.Status)
	}
	return nil
}

// WithTool is a copy of the client whose queries carry log_comment "<tool>:<query name>", so system.query_log tells
// the tool that sent them. The copy shares the connections of c.
func (c *Client) WithTool(tool string) *Client {
	d := *c
	d.tool = tool
	return &d
}

// DB is the database name the client reads.
func (c *Client) DB() string { return c.db }

// dialTimeout and headerTimeout bound a server that does not take the connection or does not start the answer:
// a query that sorts sends its header after the sort, which takes seconds over days of events.
const (
	dialTimeout   = 3 * time.Second
	headerTimeout = 30 * time.Second
)

// ErrUnavailable marks an answer that did not come: no connection, a refused or cut one, or the time ran out.
var ErrUnavailable = errors.New("database unavailable")

// ServerError is an error the server answered with: the first line of its message.
type ServerError struct {
	status int
	code   string
	msg    string
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("clickhouse: HTTP %d, code %s: %s", e.status, e.code, e.msg)
}

// Query is one read: the statement with typed parameters, their values and the settings of this query.
// Name goes to log_comment after the tool name, so the query is found in system.query_log by it.
type Query struct {
	Name     string
	SQL      string
	Params   map[string]string
	Settings url.Values
}

// Select is SelectTSV of a query without its own settings: the name for log_comment, the statement and parameters.
func (c *Client) Select(ctx context.Context, name, sql string, params map[string]string) ([][]string, error) {
	return c.SelectTSV(ctx, Query{Name: name, SQL: sql, Params: params})
}

// SelectTSV executes a read-only query and returns its TabSeparated rows.
func (c *Client) SelectTSV(ctx context.Context, q Query) ([][]string, error) {
	var rows [][]string
	err := c.EachTSV(ctx, q, func(fields []string) error {
		rows = append(rows, fields)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// EachTSV executes a read-only query and gives fn its TabSeparated rows one by one, unescaped, as they come: a large
// answer is not held whole. An error of fn stops the read and is returned as is.
func (c *Client) EachTSV(ctx context.Context, q Query, fn func(fields []string) error) error {
	q.SQL += " FORMAT TabSeparated"
	resp, err := c.post(ctx, q)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	br := bufio.NewReaderSize(resp.Body, 1<<20)
	// An error after the first rows comes inside the answer, the status 200 already sent: a block between lines
	// "__exception__\r" with the tag of the header. A row cannot hold a bare \r: TabSeparated escapes it.
	tag := resp.Header.Get("X-ClickHouse-Exception-Tag")
	for {
		line, rerr := br.ReadString('\n')
		if tag != "" && line == "__exception__\r\n" {
			return exceptionBlock(br, tag, resp.Header.Get("X-ClickHouse-Exception-Code"))
		}
		if line = strings.TrimSuffix(line, "\n"); line != "" && line != "\r" {
			fields := strings.Split(line, "\t")
			for i := range fields {
				fields[i] = TSVUnescape(fields[i])
			}
			if err := fn(fields); err != nil {
				return err
			}
		}
		if errors.Is(rerr, io.EOF) {
			return nil
		}
		if rerr != nil {
			if IsTransportError(rerr) || ctx.Err() != nil {
				return fmt.Errorf("%w: answer cut: %w", ErrUnavailable, rerr)
			}
			return fmt.Errorf("clickhouse answer: %w", rerr)
		}
	}
}

// post sends the statement of q as it is, its format clause included, and returns the answer of status 200; any
// other status is a ServerError with the first line of the message, no answer — ErrUnavailable.
func (c *Client) post(ctx context.Context, q Query) (*http.Response, error) {
	params := url.Values{}
	maps.Copy(params, q.Settings)
	params.Set("readonly", "1")
	params.Set("database", c.db)
	params.Set("log_comment", c.tool+":"+q.Name)
	for name, value := range q.Params {
		params.Set("param_"+name, value)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"?"+params.Encode(), strings.NewReader(q.SQL))
	if err != nil {
		return nil, fmt.Errorf("clickhouse request: %w", err)
	}
	// The server is local; a compressed answer would only spend CPU on both ends.
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, rerr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if rerr != nil {
			msg = append(msg, " (answer cut: "+rerr.Error()+")"...)
		}
		first, _, _ := strings.Cut(strings.TrimSpace(string(msg)), "\n")
		return nil, &ServerError{status: resp.StatusCode, code: resp.Header.Get("X-ClickHouse-Exception-Code"), msg: first}
	}
	return resp, nil
}

// exceptionBlock reads the error the server wrote after the rows it had sent.
func exceptionBlock(br *bufio.Reader, tag, code string) error {
	e := &ServerError{status: http.StatusOK, code: code, msg: "an exception block cut or with another tag"}
	got, err := br.ReadString('\n')
	if err != nil || strings.TrimRight(got, "\r\n") != tag {
		return e
	}
	if msg, err := br.ReadString('\n'); err == nil || msg != "" {
		e.msg = strings.TrimSpace(msg)
	}
	return e
}

// IsTransportError reports whether err indicates a broken or timed-out transport.
func IsTransportError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded)
}

// TSVUnescape decodes a TabSeparated field.
func TSVUnescape(s string) string {
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

// TSVEscape encodes a TabSeparated field.
func TSVEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`, "\x00", `\0`, "\b", `\b`, "\f", `\f`, "\a", `\a`, "\v", `\v`)
	return r.Replace(s)
}

// String encodes a String query parameter value.
func String(s string) string { return TSVEscape(s) }

// Strings encodes an Array(String) query parameter value.
func Strings(values []string) string { return ArrayParam(values, Quote) }

// Uints encodes an Array(UInt64) query parameter value.
func Uints(values []uint64) string {
	return ArrayParam(values, func(v uint64) string { return strconv.FormatUint(v, 10) })
}

// ArrayParam encodes the contents of an array query parameter.
func ArrayParam[T any](values []T, literal func(T) string) string {
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

// Quote escapes a string literal inside a ClickHouse array parameter.
func Quote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// Time formats a DateTime64(3, 'UTC') query parameter value.
func Time(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.000")
}
