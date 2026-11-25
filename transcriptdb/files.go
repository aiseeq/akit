package transcriptdb

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// SourceError is a failure of the database as a source of transcripts: the query or its HTTP answer, a raw stream
// cut or with a gap, a file not loaded to its end and missing on disk too. A report must not be built over it:
// reading the files gives no such error, and skipping the file silently would change the result.
type SourceError struct{ Err error }

func (e *SourceError) Error() string { return e.Err.Error() }
func (e *SourceError) Unwrap() error { return e.Err }

func sourceErr(format string, args ...any) error {
	return &SourceError{fmt.Errorf(format, args...)}
}

// IsSourceError reports whether err comes from the database rather than from what was read: a SourceError, an
// answer of the server (ServerError) or no answer (ErrUnavailable) anywhere in the chain.
func IsSourceError(err error) bool {
	var se *SourceError
	var server *ServerError
	return errors.As(err, &se) || errors.As(err, &server) || errors.Is(err, ErrUnavailable)
}

// KindCodexExt is the kind of a Codex rollout outside worker tasks, from the session folders of
// transcript.CodexRoots: a benchmark's or the Codex CLI's own. tl shows it only when asked (--codex-ext).
const KindCodexExt = "codex-ext"

// IsCodexKind reports whether a files kind is a Codex rollout: of a worker task (codex) or KindCodexExt.
func IsCodexKind(kind string) bool { return kind == "codex" || kind == KindCodexExt }

// File is the newest version of a files row. The lines of the file in raw are numbered 1 to Lines; Ingested is
// their length in bytes, each with its '\n'.
type File struct {
	ID       uint64 `json:"file_id"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`    // claude, sub, codex (worker tasks) or KindCodexExt
	Project  string `json:"project"` // the Claude project folder, worker:<task>, or for KindCodexExt its cwd named as a Claude project folder
	Session  string `json:"session"` // the short id tl prints
	Parent   string `json:"parent"`  // the parent session of a subagent
	MTimeSec int64  `json:"mtime_s"`
	MTimeMS  int64  `json:"mtime_ms"`
	Size     uint64 `json:"size"`
	Ingested uint64 `json:"ingested"`
	Lines    uint32 `json:"lines"`
	Dirty    uint8  `json:"dirty"`
}

// MTime is the modification time of the file, to the millisecond when available.
func (f File) MTime() time.Time {
	if f.MTimeMS != 0 {
		return time.UnixMilli(f.MTimeMS)
	}
	return time.Unix(f.MTimeSec, 0)
}

// ModifiedNotBefore reports whether the file may have been modified at t or later. Millisecond storage is
// rounded down, so retain the rest of that millisecond; old rows with whole-second mtime_ms values retain a second.
func (f File) ModifiedNotBefore(t time.Time) bool {
	precision := time.Second
	// Rows written before mtime_ms carried milliseconds hold whole seconds times 1000: a zero remainder
	// means second precision, even though mtime_ms is nonzero.
	if f.MTimeMS != 0 && f.MTimeMS%1000 != 0 {
		precision = time.Millisecond
	}
	return f.MTime().Add(precision).After(t)
}

// NeedsDiskRead reports whether the database holds the file only in part: a row of a send in progress (Dirty) or
// a file that grew past its loaded lines. Its lines are read from disk, as without the database.
func (f File) NeedsDiskRead() bool { return f.Dirty != 0 || f.Ingested < f.Size }

// DateTimeParam is the value of a {name:DateTime('UTC')} parameter: t to the second, in UTC.
func DateTimeParam(t time.Time) string { return t.UTC().Format(time.DateTime) }

// Files is the newest version of the files rows that match where, a condition over the columns of files; its
// values go as typed parameters ({name:Type}) in params, never into the text. Every error is a SourceError.
func (c *Client) Files(ctx context.Context, where string, params map[string]string) ([]File, error) {
	q := Query{Name: "files", Params: params, SQL: "SELECT file_id, path, kind, project, session, parent, " +
		"toInt64(toUnixTimestamp(mtime)) AS mtime_s, toInt64(toUnixTimestamp64Milli(mtime)) AS mtime_ms, size, ingested, lines, dirty FROM " + c.db + ".files FINAL WHERE " + where +
		" SETTINGS output_format_json_quote_64bit_integers = 0 FORMAT JSONEachRow"}
	resp, err := c.post(ctx, q)
	if err != nil {
		return nil, &SourceError{fmt.Errorf("files: %w", err)}
	}
	defer resp.Body.Close()
	var out []File
	dec := json.NewDecoder(bufio.NewReaderSize(resp.Body, 1<<16))
	for {
		var f File
		err := dec.Decode(&f)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, sourceErr("files: read the answer: %w", err)
		}
		out = append(out, f)
	}
}

// FilesByPath is the files rows of the given paths; a path the database does not hold has no row.
func (c *Client) FilesByPath(ctx context.Context, paths ...string) ([]File, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	return c.Files(ctx, "path IN {paths:Array(String)}", map[string]string{"paths": Strings(paths)})
}
