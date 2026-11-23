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
	"time"
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
	// pass: inserts into these tables let through before fail applies; tried: inserts refused by fail
	pass, tried map[string]int
	down        bool
	kills       int // KILL QUERY statements
	// hold: inserts into these tables are kept unapplied and refused, to be sent again later (late)
	hold map[string]int
	late []*http.Request
	// pause: the next insert into the table waits until released
	pause map[string]*pause
}

type pause struct {
	arrived, release chan struct{}
	early            bool // the condition of the insert is read when it comes, its rows are written when released
}

// pauseNext makes the next insert into table wait; arrived closes when it comes, release lets it go on.
func (f *fakeCH) pauseNext(table string) *pause {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := &pause{arrived: make(chan struct{}), release: make(chan struct{})}
	if f.pause == nil {
		f.pause = map[string]*pause{}
	}
	f.pause[table] = p
	return p
}

// sent — one insert as the server got it.
type sent struct {
	table, token string
	query        string
	rows         [][]string
	settings     map[string]string
}

func newFake(t *testing.T) (*fakeCH, *server) {
	f := &fakeCH{rows: map[string][][]string{}, tokens: map[string]map[string]bool{}, fail: map[string]int{}, lose: map[string]int{},
		pass: map[string]int{}, tried: map[string]int{}, hold: map[string]int{}}
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
	deleteRe   = regexp.MustCompile(`^DELETE FROM \w+\.(\w+) WHERE file_id = (\d+)(?: AND line > (\d+))?(?: AND .*)?$`)
	fenceRe    = regexp.MustCompile(`\(file_id, gen\) IN \(((?:\(\d+,\d+\),?)+)\)`)
	pairRe     = regexp.MustCompile(`\((\d+),(\d+)\)`)
	filesSelRe = regexp.MustCompile(`^SELECT file_id, path, .* FROM \w+\.files FINAL`)
)

func (f *fakeCH) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	q := r.URL.Query().Get("query")
	if q == "" {
		q = string(body)
		body = nil
	}
	var pre [][]string // rows a paused insert let through when it came, the condition already read
	early := false
	if m := insertRe.FindStringSubmatch(q); m != nil {
		f.mu.Lock()
		p := f.pause[m[1]]
		delete(f.pause, m[1])
		if p != nil && p.early {
			pre, early = f.fenced(q, m[1], tsvRows(body)), true
		}
		f.mu.Unlock()
		if p != nil { // the request waits, the client alive, until the test lets it go on
			close(p.arrived)
			<-p.release
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		http.Error(w, "Code: 210. connection refused", http.StatusServiceUnavailable)
		return
	}
	switch {
	case insertRe.MatchString(q):
		table := insertRe.FindStringSubmatch(q)[1]
		if f.hold[table] > 0 {
			f.hold[table]--
			f.tried[table]++
			late := r.Clone(r.Context())
			late.Body = io.NopCloser(bytes.NewReader(body))
			f.late = append(f.late, late)
			http.Error(w, "Code: 999. held", http.StatusInternalServerError)
			return
		}
		if f.pass[table] > 0 {
			f.pass[table]--
		} else if f.fail[table] > 0 {
			f.fail[table]--
			f.tried[table]++
			http.Error(w, "Code: 999. refused", http.StatusInternalServerError)
			return
		}
		tok := r.URL.Query().Get("insert_deduplication_token")
		settings := map[string]string{}
		for k := range r.URL.Query() {
			settings[k] = r.URL.Query().Get(k)
		}
		rows := tsvRows(body)
		if table == "usage" && strings.Contains(q, "SELECT ") {
			var err error
			rows, err = f.usageFromEvents(q)
			if err != nil {
				http.Error(w, "fake: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		rows = f.fenced(q, table, rows)
		if early {
			rows = pre
		}
		w.Header().Set("X-ClickHouse-Summary", fmt.Sprintf(`{"read_rows":"0","written_rows":"%d"}`, len(rows)))
		f.inserts = append(f.inserts, sent{table: table, token: tok, query: q, rows: rows, settings: settings})
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
	case strings.HasPrefix(q, "CREATE "), strings.HasPrefix(q, "ALTER TABLE "):
	case strings.HasPrefix(q, "KILL QUERY "):
		f.kills++
	case q == "SELECT serverUUID()":
		fmt.Fprintln(w, "00000000-0000-4000-8000-0000000000fa")
	case genSelRe.MatchString(q):
		m := genSelRe.FindStringSubmatch(q)
		n := 0
		for _, row := range f.latestFiles() {
			if row[fileCol["file_id"]] == m[1] && row[fileCol["gen"]] == m[2] {
				n++
			}
		}
		fmt.Fprintln(w, n)
	case deleteRe.MatchString(q):
		m := deleteRe.FindStringSubmatch(q)
		if !f.genHolds(q) {
			return
		}
		f.deletes = append(f.deletes, m[1]+":"+m[2])
		from, _ := strconv.Atoi(m[3]) // no line condition: from 0, all rows
		idCol, lineCol := 0, 1        // raw
		if m[1] == "events" {
			idCol, lineCol = 1, 5
		}
		var kept [][]string
		for _, row := range f.rows[m[1]] {
			if m[1] == "usage" {
				if row[0] != m[2] {
					kept = append(kept, row)
				} else if strings.Contains(q, "toDateTime(ts) + INTERVAL") {
					at, err := time.Parse("2006-01-02 15:04:05.000", row[1])
					if err != nil {
						http.Error(w, "fake: "+err.Error(), http.StatusBadRequest)
						return
					}
					within, err := retainedByRetentionSQL(q, at, "", "")
					if err != nil {
						http.Error(w, "fake: "+err.Error(), http.StatusBadRequest)
						return
					}
					if !within {
						kept = append(kept, row)
					}
				}
				continue
			}
			if n, _ := strconv.Atoi(row[lineCol]); row[idCol] != m[2] || n <= from {
				kept = append(kept, row)
			}
		}
		f.rows[m[1]] = kept
	case filesSelRe.MatchString(q):
		for _, row := range f.latestFiles() {
			fmt.Fprintln(w, escapeRow(row))
		}
	default:
		http.Error(w, "fake: unknown statement "+q, http.StatusBadRequest)
	}
}

var heldRe = regexp.MustCompile(`file_id NOT IN \(SELECT file_id FROM \w+\.files FINAL WHERE file_id IN \([\d,]+\) AND \(file_id, gen\) NOT IN \(((?:\(\d+,\d+\),?)+)\)\)`)

// held — the files of rows (files rows) that have no row yet or whose latest row has the gen of pairs.
func (f *fakeCH) held(rows [][]string, pairs string) map[string]bool {
	gen := map[string]string{}
	for _, row := range f.latestFiles() {
		gen[row[fileCol["file_id"]]] = row[fileCol["gen"]]
	}
	want := map[string]string{}
	for _, p := range pairRe.FindAllStringSubmatch(pairs, -1) {
		want[p[1]] = p[2]
	}
	out := map[string]bool{}
	for _, r := range rows {
		id := r[fileCol["file_id"]]
		if g, ok := gen[id]; !ok || g == want[id] {
			out[id] = true
		}
	}
	return out
}

var genSelRe = regexp.MustCompile(`^SELECT count\(\) FROM \w+\.files FINAL WHERE file_id = (\d+) AND gen = (\d+)$`)

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

// genHolds — the generation condition of a statement holds: every file it names has that gen in its latest row.
func (f *fakeCH) genHolds(q string) bool {
	m := fenceRe.FindStringSubmatch(q)
	if m == nil {
		return true
	}
	allowed := f.allowed(m[1])
	for _, p := range pairRe.FindAllStringSubmatch(m[1], -1) {
		if !allowed[p[1]] {
			return false
		}
	}
	return true
}

// allowed — the files of pairs (id,gen) whose latest row has that gen.
func (f *fakeCH) allowed(pairs string) map[string]bool {
	gen := map[string]string{}
	for _, row := range f.latestFiles() {
		gen[row[fileCol["file_id"]]] = row[fileCol["gen"]]
	}
	ok := map[string]bool{}
	for _, p := range pairRe.FindAllStringSubmatch(pairs, -1) {
		if gen[p[1]] == p[2] {
			ok[p[1]] = true
		}
	}
	return ok
}

// fenced keeps the rows of an INSERT ... SELECT FROM input() WHERE <generation condition> that pass it.
func (f *fakeCH) fenced(q, table string, rows [][]string) [][]string {
	if !strings.Contains(q, "FROM input()") {
		return rows
	}
	var allowed map[string]bool
	if m := heldRe.FindStringSubmatch(q); m != nil {
		allowed = f.held(rows, m[1])
	} else if m := fenceRe.FindStringSubmatch(q); m != nil {
		allowed = f.allowed(m[1])
	} else {
		return rows
	}
	idCol := 0
	if table == "events" {
		idCol = 1
	}
	var out [][]string
	for _, r := range rows {
		if allowed[r[idCol]] {
			out = append(out, r)
		}
	}
	return out
}

// sendLate applies the held inserts now, as a server that runs the request of a stopped client late.
func (f *fakeCH) sendLate() {
	f.mu.Lock()
	late := f.late
	f.late = nil
	f.mu.Unlock()
	for _, r := range late {
		f.ServeHTTP(httptest.NewRecorder(), r)
	}
}

func (f *fakeCH) triedFailing(table string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tried[table]
}

func (f *fakeCH) set(fn func(*fakeCH)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}
