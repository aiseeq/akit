package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeCH answers the statements the loader sends, as ClickHouse would for these tables: inserts are kept by table,
// a repeated insert_deduplication_token is dropped, DELETE removes the rows of a file. It records every insert.
type fakeCH struct {
	mu      sync.Mutex
	rows    map[string][][]string // table → rows as inserted
	tokens  map[string]map[string]bool
	inserts []sent
	deletes []string
	// fail: the next inserts into these tables are refused before they are applied (the server is down);
	// lose: the next inserts are applied but the answer is an error (the answer was lost)
	fail, lose map[string]int
	down       bool
}

// sent — one insert as the server got it.
type sent struct {
	table, token string
	rows         [][]string
	settings     map[string]string
}

func newFake(t *testing.T) (*fakeCH, *server) {
	f := &fakeCH{rows: map[string][][]string{}, tokens: map[string]map[string]bool{}, fail: map[string]int{}, lose: map[string]int{}}
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	srv, err := newServer(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	return f, srv
}

var (
	insertRe   = regexp.MustCompile(`^INSERT INTO \w+\.(\w+) \(`)
	deleteRe   = regexp.MustCompile(`^DELETE FROM \w+\.(\w+) WHERE file_id = (\d+)$`)
	rawTailRe  = regexp.MustCompile(`^SELECT raw FROM \w+\.raw WHERE file_id = (\d+)(?: AND line > (\d+))? ORDER BY line FORMAT LineAsString$`)
	evMaxRe    = regexp.MustCompile(`^SELECT max\(line\) FROM \w+\.events WHERE file_id = (\d+)`)
	filesSelRe = regexp.MustCompile(`^SELECT file_id, path, .* FROM \w+\.files FINAL`)
)

func (f *fakeCH) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	q := r.URL.Query().Get("query")
	if q == "" {
		q = string(body)
		body = nil
	}
	if f.down {
		http.Error(w, "Code: 210. connection refused", http.StatusServiceUnavailable)
		return
	}
	switch {
	case insertRe.MatchString(q):
		table := insertRe.FindStringSubmatch(q)[1]
		if f.fail[table] > 0 {
			f.fail[table]--
			http.Error(w, "Code: 999. refused", http.StatusInternalServerError)
			return
		}
		tok := r.URL.Query().Get("insert_deduplication_token")
		settings := map[string]string{}
		for k := range r.URL.Query() {
			settings[k] = r.URL.Query().Get(k)
		}
		rows := tsvRows(body)
		f.inserts = append(f.inserts, sent{table: table, token: tok, rows: rows, settings: settings})
		if f.tokens[table] == nil {
			f.tokens[table] = map[string]bool{}
		}
		if tok == "" || !f.tokens[table][tok] {
			f.tokens[table][tok] = true
			f.rows[table] = append(f.rows[table], rows...)
		}
		if f.lose[table] > 0 {
			f.lose[table]--
			http.Error(w, "Code: 999. answer lost", http.StatusInternalServerError)
		}
	case strings.HasPrefix(q, "CREATE "):
	case deleteRe.MatchString(q):
		m := deleteRe.FindStringSubmatch(q)
		f.deletes = append(f.deletes, m[1]+":"+m[2])
		var kept [][]string
		col := 0
		if m[1] != "raw" {
			col = 1 // events and replies: ts first
		}
		for _, row := range f.rows[m[1]] {
			if row[col] != m[2] {
				kept = append(kept, row)
			}
		}
		f.rows[m[1]] = kept
	case filesSelRe.MatchString(q):
		for _, row := range f.latestFiles() {
			fmt.Fprintln(w, escapeRow(row))
		}
	case strings.HasPrefix(q, "SELECT file_id, max(line) FROM"):
		mx := map[string]int{}
		for _, row := range f.rows["raw"] {
			n, _ := strconv.Atoi(row[1])
			mx[row[0]] = max(mx[row[0]], n)
		}
		for id, n := range mx {
			fmt.Fprintf(w, "%s\t%d\n", id, n)
		}
	case rawTailRe.MatchString(q):
		m := rawTailRe.FindStringSubmatch(q)
		from, _ := strconv.Atoi(m[2])
		for _, row := range f.fileRaw(m[1]) {
			if n, _ := strconv.Atoi(row[1]); n > from {
				fmt.Fprintln(w, row[3])
			}
		}
	case evMaxRe.MatchString(q):
		id := evMaxRe.FindStringSubmatch(q)[1]
		mx := 0
		for _, row := range f.rows["events"] {
			if n, _ := strconv.Atoi(row[5]); row[1] == id {
				mx = max(mx, n)
			}
		}
		fmt.Fprintln(w, mx)
	default:
		http.Error(w, "fake: unknown statement "+q, http.StatusBadRequest)
	}
}

func escapeRow(row []string) string {
	var b bytes.Buffer
	for i, s := range row {
		if i > 0 {
			b.WriteByte('\t')
		}
		escapeString(&b, s)
	}
	return b.String()
}

// latestFiles — the files rows FINAL would give: the largest ver per file.
func (f *fakeCH) latestFiles() [][]string {
	best := map[string][]string{}
	for _, row := range f.rows["files"] {
		if old, ok := best[row[0]]; !ok || ver(row) >= ver(old) {
			best[row[0]] = row
		}
	}
	var out [][]string
	for _, row := range best {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

func ver(row []string) uint64 {
	n, _ := strconv.ParseUint(row[len(row)-1], 10, 64)
	return n
}

// fileRaw — raw rows of a file by line.
func (f *fakeCH) fileRaw(id string) [][]string {
	var out [][]string
	for _, row := range f.rows["raw"] {
		if row[0] == id {
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := strconv.Atoi(out[i][1])
		b, _ := strconv.Atoi(out[j][1])
		return a < b
	})
	return out
}

// rebuilt — the file as raw rebuilds it, and whether a line is there twice.
func (f *fakeCH) rebuilt(id uint64) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	seen := map[string]bool{}
	dup := false
	for _, row := range f.fileRaw(strconv.FormatUint(id, 10)) {
		dup = dup || seen[row[1]]
		seen[row[1]] = true
		b.WriteString(row[3] + "\n")
	}
	return b.String(), dup
}

// events — events rows of a file as "line role who: text", in line order.
func (f *fakeCH) events(id uint64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, row := range f.rows["events"] {
		if row[1] == strconv.FormatUint(id, 10) {
			out = append(out, row[5]+" "+row[6]+" "+row[7]+": "+row[8])
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := strconv.Atoi(strings.Fields(out[i])[0])
		b, _ := strconv.Atoi(strings.Fields(out[j])[0])
		return a < b
	})
	return out
}

func (f *fakeCH) sentTo(table string) []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sent
	for _, s := range f.inserts {
		if s.table == table {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeCH) set(fn func(*fakeCH)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}
