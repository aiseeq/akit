package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strconv"
	"time"

	"github.com/aiseeq/akit/transcript"
)

// shownProblems — how many problem lines a check prints before it only counts them.
const shownProblems = 20

type checker struct {
	srv   *server
	db    string
	home  string
	out   io.Writer
	shown int
	bad   int
}

func (c *checker) problem(format string, args ...any) {
	c.bad++
	if c.shown < shownProblems {
		fmt.Fprintf(c.out, format+"\n", args...)
		c.shown++
	}
}

func (c *checker) rows(q string) ([][]string, error) {
	data, err := c.srv.query(context.Background(), q, 30*time.Minute)
	return tsvRows(data), err
}

// verify rebuilds n random files from raw and compares sha256 with the same bytes of the file on disk: the first
// ingested bytes, as a live file may have grown since.
func (c *checker) verify(n int) (bool, error) {
	q := fmt.Sprintf("SELECT file_id, path, ingested, kind FROM %s.files FINAL WHERE ingested > 0 ORDER BY cityHash64(file_id, %d) LIMIT %d FORMAT TabSeparated",
		c.db, rand.Uint32(), n)
	rows, err := c.rows(q)
	if err != nil {
		return false, err
	}
	ok, gone := 0, 0
	h, sum := sha256.New(), make([]byte, 0, sha256.Size)
	for _, r := range rows {
		id, path := r[0], r[1]
		p := &fields{row: r}
		ingested := int64(p.uint(2))
		if p.err != nil {
			return false, fmt.Errorf("files: %w", p.err)
		}
		disk, err := fileSum(path, ingested)
		if errors.Is(err, os.ErrNotExist) {
			gone++
			continue
		}
		if err != nil {
			c.problem("DIFF\t%s\t%s: %v", r[3], path, err)
			continue
		}
		h.Reset()
		if err := c.srv.post(context.Background(), fmt.Sprintf("SELECT raw FROM %s.raw WHERE file_id = %s ORDER BY line FORMAT LineAsString", c.db, id), nil, nil, h, 30*time.Minute); err != nil {
			return false, err
		}
		if sum = h.Sum(sum[:0]); !bytes.Equal(sum, disk) {
			c.problem("DIFF\t%s\t%d bytes\t%s", r[3], ingested, path)
			continue
		}
		ok++
	}
	fmt.Fprintf(c.out, "verify: %d files rebuilt from raw: ok=%d diff=%d gone=%d\n", len(rows), ok, c.bad, gone)
	return c.bad == 0, nil
}

func fileSum(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("sum: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(h, f, n); err != nil {
		return nil, fmt.Errorf("sum %s: %w", path, err)
	}
	return h.Sum(nil), nil
}

// check — completeness: every transcript tl --all --codex reads has its files row, loaded up to its last complete
// line, raw holds each of its lines once, and events has as many events as transcript.Read gives for those lines.
func (c *checker) check() (bool, error) {
	srcs, err := transcript.AllSources(c.home, time.Time{}, "", true, true)
	if err != nil {
		return false, err
	}
	files, err := c.rows("SELECT path, file_id, ingested, lines, dirty, ev_lines FROM " + c.db + ".files FINAL FORMAT TabSeparated")
	if err != nil {
		return false, err
	}
	raw, err := c.rows("SELECT file_id, count(), max(line) FROM " + c.db + ".raw GROUP BY file_id FORMAT TabSeparated")
	if err != nil {
		return false, err
	}
	events, err := c.rows("SELECT file_id, count() FROM " + c.db + ".events GROUP BY file_id FORMAT TabSeparated")
	if err != nil {
		return false, err
	}
	byPath := map[string][]string{}
	for _, r := range files {
		byPath[r[0]] = r
	}
	rawOf := map[string][2]string{}
	for _, r := range raw {
		rawOf[r[0]] = [2]string{r[1], r[2]}
	}
	evOf := map[string]string{}
	for _, r := range events {
		evOf[r[0]] = r[1]
	}
	for _, s := range srcs {
		r, ok := byPath[s.Path]
		if !ok {
			c.problem("missing\t%s", s.Path)
			continue
		}
		if err := c.checkFile(s.Path, r, rawOf[r[1]], evOf[r[1]]); err != nil {
			return false, err
		}
	}
	fmt.Fprintf(c.out, "check: %d transcripts: ok=%d problems=%d\n", len(srcs), len(srcs)-c.bad, c.bad)
	return c.bad == 0, nil
}

// lastLineEnd — the offset after the last '\n' of the file, read from its end.
func lastLineEnd(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("last line: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("last line: %w", err)
	}
	buf := make([]byte, 64<<10)
	for end := st.Size(); end > 0; {
		from := max(end-int64(len(buf)), 0)
		b := buf[:end-from]
		if _, err := f.ReadAt(b, from); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
			return from + int64(i) + 1, nil
		}
		end = from
	}
	return 0, nil
}

// materialize builds the text index of the events after a first load, waiting for the mutation to finish.
func (c *checker) materialize() error {
	ctx := context.Background()
	start := time.Now()
	if _, err := c.srv.query(ctx, "ALTER TABLE "+c.db+".events MATERIALIZE INDEX hay_ng", time.Minute); err != nil {
		return err
	}
	q := "SELECT count(), any(latest_fail_reason) FROM system.mutations WHERE database = '" + c.db + "' AND table = 'events' AND NOT is_done FORMAT TabSeparated"
	for {
		rows, err := c.rows(q)
		if err != nil {
			return err
		}
		if len(rows) == 1 && rows[0][0] == "0" {
			break
		}
		if len(rows) == 1 && rows[0][1] != "" {
			return fmt.Errorf("materialize index: %s", shorten(rows[0][1], 300))
		}
		time.Sleep(5 * time.Second)
	}
	fmt.Fprintf(c.out, "materialize: index hay_ng of %s.events built in %s\n", c.db, time.Since(start).Round(time.Second))
	return nil
}

// checkFile — one transcript against its files row r, its raw rows (count, max line) and its event count.
func (c *checker) checkFile(path string, r []string, raw [2]string, events string) error {
	ingested := r[2]
	p := &fields{row: r}
	lines, dirty, evLines := p.int(3), p.int(4), p.int(5)
	if p.err != nil {
		return fmt.Errorf("files: %w", p.err)
	}
	if dirty != 0 || evLines < lines {
		c.problem("state\t%s\tdirty %d, events loaded up to line %d of %d", path, dirty, evLines, lines)
		return nil
	}
	end, err := lastLineEnd(path)
	if err != nil {
		c.problem("unreadable\t%s: %v", path, err)
		return nil
	}
	if strconv.FormatInt(end, 10) != ingested {
		c.problem("ingested\t%s\tfiles %s, complete lines end at %d", path, ingested, end)
		return nil
	}
	if lines > 0 && raw != [2]string{r[3], r[3]} {
		c.problem("raw\t%s\t%d lines, raw has %s rows up to line %s", path, lines, raw[0], raw[1])
		return nil
	}
	n := 0
	if _, err := transcript.Read(path, nil, func(e transcript.Event) {
		if e.Line <= lines {
			n++
		}
	}); err != nil {
		c.problem("unreadable\t%s: %v", path, err)
		return nil
	}
	if events == "" {
		events = "0"
	}
	if events != strconv.Itoa(n) {
		c.problem("events\t%s\tevents %s, transcript.Read %d", path, events, n)
	}
	return nil
}
