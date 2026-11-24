package transcriptdb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeRow writes a RowBinary row (line UInt32, raw String) as the server answers the raw query.
func writeRow(w io.Writer, line uint32, raw string) {
	_ = binary.Write(w, binary.LittleEndian, line)
	var n [binary.MaxVarintLen64]byte
	_, _ = w.Write(n[:binary.PutUvarint(n[:], uint64(len(raw)))])
	_, _ = io.WriteString(w, raw)
}

// serve is a client of a stub server; every request must be a read-only POST of the stub tool.
func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method != http.MethodPost || q.Get("readonly") != "1" || q.Get("database") != "tr" || !strings.HasPrefix(q.Get("log_comment"), "stub:") {
			t.Errorf("request %s %s", r.Method, r.URL.RawQuery)
		}
		h(w, r)
	}))
	t.Cleanup(s.Close)
	c, err := New(s.URL, "tr")
	if err != nil {
		t.Fatal(err)
	}
	return c.WithTool("stub")
}

func TestFilesUsesPostBodyAndBoundParameters(t *testing.T) {
	const injected = `\') > 0 OR 1=1) --`
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Query().Has("query") || strings.Contains(string(body), injected) {
			t.Errorf("SQL or a value out of place: %s %s", r.URL.RawQuery, body)
		}
		if !strings.Contains(string(body), "{prefix:String}") || !strings.Contains(string(body), "FROM tr.files FINAL") {
			t.Errorf("query without the parameter or the newest version: %s", body)
		}
		if got := r.URL.Query().Get("param_prefix"); got != injected {
			t.Errorf("param_prefix=%q, want %q", got, injected)
		}
		io.WriteString(w, `{"file_id":12,"path":"/p.jsonl","kind":"claude","project":"p","session":"s1","mtime_s":1795334400,"size":9,"ingested":9,"lines":2,"dirty":0}`+"\n")
	})
	files, err := c.Files(context.Background(), "startsWith(project,{prefix:String})", map[string]string{"prefix": injected})
	if err != nil || len(files) != 1 || files[0].Lines != 2 || files[0].ID != 12 || files[0].Session != "s1" || !files[0].MTime().Equal(time.Unix(1795334400, 0)) {
		t.Fatalf("Files = %#v, %v", files, err)
	}
}

func TestFilesByPathBindsPaths(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("param_paths"); got != `['/a\'b.jsonl','/c.jsonl']` {
			t.Errorf("param_paths=%q", got)
		}
		io.WriteString(w, `{"file_id":1,"path":"/c.jsonl"}`+"\n")
	})
	files, err := c.FilesByPath(context.Background(), "/a'b.jsonl", "/c.jsonl")
	if err != nil || len(files) != 1 || files[0].Path != "/c.jsonl" {
		t.Fatalf("FilesByPath = %#v, %v", files, err)
	}
	if files, err := c.FilesByPath(context.Background()); err != nil || files != nil {
		t.Fatalf("no paths: %v %v", files, err)
	}
}

func TestFilesReportsServerError(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Code: 62. Syntax error", http.StatusBadRequest)
	})
	_, err := c.Files(context.Background(), "1", nil)
	var server *ServerError
	if !IsSourceError(err) || !errors.As(err, &server) || !strings.Contains(err.Error(), "Syntax error") {
		t.Fatalf("server error not passed on: %v", err)
	}
}

func TestFilesRejectsBrokenAnswer(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"file_id":1,"path":"/a.jsonl"}`+"\nCode: 241. DB::Exception: Memory limit exceeded\n")
	})
	if _, err := c.Files(context.Background(), "1", nil); !IsSourceError(err) {
		t.Fatalf("an answer broken after a row accepted: %v", err)
	}
}

func rawServer(t *testing.T, write func(io.Writer)) *Client {
	return serve(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "FROM tr.raw") || !strings.Contains(string(body), "BETWEEN 1 AND {lines:UInt32}") ||
			r.URL.Query().Get("param_file_id") != "12" || r.URL.Query().Get("param_lines") != "2" {
			t.Errorf("raw query without the bounds of the version: %s %s", body, r.URL.RawQuery)
		}
		var b bytes.Buffer
		write(&b)
		_, _ = w.Write(b.Bytes())
	})
}

func TestOpenRawStreamsAllLinesOfVersion(t *testing.T) {
	c := rawServer(t, func(w io.Writer) {
		writeRow(w, 1, `{"type":"user"}`)
		writeRow(w, 2, "{\"type\":\"assistant\",\"t\":\"a\\tb\"}\r")
	})
	r, err := c.OpenRaw(context.Background(), File{ID: 12, Path: "/p.jsonl", Size: 9, Ingested: 9, Lines: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "{\"type\":\"user\"}\n{\"type\":\"assistant\",\"t\":\"a\\tb\"}\r\n" {
		t.Fatalf("stream = %q, %v", got, err)
	}
}

func TestOpenRawRejectsBrokenStream(t *testing.T) {
	for name, write := range map[string]func(io.Writer){
		"empty":              func(io.Writer) {},
		"cut in the number":  func(w io.Writer) { _, _ = w.Write([]byte{1, 0}) },
		"cut line":           func(w io.Writer) { writeRow(w, 1, `{"x":1}`); _, _ = w.Write([]byte{2, 0, 0, 0, 9, '{'}) },
		"last line missing":  func(w io.Writer) { writeRow(w, 1, `{"x":1}`) },
		"gap":                func(w io.Writer) { writeRow(w, 1, `{}`); writeRow(w, 3, `{}`) },
		"not from the first": func(w io.Writer) { writeRow(w, 2, `{}`); writeRow(w, 3, `{}`) },
		"line twice":         func(w io.Writer) { writeRow(w, 1, `{}`); writeRow(w, 1, `{}`) },
		"tail after the end": func(w io.Writer) {
			writeRow(w, 1, `{}`)
			writeRow(w, 2, `{}`)
			_, _ = io.WriteString(w, "Code: 241. DB::Exception")
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := rawServer(t, write)
			r, err := c.OpenRaw(context.Background(), File{ID: 12, Path: "/x.jsonl", Size: 1, Ingested: 1, Lines: 2})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if _, err := io.ReadAll(r); !IsSourceError(err) || !strings.Contains(err.Error(), "/x.jsonl") {
				t.Fatalf("incomplete answer accepted: %v", err)
			}
		})
	}
}

func TestOpenRawServerErrorIsSourceError(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Code: 241. DB::Exception: Memory limit exceeded", http.StatusInternalServerError)
	})
	if _, err := c.OpenRaw(context.Background(), File{ID: 12, Path: "/x.jsonl", Lines: 2}); !IsSourceError(err) ||
		!strings.Contains(err.Error(), "Memory limit exceeded") || !strings.Contains(err.Error(), "/x.jsonl") {
		t.Fatalf("server error: %v", err)
	}
}

func TestOpenReadsIncompleteFileFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.jsonl")
	if err := os.WriteFile(path, []byte("disk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := serve(t, func(http.ResponseWriter, *http.Request) {
		t.Error("an incomplete file is read from the database")
	})
	for _, f := range []File{
		{ID: 1, Path: path, Dirty: 1, Size: 5, Ingested: 5, Lines: 1},
		{ID: 1, Path: path, Size: 5, Ingested: 4, Lines: 1},
	} {
		if _, err := c.OpenRaw(context.Background(), f); !IsSourceError(err) {
			t.Errorf("OpenRaw took an incomplete file %+v: %v", f, err)
		}
		r, err := c.Open(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		r.Close()
		if err != nil || string(got) != "disk\n" {
			t.Errorf("Open(%+v) = %q, %v", f, got, err)
		}
	}
	if _, err := c.Open(context.Background(), File{Path: filepath.Join(dir, "gone.jsonl"), Dirty: 1}); !errors.Is(err, fs.ErrNotExist) || !IsSourceError(err) {
		t.Errorf("missing on disk too: want ErrNotExist, got %v", err)
	}
}

// OpenLoaded reads the loaded lines of a file that grew past them or is being sent; no lines — no query.
func TestOpenLoadedReadsLoadedLinesOfGrowingFile(t *testing.T) {
	c := rawServer(t, func(w io.Writer) {
		writeRow(w, 1, `{"a":1}`)
		writeRow(w, 2, `{"b":2}`)
	})
	for _, f := range []File{
		{ID: 12, Path: "/g.jsonl", Size: 100, Ingested: 16, Lines: 2},
		{ID: 12, Path: "/g.jsonl", Dirty: 1, Ingested: 16, Lines: 2},
	} {
		r, err := c.OpenLoaded(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		r.Close()
		if err != nil || string(got) != "{\"a\":1}\n{\"b\":2}\n" {
			t.Fatalf("OpenLoaded(%+v) = %q, %v", f, got, err)
		}
	}
	empty := serve(t, func(http.ResponseWriter, *http.Request) { t.Error("a file without lines is queried") })
	r, err := empty.OpenLoaded(context.Background(), File{ID: 3, Path: "/e.jsonl"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(r); err != nil || len(got) != 0 {
		t.Fatalf("no lines: %q %v", got, err)
	}
}

func TestModifiedNotBeforeKeepsSameSecond(t *testing.T) {
	f := File{MTimeSec: 100}
	if !f.ModifiedNotBefore(time.Unix(100, 500e6)) || !f.ModifiedNotBefore(time.Unix(100, 0)) {
		t.Error("a file of the same second dropped")
	}
	if f.ModifiedNotBefore(time.Unix(101, 0)) {
		t.Error("a file of the second before kept")
	}
}

func TestParseErrorIsNotSourceError(t *testing.T) {
	if IsSourceError(fmt.Errorf("parsing rollout x: %w", bufio.ErrTooLong)) {
		t.Fatal("a parse error taken for a source failure")
	}
	for _, err := range []error{sourceErr("HTTP %d", 500), &ServerError{status: 500}, fmt.Errorf("%w: refused", ErrUnavailable)} {
		if !IsSourceError(fmt.Errorf("parsing rollout x: %w", err)) {
			t.Fatalf("source failure under a wrapper not seen: %v", err)
		}
	}
}

func TestDateTimeParamIsUTCSeconds(t *testing.T) {
	at := time.Date(2026, 1, 2, 4, 5, 6, 789e6, time.FixedZone("x", 3600))
	if got := DateTimeParam(at); got != "2026-01-02 03:05:06" {
		t.Fatalf("DateTimeParam = %q", got)
	}
}
