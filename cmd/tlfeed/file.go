package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aiseeq/akit/transcript"
)

// tailBytes — how many of the last ingested bytes identify a file: a file cut back and grown past its old size
// keeps its size larger than what is loaded, but not these bytes.
const tailBytes = 256

// file is what the loader knows about one transcript: its identity, how far it is in the tables, and, while it is
// active, the parser that keeps the state between lines.
type file struct {
	id            uint64
	src           transcript.Source
	kind, project string
	inode         uint64
	size          int64
	mtime         time.Time
	ingested      int64 // bytes of complete lines in raw

	complete    int64 // end of the last complete line seen in the file
	lines       int   // lines in raw
	evLines     int   // lines whose events are in events (below lines only after a crash between the inserts)
	tail        uint64
	gen         uint64
	name        string
	bad         int
	first, last time.Time
	follow      *transcript.Follow
	parsed      int64 // bytes the parser has read
	badBase     int   // bad lines among those the parser skipped (transcript.Follow.SkipThrough)
	grown       time.Time
	gone        bool
	stale       bool   // the last look could not read the file: the next pass looks again
	ack         stored // as the files table has it: the server confirmed this much
}

// stored — the state of a file as the files table has it, for the dirty row of a batch that sends more.
type stored struct {
	lines, evLines int
	ingested       int64
	tail           uint64
}

// commit records the present state as the one in the files table.
func (f *file) commit() {
	f.ack = stored{lines: f.lines, evLines: f.evLines, ingested: f.ingested, tail: f.tail}
}

// committed — the file as the files table has it.
func (f *file) committed() *file {
	c := *f
	c.lines, c.evLines, c.ingested, c.tail = f.ack.lines, f.ack.evLines, f.ack.ingested, f.ack.tail
	return &c
}

// describe sets the source fields that come from the path.
func (f *file) describe(home string, src transcript.Source) {
	f.src = src
	f.kind, f.project = kindOf(home, src)
}

// kindOf — claude, sub or codex, and the project: the Claude project folder or worker:<task>.
func kindOf(home string, src transcript.Source) (kind, project string) {
	sep := string(filepath.Separator)
	if rel, ok := strings.CutPrefix(src.Path, filepath.Join(home, ".local", "state", "worker")+sep); ok {
		task, _, _ := strings.Cut(rel, sep)
		return "codex", "worker:" + task
	}
	if strings.Contains(src.Path, sep+"codex"+sep+"sessions"+sep) {
		return "codex", ""
	}
	kind = "claude"
	if src.ParentID != "" {
		kind = "sub"
	}
	rel := strings.TrimPrefix(src.Path, filepath.Join(home, ".claude", "projects")+sep)
	project, _, _ = strings.Cut(rel, sep)
	return kind, project
}

// newID — the file id: the second of its first event in the high bits, so files of one time window sit next to
// each other inside every role of the events table; the low 22 bits of a path hash keep ids apart within a second.
func newID(path string, at time.Time, taken func(uint64) bool) uint64 {
	h := fnv.New64a()
	h.Write([]byte(path))
	id := uint64(max(at.Unix(), 0))<<22 | h.Sum64()&(1<<22-1)
	for id == 0 || taken(id) {
		id++
	}
	return id
}

func inodeOf(st os.FileInfo) uint64 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return s.Ino
	}
	return 0
}

// tailSum — the sum of the tailBytes bytes before end.
func tailSum(b []byte) uint64 {
	if len(b) == 0 {
		return 0
	}
	s := sha256.Sum256(b)
	return binary.BigEndian.Uint64(s[:8])
}

// tailAt reads the tailBytes bytes before end of the file and sums them; ok false — the file is shorter.
func tailAt(path string, end int64) (uint64, bool, error) {
	if end == 0 {
		return 0, true, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false, fmt.Errorf("tail sum: %w", err)
	}
	defer f.Close()
	from := max(end-tailBytes, 0)
	b := make([]byte, end-from)
	if _, err := f.ReadAt(b, from); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("tail sum: %w", err)
	}
	return tailSum(b), true, nil
}

// pieces reads the file from off up to size in pieces of about n bytes, each ending at a line end, and gives
// each to fn with its offset. A line longer than n makes its piece longer. Returns the end of the last complete
// line; the incomplete rest is left for the next pass.
func pieces(path string, off, size int64, n int, buf *[]byte, fn func(data []byte, at int64) error) (int64, error) {
	if size <= off {
		return off, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return off, fmt.Errorf("read transcript: %w", err)
	}
	defer f.Close()
	for off < size {
		want := min(int64(n), size-off)
		var data []byte
		for {
			if int64(cap(*buf)) < want {
				*buf = make([]byte, want)
			}
			b := (*buf)[:want]
			got, err := f.ReadAt(b, off)
			if err != nil && !errors.Is(err, io.EOF) {
				return off, fmt.Errorf("read %s: %w", path, err)
			}
			b = b[:got]
			if nl := bytes.LastIndexByte(b, '\n'); nl >= 0 {
				data = b[:nl+1]
				break
			}
			if int64(got) < want || off+want >= size {
				return off, nil // only an incomplete line is left
			}
			want = min(want*2, size-off)
		}
		if err := fn(data, off); err != nil {
			return off, err
		}
		off += int64(len(data))
	}
	return off, nil
}
