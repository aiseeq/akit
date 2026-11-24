package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/session"
	"github.com/aiseeq/akit/transcript"
	"github.com/aiseeq/akit/transcriptdb"
)

// fakeEvent and fakeFile are the rows the loader would write for a synthetic transcript: text as Shown(), the
// number of images, every line as is in raw.
type fakeEvent struct {
	fileID uint64
	path   string
	line   int
	ts     time.Time
	role   string
	who    string
	text   string
	images int
}

type fakeCorpus struct {
	events []fakeEvent
	raw    map[[2]uint64]string
	bad    map[string]int
}

// loadFakeCorpus reads every session transcript under home as the loader does.
func loadFakeCorpus(t *testing.T, home string) fakeCorpus {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(session.ProjectsDir(home), "*", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	c := fakeCorpus{raw: map[[2]uint64]string{}, bad: map[string]int{}}
	for i, path := range paths {
		id := uint64(100 + i)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for n, raw := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
			c.raw[[2]uint64{id, uint64(n + 1)}] = raw
		}
		meta, err := transcript.Read(path, nil, func(ev transcript.Event) {
			c.events = append(c.events, fakeEvent{fileID: id, path: path, line: ev.Line, ts: ev.Time, role: string(ev.Role),
				who: ev.Who, text: ev.Shown(), images: ev.Images})
		})
		if err != nil {
			t.Fatal(err)
		}
		c.bad[path] = meta.Bad
	}
	return c
}

// fakeCorpusDB serves the queries of user-word from the synthetic transcripts under e.home, so the same command
// can run through the files and through the database and its stdout be compared.
func fakeCorpusDB(t *testing.T, e env) {
	t.Helper()
	c := loadFakeCorpus(t, e.home)
	healthyLoader(t, e.home, time.Now())
	fakeDB(t, func(sql string, params map[string]string) string {
		rows, err := c.answer(sql, params)
		if err != nil {
			t.Errorf("fake database: %v in %s", err, sql)
			return ""
		}
		var b strings.Builder
		for _, r := range rows {
			for i, field := range r {
				if i > 0 {
					b.WriteByte('\t')
				}
				b.WriteString(transcriptdb.TSVEscape(field))
			}
			b.WriteByte('\n')
		}
		return b.String()
	})
}

func (c fakeCorpus) answer(sql string, params map[string]string) ([][]string, error) {
	switch {
	case strings.Contains(sql, " FROM raw "):
		return c.rawRows(sql, params)
	case strings.Contains(sql, "SELECT DISTINCT files.path"):
		return c.candidates(params)
	case strings.Contains(sql, "countIf("):
		return c.counts(sql, params)
	}
	var keep []fakeEvent
	roles := []string{"user", "ask"}
	if strings.Contains(sql, "'agent'") {
		roles = append(roles, "agent")
	}
	path, byPath := params["path"]
	terms, err := arrayParam(params["terms"])
	if err != nil {
		return nil, err
	}
	since, err := timeParam(params["since"])
	if err != nil {
		return nil, err
	}
	for _, ev := range c.events {
		if !slices.Contains(roles, ev.role) || byPath && ev.path != transcriptdb.TSVUnescape(path) ||
			!since.IsZero() && ev.ts.Before(since) || len(terms) > 0 && !ev.hayHasAny(terms) {
			continue
		}
		keep = append(keep, ev)
	}
	if !byPath {
		slices.SortStableFunc(keep, func(a, b fakeEvent) int {
			if d := a.ts.Compare(b.ts); d != 0 {
				return d
			}
			if d := strings.Compare(a.path, b.path); d != 0 {
				return d
			}
			return a.line - b.line
		})
	}
	columns, err := selectColumns(sql)
	if err != nil {
		return nil, err
	}
	var rows [][]string
	for _, ev := range keep {
		r := make([]string, len(columns))
		for i, col := range columns {
			if r[i], err = c.value(ev, col); err != nil {
				return nil, err
			}
		}
		rows = append(rows, r)
	}
	return rows, nil
}

func (ev fakeEvent) hayHasAny(terms []string) bool {
	hay := strings.ToLower(ev.who + " " + ev.text)
	for _, term := range terms {
		if strings.Contains(hay, term) {
			return true
		}
	}
	return false
}

func (c fakeCorpus) value(ev fakeEvent, column string) (string, error) {
	for _, prefix := range []string{"events.", "files.", "f."} {
		column = strings.ReplaceAll(column, prefix, "")
	}
	switch column {
	case "path":
		return ev.path, nil
	case "toString(file_id)":
		return strconv.FormatUint(ev.fileID, 10), nil
	case "toString(line)":
		return strconv.Itoa(ev.line), nil
	case "toString(toUnixTimestamp64Milli(ts))":
		return strconv.FormatInt(ev.ts.UnixMilli(), 10), nil
	case "role":
		return ev.role, nil
	case "text":
		return ev.text, nil
	case "toString(images)":
		return strconv.Itoa(ev.images), nil
	case "toString(bad)":
		return strconv.Itoa(c.bad[ev.path]), nil
	}
	return "", fmt.Errorf("column %q", column)
}

func (c fakeCorpus) rawRows(sql string, params map[string]string) ([][]string, error) {
	if !strings.HasPrefix(sql, "SELECT toString(file_id), toString(line), raw FROM raw ") {
		return nil, fmt.Errorf("raw query")
	}
	var rows [][]string
	for item := range strings.SplitSeq(strings.Trim(params["keys"], "[]"), "),(") {
		id, line, ok := strings.Cut(strings.Trim(item, "()"), ",")
		file, err1 := strconv.ParseUint(id, 10, 64)
		n, err2 := strconv.ParseUint(line, 10, 32)
		if !ok || err1 != nil || err2 != nil {
			return nil, fmt.Errorf("raw key %q", item)
		}
		if raw, ok := c.raw[[2]uint64{file, n}]; ok {
			rows = append(rows, []string{id, line, raw})
		}
	}
	return rows, nil
}

// candidates — the paths with a user or ask line in a marker day that has one of the day's terms.
func (c fakeCorpus) candidates(params map[string]string) ([][]string, error) {
	paths := map[string]bool{}
	for i := 0; params[fmt.Sprintf("s%d", i)] != ""; i++ {
		start, err1 := timeParam(params[fmt.Sprintf("s%d", i)])
		end, err2 := timeParam(params[fmt.Sprintf("e%d", i)])
		terms, err3 := arrayParam(params[fmt.Sprintf("t%d", i)])
		if err1 != nil || err2 != nil || err3 != nil {
			return nil, fmt.Errorf("candidate params %v", params)
		}
		for _, ev := range c.events {
			if (ev.role == "user" || ev.role == "ask") && !ev.ts.Before(start) && ev.ts.Before(end) && (len(terms) == 0 || ev.hayHasAny(terms)) {
				paths[ev.path] = true
			}
		}
	}
	var rows [][]string
	for _, p := range slices.Sorted(func(yield func(string) bool) {
		for p := range paths {
			if !yield(p) {
				return
			}
		}
	}) {
		rows = append(rows, []string{p})
	}
	return rows, nil
}

// counts — the events in each record window.
func (c fakeCorpus) counts(sql string, params map[string]string) ([][]string, error) {
	n := strings.Count(sql, "countIf(")
	row := make([]string, n)
	for i := range n {
		start, err1 := timeParam(params[fmt.Sprintf("s%d", i)])
		end, err2 := timeParam(params[fmt.Sprintf("e%d", i)])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("count params %v", params)
		}
		count := 0
		for _, ev := range c.events {
			if !ev.ts.Before(start) && ev.ts.Before(end) {
				count++
			}
		}
		row[i] = strconv.Itoa(count)
	}
	return [][]string{row}, nil
}

// selectColumns — the expressions between SELECT and FROM, split at the commas outside parentheses.
func selectColumns(sql string) ([]string, error) {
	list, _, ok := strings.Cut(strings.TrimPrefix(sql, "SELECT "), " FROM ")
	if !ok {
		return nil, fmt.Errorf("no FROM")
	}
	var out []string
	depth, start := 0, 0
	for i, r := range list {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(list[start:i]))
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(list[start:])), nil
}

func arrayParam(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	var cur strings.Builder
	in := false
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; {
		case in && ch == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case ch == '\'':
			if in {
				out = append(out, cur.String())
				cur.Reset()
			}
			in = !in
		case in:
			cur.WriteByte(ch)
		}
	}
	return out, nil
}

func timeParam(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse("2006-01-02 15:04:05.000", s)
}
