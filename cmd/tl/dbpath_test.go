package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiseeq/akit/price"
	"github.com/aiseeq/akit/transcript"
	"github.com/aiseeq/akit/transcript/waitrec"
	"github.com/aiseeq/akit/transcriptdb"
)

// fakeCH answers the queries of the database path the way ClickHouse would over tables the loader filled from
// the transcripts of a synthetic home: it evaluates the conditions the client sends (parameters, LIKE, the regular
// expressions, the raw prefilter) in Go. Every request must be a read-only POST.
type fakeCH struct {
	t            *testing.T
	files        []*chFile
	mu           sync.Mutex
	names        []string // log_comment of each request, in order
	eventWhole   []string // whole-text limits used by the event listing
	eventSQL     []string // event projections, for the text-preservation contract
	eventRunes   []int    // the characters of each event text the listing answers carried, in order
	contextFiles [][]string
	usageSQL     []string
	gate         chan struct{} // when set, requests wait for it to close
	gateName     string
	eventStarted chan struct{}
	fail         map[string]bool
	legacyUsage  bool
	noFlatLen    bool // the events table has rows loaded before flat_len, with 0 there
}

type chFile struct {
	id                          uint64
	path, name, desc, kind, cwd string
	session, parent, project    string
	mtime                       time.Time
	first                       time.Time
	lines, evLines              int
	loaded                      time.Time
	events                      []chEvent
	raw                         []string // the lines, from 1
	bad                         int
}

type chEvent struct {
	ev  transcript.Event
	seq uint64
	off uint64
	// flatLen is flat_len as the loader writes it: the characters of transcript.Short of the shown text
	flatLen int
}

// wantEventTextSQL is the projection of the text of a listing the fake knows: what it answers is eventText, and
// TestLiveEventText holds that a real server answers the same to it.
const wantEventTextSQL = "if((lengthUTF8(text) <= {cut:UInt32} OR (flat_len = 0 AND lengthUTF8(text) <= {whole:UInt32})) AS whole_text, " +
	"text, substringUTF8(text, 1, {cut:UInt32})), if(whole_text, 0, if(flat_len > 0, flat_len, " +
	`lengthUTF8(arrayStringConcat(arrayFilter(x -> x != '', splitByRegexp({ws:String}, ` +
	`replaceAll(replaceRegexpAll(text, {trim:String}, ''), '\n', ' ⏎ '))), ' '))))`

// eventText is the text and the flattened length the listing query gives for an event: a text up to cut, or up to
// whole without a stored length, whole with 0; a longer one as its first cut characters with the stored length,
// or the counted one where none is stored.
func eventText(text string, flatLen, cut, whole int) (string, int) {
	r := []rune(text)
	if len(r) <= cut || flatLen == 0 && len(r) <= whole {
		return text, 0
	}
	if flatLen == 0 {
		flatLen = len([]rune(transcript.Short(text, 0)))
	}
	return string(r[:cut]), flatLen
}

func TestSortHitsUsesSequenceBeforeRoleAndPart(t *testing.T) {
	base := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	makeHit := func(seq uint64, role transcript.Role, part string, off uint64) sortedHit {
		return sortedHit{hit: hit{src: source{Path: "/synthetic/transcript.jsonl"}, ev: transcript.Event{Time: base, Line: 4, Role: role}},
			seq: seq, rank: roleRank[role], part: part, off: off}
	}
	hits := []sortedHit{
		makeHit(2, transcript.User, "a", 0),
		makeHit(1, transcript.Agent, "z", 9),
		makeHit(1, transcript.Cmd, "a", 0),
	}
	sortHits(hits)
	if got := []transcript.Role{hits[0].ev.Role, hits[1].ev.Role, hits[2].ev.Role}; !slices.Equal(got, []transcript.Role{transcript.Agent, transcript.Cmd, transcript.User}) {
		t.Fatalf("order by seq then role/part = %v", got)
	}
	hits = []sortedHit{makeHit(1, transcript.Cmd, "z", 9), makeHit(1, transcript.Agent, "a", 0)}
	sortHits(hits)
	if hits[0].ev.Role != transcript.Agent || hits[1].ev.Role != transcript.Cmd {
		t.Fatalf("equal seq did not keep role rank first: %v, %v", hits[0].ev.Role, hits[1].ev.Role)
	}
	hits = []sortedHit{makeHit(1, transcript.Agent, "z", 9), makeHit(1, transcript.Agent, "a", 0)}
	sortHits(hits)
	if hits[0].part != "a" || hits[1].part != "z" {
		t.Fatalf("equal seq and role did not use part/off: %+v", hits)
	}
}

// newFakeCH loads the transcripts; unloaded names files whose last lines are absent from the tables.
func newFakeCH(t *testing.T, paths []string, unloaded map[string]int) *fakeCH {
	t.Helper()
	s := &fakeCH{t: t}
	for i, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.SplitAfter(string(data), "\n")
		if lines[len(lines)-1] == "" || !strings.HasSuffix(lines[len(lines)-1], "\n") {
			lines = lines[:len(lines)-1]
		}
		n := len(lines) - unloaded[path]
		f := &chFile{id: uint64(i+1) << 22, path: path, lines: n, evLines: n, loaded: time.Now()}
		for _, l := range lines {
			f.raw = append(f.raw, strings.TrimRight(l, "\r\n"))
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		f.mtime = st.ModTime()
		f.project = filepath.Base(filepath.Dir(path))
		f.session = strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if findUUID(filepath.Base(path)) != "" {
			f.session = sourceFullID(source{Path: path, ID: path})[:8]
		}
		switch {
		case waitrec.IsRollout(path) && strings.Contains(path, string(filepath.Separator)+"worker"+string(filepath.Separator)):
			f.kind = "codex"
		case waitrec.IsRollout(path):
			f.kind = transcriptdb.KindCodexExt
		case strings.Contains(path, string(filepath.Separator)+"subagents"+string(filepath.Separator)):
			f.kind = "sub"
			f.session = strings.TrimPrefix(f.session, "agent-")
			f.parent = filepath.Base(filepath.Dir(filepath.Dir(path)))
			f.parent = f.parent[:min(8, len(f.parent))]
		default:
			f.kind = "claude"
		}
		// Only events confirmed through evLines are in the fake events table.
		seqByLine := map[int]uint64{}
		meta, err := transcript.Read(path, nil, func(ev transcript.Event) {
			if ev.Line <= n {
				seq := seqByLine[ev.Line]
				seqByLine[ev.Line]++
				f.events = append(f.events, chEvent{ev: ev, seq: seq, off: uint64(len(f.events)), flatLen: len([]rune(transcript.Short(ev.Shown(), 0)))})
				if f.first.IsZero() || ev.Time.Before(f.first) {
					f.first = ev.Time
				}
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		if src, err := transcript.SourceAt(path); err == nil {
			f.desc = src.Description
		}
		if transcriptdb.IsCodexKind(f.kind) {
			f.cwd, _ = transcript.CodexProjectCWD(path)
		}
		f.name, f.bad = meta.Name, meta.Bad
		if n < len(lines) {
			// the loader read the file when its last loaded line was new
			var last time.Time
			for _, e := range f.events {
				if e.ev.Line <= n && e.ev.Time.After(last) {
					last = e.ev.Time
				}
			}
			f.loaded = last.Add(time.Second)
		}
		s.files = append(s.files, f)
	}
	return s
}

func (s *fakeCH) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.names)
}

func (s *fakeCH) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	sql := string(body)
	if r.Method != http.MethodPost || q.Get("readonly") != "1" {
		s.t.Errorf("request %s without readonly=1: %s", r.Method, r.URL.RawQuery)
		http.Error(w, "Code: 164. DB::Exception: readonly", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("Accept-Encoding") != "identity" {
		s.t.Errorf("request enabled HTTP compression: %q", r.Header.Get("Accept-Encoding"))
	}
	for _, word := range []string{"INSERT", "ALTER", "OPTIMIZE", "DROP", "CREATE", "SYSTEM"} {
		if strings.Contains(strings.ToUpper(sql), word+" ") {
			s.t.Errorf("a query that writes: %s", sql)
		}
	}
	name := strings.TrimPrefix(q.Get("log_comment"), "tl:")
	s.mu.Lock()
	s.names = append(s.names, name)
	if name == "events" {
		s.eventWhole = append(s.eventWhole, q.Get("param_whole"))
		s.eventSQL = append(s.eventSQL, sql)
		if s.eventStarted != nil {
			select {
			case s.eventStarted <- struct{}{}:
			default:
			}
		}
	}
	if name == "events-context-events" {
		s.contextFiles = append(s.contextFiles, chArray(q.Get("param_files")))
	}
	if name == "usage" {
		s.usageSQL = append(s.usageSQL, sql)
	}
	gate := s.gate
	s.mu.Unlock()
	if gate != nil && (s.gateName == "" || name == s.gateName) {
		<-gate
	}
	s.mu.Lock()
	fail := s.fail[name]
	s.mu.Unlock()
	if fail {
		http.Error(w, "Code: 999. DB::Exception: synthetic query failure", http.StatusInternalServerError)
		return
	}
	var out bytes.Buffer
	row := func(fields ...any) {
		for i, f := range fields {
			if i > 0 {
				out.WriteByte('\t')
			}
			out.WriteString(tsvEscape(fmt.Sprint(f)))
		}
		out.WriteByte('\n')
	}
	param := func(name string) string { return q.Get("param_" + name) }
	switch name {
	case "usage_legacy":
		// what a query of the usage table asks first
		if s.legacyUsage {
			row(1)
		} else {
			row(0)
		}
	case "files":
		for _, f := range s.files {
			if !fakeFileMatches(q, sql, f) {
				continue
			}
			row(f.id, f.path, f.name, f.desc, f.lines, f.evLines, f.loaded.UnixMilli(), f.bad,
				f.mtime.Unix()*1000, ms(f.first), f.session, f.parent, f.kind, f.project, f.cwd)
		}
	case "raw-hidden":
		s.rawHidden(q, sql, row)
	case "link_names":
		for _, f := range s.files {
			if f.kind == transcriptdb.KindCodexExt && strings.Contains(sql, "WHERE kind != 'codex-ext'") {
				continue
			}
			row(f.path, f.session, f.kind)
		}
	case "shadow_files":
		for _, f := range s.files {
			row(f.path, f.lines)
		}
	case "usage":
		for _, u := range s.usageAnswer(q, sql) {
			if strings.Contains(sql, " FROM usage") {
				row(u.file.id, u.at, u.session, u.model, u.reply, u.request, u.Input, u.Output, u.CacheRead, u.CacheWrite, u.Write5m, u.Write1h, u.Fast, u.Geo)
			} else {
				row(u.file.id, u.legacy, u.model, u.reply, u.request, u.Input, u.Output, u.CacheRead, u.CacheWrite, u.Write5m, u.Write1h, u.Fast, u.Geo, u.at, u.line)
			}
		}
	case "events", "context-matches", "events-context-events":
		matchedFiles := map[uint64]bool{}
		for _, f := range s.files {
			for _, e := range f.events {
				if !s.match(q, sql, f, e) {
					continue
				}
				if name == "context-matches" {
					if !matchedFiles[f.id] {
						row(f.id)
						matchedFiles[f.id] = true
					}
					continue
				}
				ev := e.ev
				text, flat := ev.Shown(), 0
				if strings.Contains(sql, wantEventTextSQL) {
					cut, _ := strconv.Atoi(param("cut"))
					whole, _ := strconv.Atoi(param("whole"))
					stored := e.flatLen
					if s.noFlatLen {
						stored = 0
					}
					text, flat = eventText(text, stored, cut, whole)
				} else if !strings.Contains(sql, "SELECT file_id, line, seq, toUnixTimestamp64Milli(ts), role, who, subtype, text, 0, _part") {
					s.t.Errorf("an event projection the fake does not know: %s", sql)
				}
				s.mu.Lock()
				s.eventRunes = append(s.eventRunes, utf8.RuneCountInString(text))
				s.mu.Unlock()
				values := []any{f.id, ev.Line, e.seq, ms(ev.Time), ev.Role, ev.Who, ev.Subtype, text, flat, "all_1_1_0", e.off}
				if name == "events-context-events" {
					matched := "0"
					if re2Match(tsvUnescape(param("grep")), ev.Who+" "+ev.Shown()) {
						matched = "1"
					}
					values = append(values, matched)
				}
				row(values...)
			}
		}
	case "bad":
		for _, f := range s.files {
			if !slices.Contains(chArray(param("files")), strconv.FormatUint(f.id, 10)) {
				continue
			}
			for i, raw := range f.raw {
				row(f.id, i+1, raw)
			}
		}
	case "texts":
		pairs := map[[2]uint64]bool{}
		for _, m := range regexp.MustCompile(`\((\d+),(\d+)\)`).FindAllStringSubmatch(param("pairs"), -1) {
			a, _ := strconv.ParseUint(m[1], 10, 64)
			b, _ := strconv.ParseUint(m[2], 10, 64)
			pairs[[2]uint64{a, b}] = true
		}
		for _, f := range s.files {
			for _, e := range f.events {
				if pairs[[2]uint64{f.id, uint64(e.ev.Line)}] {
					row(f.id, e.ev.Line, e.ev.Role, "all_1_1_0", e.off, e.ev.Shown())
				}
			}
		}
	case "event":
		for _, f := range s.files {
			for _, e := range f.events {
				if strconv.FormatUint(f.id, 10) == param("file") && strconv.Itoa(e.ev.Line) == param("line") {
					row(ms(e.ev.Time), e.seq, e.ev.Role, e.ev.Who, e.ev.Shown(), "all_1_1_0", e.off)
				}
			}
		}
	default:
		s.t.Errorf("unknown query %q: %s", name, sql)
		http.Error(w, "Code: 62. DB::Exception: unknown", http.StatusBadRequest)
		return
	}
	_, _ = w.Write(out.Bytes())
}

// rawHidden answers rawSummarySQL from the synthetic files: what the server would count, so the tests of the
// summary line run on it; the text of the query itself is checked by TestRawSummarySQL.
func (s *fakeCH) rawHidden(q url.Values, sql string, row func(...any)) {
	if strings.Count(sql, "FROM raw WHERE") != 1 || !strings.Contains(sql, "groupBitOrIf") || !strings.Contains(sql, "transform(file_id") {
		s.t.Errorf("raw-hidden query is not the one-pass summary: %s", sql)
	}
	param := func(name string) string { return q.Get("param_" + name) }
	// the events of every role pass s.match; the bits of the roles follow role_names
	all := url.Values{}
	maps.Copy(all, q)
	all.Set("param_roles", param("role_names"))
	names := chArray(param("role_names"))
	visible, err := strconv.ParseUint(param("visible"), 10, 16)
	if err != nil {
		s.t.Errorf("raw-hidden visible = %q: %v", param("visible"), err)
	}
	type lineKey struct {
		file uint64
		line int
	}
	matched := map[lineKey]bool{}
	bits := map[lineKey]uint64{}
	for _, f := range s.files {
		for i := range min(f.evLines, len(f.raw)) {
			if s.rawMatch(q, f, i+1) {
				matched[lineKey{f.id, i + 1}] = true
			}
		}
		for _, event := range f.events {
			key := lineKey{f.id, event.ev.Line}
			if s.match(all, sql, f, event) {
				bits[key] |= 1 << slices.Index(names, string(event.ev.Role))
			}
		}
	}
	count, roles := 0, uint64(0)
	for key := range matched {
		if bits[key] != 0 && bits[key]&visible == 0 {
			count++
			roles |= bits[key]
		}
	}
	row(count, roles)
}

func (s *fakeCH) rawMatch(q url.Values, f *chFile, line int) bool {
	if line < 1 || line > len(f.raw) || line > f.evLines {
		return false
	}
	param := func(name string) string { return q.Get("param_" + name) }
	raw := f.raw[line-1]
	for i := 0; param(fmt.Sprintf("rawlit%d", i)) != ""; i++ {
		if !slices.ContainsFunc(chArray(param(fmt.Sprintf("rawlit%d", i))), func(lit string) bool {
			return strings.Contains(strings.ToLower(raw), strings.ToLower(lit))
		}) {
			return false
		}
	}
	return param("raw") != "" && regexp.MustCompile(tsvUnescape(param("raw"))).MatchString(raw)
}

func fakeFileMatches(q url.Values, sql string, f *chFile) bool {
	param := func(name string) string { return q.Get("param_" + name) }
	selected := false
	for _, path := range chArray(param("find_paths")) {
		selected = selected || f.path == path
	}
	for i := 0; ; i++ {
		id := param(fmt.Sprintf("find_id_%d", i))
		if id == "" {
			break
		}
		selected = selected || f.kind != transcriptdb.KindCodexExt && (strings.HasPrefix(f.session, id) || strings.Contains(f.path, id))
	}
	if (param("find_paths") != "" || param("find_id_0") != "") && !selected {
		return false
	}
	if v := param("file_since"); v != "" && f.mtime.Before(chParseTime(nil, v)) {
		return false
	}
	if v := param("file_until"); v != "" && f.first.After(chParseTime(nil, v)) {
		return false
	}
	if strings.Contains(sql, " AND kind != 'codex' AND") && f.kind == "codex" || strings.Contains(sql, " AND kind != 'sub' AND") && f.kind == "sub" ||
		strings.Contains(sql, " AND kind != 'codex-ext'") && f.kind == transcriptdb.KindCodexExt {
		return false
	}
	if project := param("project"); project != "" {
		if f.kind == "codex" || f.kind == transcriptdb.KindCodexExt && strings.Contains(sql, "kind IN ('codex', 'codex-ext') AND") {
			pathMatch := strings.Contains(sql, "position(path, {project:String})") && strings.Contains(f.path, project)
			cwdMatch := strings.Contains(sql, "position(cwd, {project:String})") && strings.Contains(f.cwd, project)
			if !pathMatch && !cwdMatch {
				return false
			}
		} else {
			pathMatch := strings.Contains(sql, "position(path, {project:String})") && strings.Contains(f.path, project)
			projectMatch := strings.Contains(sql, "position(project, {project:String})") && strings.Contains(f.project, project)
			if !pathMatch && !projectMatch {
				return false
			}
		}
	}
	return true
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// chUsage are the usage columns of one event.
type chUsage struct {
	file                           *chFile
	at                             int64
	line                           int
	seq                            uint64
	legacy                         int
	session, model, reply, request string
	price.Usage
}

// usageColumns are the usage columns of an event as the loader fills them: zero where it has no usage.
func (s *fakeCH) usageColumns(f *chFile, e chEvent) chUsage {
	c := chUsage{file: f, at: ms(e.ev.Time), line: e.ev.Line, seq: e.seq, session: f.session}
	if transcriptdb.IsCodexKind(f.kind) {
		c.session = f.path
	} else if f.parent != "" {
		c.session = f.parent
	}
	if r := e.ev.Reply; r != nil {
		if r.Usage != nil {
			c.model, c.reply, c.request, c.Usage = r.Model, r.ID, r.RequestID, *r.Usage
		}
	} else if u := e.ev.Tokens.PriceUsage(); u != nil && !s.legacyUsage {
		c.model, c.reply, c.Usage = e.ev.Tokens.Model, fmt.Sprintf("codex:%d", e.ev.Line), *u
	}
	if s.legacyUsage {
		c.legacy = 1
	}
	return c
}

// usageAnswer is the answer to the usage query: the versions of usage in the events it selects. A query of the
// usage table gets the rows that table holds: it is a ReplacingMergeTree, and an insert leaves one row of a key —
// the one with the most output, the last inserted of equal ones.
func (s *fakeCH) usageAnswer(q url.Values, sql string) []chUsage {
	type key struct{ session, reply, request string }
	var inserted []key
	table := map[key]chUsage{}
	asked := map[key]bool{}
	canonical := func(c chUsage) key {
		if c.reply != "" {
			return key{c.session, c.reply, ""}
		}
		return key{c.session, "", c.request}
	}
	var versions []chUsage
	seen := map[string]int{}
	for _, f := range s.files {
		for _, e := range f.events {
			c := s.usageColumns(f, e)
			if c.reply != "" || c.request != "" {
				k := key{c.session, c.reply, c.request}
				old, ok := table[k]
				if !ok {
					inserted = append(inserted, k)
				}
				if !ok || c.Output >= old.Output {
					table[k] = c
				}
			}
			if !s.match(q, sql, f, e) || !usageSelected(sql, c, e.ev.Who) {
				continue
			}
			asked[canonical(c)] = true
			group := fmt.Sprintf("%d %d %q %q %q %+v", f.id, c.legacy, c.model, c.reply, c.request, c.Usage)
			if c.reply == "" && c.request == "" && strings.Contains(sql, "if(reply_id = '' AND request_id = '', (line, seq), (0, 0))") {
				group += fmt.Sprintf(" %d %d", c.line, c.seq)
			}
			i, ok := seen[group]
			switch {
			case !ok:
				seen[group] = len(versions)
				versions = append(versions, c)
			case strings.Contains(sql, "min((ts, line))") == (cmp.Or(cmp.Compare(c.at, versions[i].at), cmp.Compare(c.line, versions[i].line)) < 0):
				versions[i] = c
			}
		}
	}
	if !strings.Contains(sql, " FROM usage") {
		// as on the server, the order of the groups is not the order of the events
		slices.Reverse(versions)
		return versions
	}
	_, outer, _ := strings.Cut(sql, "(reply_id != '' OR request_id != ''))")
	var rows []chUsage
	for _, k := range inserted {
		c := table[k]
		at := time.UnixMilli(c.at)
		if !asked[canonical(c)] || strings.Contains(outer, "ts >= {since") && at.Before(chParseTime(s.t, q.Get("param_since"))) ||
			strings.Contains(outer, "ts <= {until") && at.After(chParseTime(s.t, q.Get("param_until"))) {
			continue
		}
		rows = append(rows, c)
	}
	return rows
}

// usageSelected is the part of the WHERE of a usage query that keeps the events with usage.
func usageSelected(sql string, c chUsage, who string) bool {
	return strings.Contains(sql, "reply_id != ''") && c.reply != "" || strings.Contains(sql, "request_id != ''") && c.request != "" ||
		strings.Contains(sql, "model != ''") && c.model != "" ||
		strings.Contains(sql, "(kind IN ('codex', 'codex-ext') AND who = 'token_count')") && transcriptdb.IsCodexKind(c.file.kind) && who == "token_count"
}

// match is the WHERE of the events queries: the same conditions ClickHouse evaluates.
func (s *fakeCH) match(q url.Values, sql string, f *chFile, e chEvent) bool {
	param := func(name string) string { return q.Get("param_" + name) }
	ev := e.ev
	if !slices.Contains(chArray(param("files")), strconv.FormatUint(f.id, 10)) || !slices.Contains(chArray(param("kinds")), f.kind) ||
		!slices.Contains(chArray(param("roles")), string(ev.Role)) {
		return false
	}
	ts := time.UnixMilli(ms(ev.Time))
	if v := param("since"); v != "" {
		at := chParseTime(s.t, v)
		if strings.Contains(sql, "ts >= {since:") && ts.Before(at) || strings.Contains(sql, "ts > {since:") && !ts.After(at) {
			return false
		}
	}
	if v := param("until"); v != "" {
		at := chParseTime(s.t, v)
		if strings.Contains(sql, "ts <= {until:") && ts.After(at) || strings.Contains(sql, "ts < {until:") && !ts.Before(at) {
			return false
		}
	}
	text := ev.Who + " " + ev.Shown()
	hay := lowerUTF8(text)
	grep := sql
	if _, conditions, ok := strings.Cut(grep, " FROM events WHERE "); ok {
		grep = conditions
	}
	if i := strings.Index(grep, "(file_id, line) IN (SELECT"); i >= 0 {
		grep = grep[:i]
	}
	// every condition over the text the query has must hold; the server's own semantics of each
	if !hayPrefilterHolds(s.t, grep, hay, param) {
		return false
	}
	if strings.Contains(grep, "multiMatchAny(hay") && !slices.ContainsFunc(chArray(param("grep")), func(p string) bool { return hyperscanMatch(p, hay) }) {
		return false
	}
	if strings.Contains(grep, "match(hay, {grep:String})") && !re2Match(tsvUnescape(param("grep")), hay) {
		return false
	}
	if strings.Contains(grep, "match(concat(who, ' ', text), {grep:String})") && !re2Match(tsvUnescape(param("grep")), text) {
		return false
	}
	if strings.Contains(sql, "FROM raw WHERE") && param("raw") != "" {
		raw := f.raw[ev.Line-1]
		for i := 0; param(fmt.Sprintf("rawlit%d", i)) != ""; i++ {
			if !slices.ContainsFunc(chArray(param(fmt.Sprintf("rawlit%d", i))), func(lit string) bool {
				return strings.Contains(strings.ToLower(raw), strings.ToLower(lit))
			}) {
				return false
			}
		}
		if !regexp.MustCompile(tsvUnescape(param("raw"))).MatchString(raw) {
			return false
		}
	}
	return true
}

// lowerUTF8 is the server's lowerUTF8: full lower case, where İ is i with a combining dot and a final Σ is ς.
func lowerUTF8(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		switch {
		case r == 'İ':
			b.WriteString("i\u0307")
		case r == 'Σ' && i > 0 && unicode.IsLetter(runes[i-1]) && (i+1 == len(runes) || !unicode.IsLetter(runes[i+1])):
			b.WriteRune('ς')
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// hayChoiceSQL is one choice of a prefilter group over hay: a LIKE or the trigrams of the ngram index.
const hayChoiceSQL = `(?:hay LIKE \{(\w+):String\}|hasAllTokens\(hay, \{(\w+):Array\(String\)\}\))`

var (
	hayChoiceRe = regexp.MustCompile(hayChoiceSQL)
	hayGroupRe  = regexp.MustCompile(`\(` + hayChoiceSQL + `(?: OR ` + hayChoiceSQL + `)*\)`)
)

// hayPrefilterHolds is the prefilter of grepCondition over hay: every parenthesized group of choices joined by OR
// holds when one of them does. A LIKE is LIKE; hasAllTokens holds when every token is three characters of hay, as
// the ngrams(3) tokenizer of the index cuts it.
func hayPrefilterHolds(t *testing.T, sql, hay string, param func(string) string) bool {
	groups := hayGroupRe.FindAllString(sql, -1)
	if n, m := len(hayChoiceRe.FindAllString(sql, -1)), len(hayChoiceRe.FindAllString(strings.Join(groups, " "), -1)); n != m {
		t.Errorf("%d of %d prefilter choices outside a group: %s", n-m, n, sql)
	}
	for _, g := range groups {
		holds := false
		for _, m := range hayChoiceRe.FindAllStringSubmatch(g, -1) {
			if m[1] != "" {
				holds = holds || likeMatch(hay, tsvUnescape(param(m[1])))
				continue
			}
			all := true
			for _, tok := range chArray(param(m[2])) {
				all = all && utf8.RuneCountInString(tok) == 3 && strings.Contains(hay, tok)
			}
			holds = holds || all
		}
		if !holds {
			return false
		}
	}
	return true
}

// likeMatch is LIKE: % any run, _ any one character, a backslash escapes.
func likeMatch(s, like string) bool {
	var re strings.Builder
	re.WriteString("(?s)^")
	for i := 0; i < len(like); i++ {
		switch c := like[i]; c {
		case '%':
			re.WriteString(".*")
		case '_':
			re.WriteString(".")
		case '\\':
			if i+1 < len(like) {
				i++
				re.WriteString(regexp.QuoteMeta(like[i : i+1]))
			}
		default:
			re.WriteString(regexp.QuoteMeta(like[i : i+1]))
		}
	}
	re.WriteString("$")
	return regexp.MustCompile(re.String()).MatchString(s)
}

// re2Match is the server's match: RE2 where a dot matches a newline unless the pattern turns it off.
func re2Match(pattern, s string) bool { return regexp.MustCompile("(?s)" + pattern).MatchString(s) }

// hyperscanMatch is multiMatchAny: a dot matches a newline, and $ matches before a final newline too.
func hyperscanMatch(pattern, s string) bool {
	re := regexp.MustCompile("(?s)" + pattern)
	return re.MatchString(s) || strings.HasSuffix(s, "\n") && re.MatchString(strings.TrimSuffix(s, "\n"))
}

// chArray reads an array parameter: numbers or quoted strings.
func chArray(v string) []string {
	v = strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
	var out []string
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == '\'':
			var b strings.Builder
			for i++; i < len(v) && v[i] != '\''; i++ {
				if v[i] == '\\' {
					i++
				}
				b.WriteByte(v[i])
			}
			out = append(out, b.String())
		case c >= '0' && c <= '9':
			j := i
			for j < len(v) && v[j] >= '0' && v[j] <= '9' {
				j++
			}
			out = append(out, v[i:j])
			i = j - 1
		}
	}
	return out
}

func chParseTime(t *testing.T, v string) time.Time {
	at, err := time.ParseInLocation("2006-01-02 15:04:05.000", v, time.UTC)
	if err != nil {
		t.Fatalf("time parameter %q: %v", v, err)
	}
	return at
}

const dbOtherSession = "5e6f7a8b-0000-4000-8000-000000000002"

func jsonText(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func userRecord(at, text string) string {
	return `{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"` + at + `","message":{"role":"user","content":` + jsonText(text) + `}}`
}

func agentRecord(at, text string) string {
	return `{"parentUuid":"x","type":"assistant","timestamp":"` + at + `","message":{"role":"assistant","content":[{"type":"text","text":` + jsonText(text) + `}]}}`
}

func rawModelRecord(at, model string, visible bool) string {
	content := `[{"type":"thinking","thinking":"private synthetic detail"},{"type":"tool_use","id":"call-1","name":"Bash","input":{"command":"private synthetic detail"}}`
	if visible {
		content += `,{"type":"text","text":"visible synthetic detail"}`
	}
	content += "]"
	return `{"type":"assistant","timestamp":"` + at + `","message":{"model":"` + model + `","role":"assistant","content":` + content + `}}`
}

func rawHiddenCorpus(t *testing.T) (env, []string) {
	t.Helper()
	e := fakeHome(t)
	path := writeReviewClaude(t, e, "-home-x-proj", "6a7b8c9d-0000-4000-8000-000000000003",
		`{"type":"api-request","model":"empty-model"}`,
		rawModelRecord("2026-10-25T10:10:00Z", "hidden-model", false),
		rawModelRecord("2026-10-25T10:10:01Z", "mixed-model", true),
	)
	fixture := filepath.Join(e.home, ".claude", "projects", "-home-x-proj", sessionID+".jsonl")
	return e, []string{fixture, path}
}

func checkRawHiddenSummary(t *testing.T, e env) {
	t.Helper()
	queries := []struct {
		name string
		args []string
		hint bool
	}{
		{"hidden-only", []string{"--all", "--raw", "hidden-model", "--max", "0"}, true},
		{"mixed-line", []string{"--all", "--raw", "mixed-model", "--max", "0"}, false},
		{"without-raw", []string{"--all", "--max", "0"}, false},
		{"all-roles", []string{"--all", "--role", "all", "--raw", "hidden-model", "--max", "0"}, false},
	}
	for _, test := range queries {
		t.Run(test.name, func(t *testing.T) {
			code, out, errOut := runTL(t, e, test.args...)
			if code != 0 && code != exitNone {
				t.Fatalf("exit %d: %s\n%s", code, errOut, out)
			}
			note := "1 line(s) matched --raw only in hidden roles (think, cmd): --role all"
			got := strings.Contains(out, note)
			if got != test.hint {
				t.Fatalf("hidden-role hint = %t, want %t:\n%s", got, test.hint, out)
			}
			if test.name == "hidden-only" && strings.Contains(out, "private synthetic detail") {
				t.Fatalf("hidden event text reached output:\n%s", out)
			}
		})
	}
}

func TestRawHiddenRoleSummaryFiles(t *testing.T) {
	e, _ := rawHiddenCorpus(t)
	e.backend = backendFiles
	checkRawHiddenSummary(t, e)
}

func TestRawHiddenSummaryWithVisibleEventsAndFilters(t *testing.T) {
	e, _ := rawHiddenCorpus(t)
	e.backend = backendFiles
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"events-and-hidden", []string{"--all", "--raw", "hidden-model|mixed-model", "--max", "0"}, "1 line(s) matched --raw only in hidden roles (think, cmd): --role all"},
		{"time-excludes-hidden", []string{"--all", "--raw", "hidden-model", "--since", "2026-10-26", "--max", "0"}, ""},
		{"grep-excludes-hidden", []string{"--all", "--raw", "hidden-model", "--grep", "^$", "--max", "0"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, out, errOut := runTL(t, e, test.args...)
			if code != 0 && code != exitNone {
				t.Fatalf("exit %d: %s\n%s", code, errOut, out)
			}
			if test.want != "" && !strings.Contains(out, test.want) || test.want == "" && strings.Contains(out, "matched --raw only in hidden roles") {
				t.Fatalf("summary = %q, want note %q", out, test.want)
			}
			if test.name == "events-and-hidden" && !strings.Contains(out, "1 event(s)") {
				t.Fatalf("expected a visible event and hidden summary together: %s", out)
			}
		})
	}
}

func TestRawHiddenSummaryMergesAcrossFiles(t *testing.T) {
	e, paths := rawHiddenCorpus(t)
	second := writeReviewClaude(t, e, "-home-x-other", "7a7b8c9d-0000-4000-8000-000000000004",
		`{"type":"attachment","timestamp":"2026-10-25T10:10:02Z","attachment":{"type":"hook_success","stdout":"attach-model"}}`)
	paths = append(paths, second)
	for _, backend := range []string{backendFiles, backendDB} {
		t.Run(backend, func(t *testing.T) {
			e.backend = backend
			var ch *fakeCH
			var srv *httptest.Server
			if backend == backendDB {
				ch = newFakeCH(t, paths, map[string]int{})
				srv = httptest.NewServer(ch)
				defer srv.Close()
				t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
			}
			code, out, errOut := runTL(t, e, "--all", "--raw", "hidden-model|attach-model", "--max", "0")
			if code != 0 && code != exitNone {
				t.Fatalf("exit %d: %s\n%s", code, errOut, out)
			}
			want := "2 line(s) matched --raw only in hidden roles (think, cmd, attach): --role all"
			if !strings.Contains(out, want) {
				t.Fatalf("summary = %q, want %q", out, want)
			}
		})
	}
}

func TestRawHiddenSummaryDeduplicatesMergedRoles(t *testing.T) {
	e, paths := rawHiddenCorpus(t)
	second := writeReviewClaude(t, e, "-home-x-other", "7a7b8c9d-0000-4000-8000-000000000005",
		`{"type":"assistant","timestamp":"2026-10-25T10:10:03Z","message":{"model":"duplicate-model","content":[{"type":"thinking","thinking":"private synthetic detail"},{"type":"tool_use","name":"Bash","input":{"command":"private synthetic detail"}}]}}`)
	paths = append(paths, second)
	for _, backend := range []string{backendFiles, backendDB} {
		t.Run(backend, func(t *testing.T) {
			e.backend = backend
			if backend == backendDB {
				srv := httptest.NewServer(newFakeCH(t, paths, map[string]int{}))
				defer srv.Close()
				t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
			}
			code, out, errOut := runTL(t, e, "--all", "--raw", "hidden-model|duplicate-model", "--max", "0")
			if code != 0 && code != exitNone {
				t.Fatalf("exit %d: %s\n%s", code, errOut, out)
			}
			want := "2 line(s) matched --raw only in hidden roles (think, cmd): --role all"
			if !strings.Contains(out, want) || strings.Contains(out, "think, cmd, think") {
				t.Fatalf("summary = %q, want unique merged roles %q", out, want)
			}
		})
	}
}

// TestRawSummarySQL pins the text of the one-pass summary: the fake server answers it from its own count, so a
// change of a condition in the query is caught here and not by the summary line.
func TestRawSummarySQL(t *testing.T) {
	d := &dbPath{opts: options{raw: `"model": ?"x`, grep: "deploy", since: time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC),
		until: time.Date(2026, 10, 25, 11, 0, 0, 0, time.UTC), roles: map[transcript.Role]bool{transcript.User: true, transcript.Agent: true}}}
	inDB := []stored{{src: source{Path: "/a.jsonl"}, file: &dbFile{id: 11, evLines: 7, kind: "claude"}},
		{src: source{Path: "/b.jsonl"}, file: &dbFile{id: 12, evLines: 9, kind: "sub"}}}
	q, err := d.rawSummaryQuery(inDB)
	if err != nil {
		t.Fatal(err)
	}
	const want = "SELECT countIf(kept AND roles != 0 AND bitAnd(roles, {visible:UInt16}) = 0), " +
		"groupBitOrIf(roles, kept AND roles != 0 AND bitAnd(roles, {visible:UInt16}) = 0) " +
		"FROM (SELECT max(raw_match) AND line <= transform(file_id, {files:Array(UInt64)}, {ev_lines:Array(UInt64)}, toUInt64(0)) AS kept, " +
		"groupBitOr(role_bit) AS roles FROM (" +
		"SELECT file_id, line, true AS raw_match, toUInt16(0) AS role_bit FROM raw WHERE file_id IN {files:Array(UInt64)} AND " +
		"multiSearchAnyCaseInsensitiveUTF8(raw, {rawlit0:Array(String)}) AND multiSearchAnyCaseInsensitiveUTF8(raw, {rawlit1:Array(String)}) AND match(raw, {raw:String}) " +
		"UNION ALL SELECT file_id, line, false, bitShiftLeft(toUInt16(1), indexOf({role_names:Array(String)}, toString(role)) - 1) " +
		"FROM events WHERE file_id IN {files:Array(UInt64)} AND kind IN {kinds:Array(String)} AND ts >= {since:DateTime64(3, 'UTC')} AND " +
		"ts <= {until:DateTime64(3, 'UTC')} AND (hasAllTokens(hay, {hay0_0:Array(String)})) AND match(concat(who, ' ', text), {grep:String})) " +
		"GROUP BY file_id, line)"
	if q.SQL != want {
		t.Errorf("raw summary SQL:\n got %s\nwant %s", q.SQL, want)
	}
	for name, value := range map[string]string{"files": "[11,12]", "ev_lines": "[7,9]", "kinds": "['claude','sub']", "visible": "9",
		"role_names": "['user','ask','peer','agent','think','cmd','out','attach','note']"} {
		if q.Params[name] != value {
			t.Errorf("param %s = %q, want %q", name, q.Params[name], value)
		}
	}
	// the summary selects the raw lines of --raw the same way as the event query
	w, err := (&dbPath{opts: d.opts}).where([]uint64{11, 12}, []string{"claude", "sub"})
	if err != nil {
		t.Fatal(err)
	}
	rawCond, _, err := rawCondition(d.opts.raw)
	if err != nil {
		t.Fatal(err)
	}
	if read := rawFromSQL + rawCond; !strings.Contains(w.sql, read) || !strings.Contains(q.SQL, read) {
		t.Errorf("the event query and the summary read the raw lines under different conditions:\n%s\n%s", w.sql, q.SQL)
	}
}

func TestRawSummaryWithoutFilters(t *testing.T) {
	d := &dbPath{opts: options{raw: "x", roles: map[transcript.Role]bool{transcript.Note: true}}}
	q, err := d.rawSummaryQuery([]stored{{src: source{Path: "/a.jsonl"}, file: &dbFile{id: 1, evLines: 2, kind: "claude"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(q.SQL, "FROM events WHERE file_id IN {files:Array(UInt64)} AND kind IN {kinds:Array(String)}) GROUP BY file_id, line)") ||
		q.Params["visible"] != "256" {
		t.Errorf("summary without --since, --until and --grep: %s, visible %s", q.SQL, q.Params["visible"])
	}
}

func TestParseRawSummary(t *testing.T) {
	visible := map[transcript.Role]bool{transcript.User: true}
	// bits 0 (user, shown), 4 (think) and 5 (cmd)
	got := parseRawSummary([][]string{{"3", "49"}}, visible)
	if got.err != nil || got.hidden != 3 || !slices.Equal(got.roles, []transcript.Role{transcript.Think, transcript.Cmd}) {
		t.Errorf("parseRawSummary = %+v", got)
	}
	for _, rows := range [][][]string{nil, {{"1"}}, {{"1", "x"}}, {{"1", "2", "3"}}, {{"1", "2"}, {"1", "2"}}} {
		if parseRawSummary(rows, visible).err == nil {
			t.Errorf("parseRawSummary(%q) took a malformed answer", rows)
		}
	}
}

func TestRawHiddenRoleSummaryDB(t *testing.T) {
	e, paths := rawHiddenCorpus(t)
	ch := newFakeCH(t, paths, map[string]int{})
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	e.backend = backendDB
	checkRawHiddenSummary(t, e)
	if !slices.Contains(ch.requests(), "raw-hidden") {
		t.Fatal("database did not query the hidden-role aggregate")
	}
}

func TestRawHiddenDBFailureKeepsEventsAndReportsProblem(t *testing.T) {
	e, paths := rawHiddenCorpus(t)
	ch := newFakeCH(t, paths, map[string]int{})
	ch.fail = map[string]bool{"raw-hidden": true}
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	e.backend = backendDB
	code, out, errOut := runTL(t, e, "--all", "--raw", "mixed-model", "--max", "0")
	if code != 0 || !strings.Contains(out, "event(s)") || !strings.Contains(out, "raw summary unavailable") || strings.Contains(errOut, "reading files instead") {
		t.Fatalf("database raw-summary failure changed event result: exit=%d out=%q stderr=%q", code, out, errOut)
	}
}

func TestRawHiddenDBUsageSkipsAggregate(t *testing.T) {
	e, paths := rawHiddenCorpus(t)
	ch := newFakeCH(t, paths, map[string]int{})
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	e.backend = backendDB
	_, _, errOut := runTL(t, e, "--all", "--usage", "--raw", "model", "--max", "0")
	if strings.Contains(errOut, "reading files instead") || slices.Contains(ch.requests(), "raw-hidden") {
		t.Fatalf("usage ran the hidden raw aggregate or fell back: requests=%v stderr=%q", ch.requests(), errOut)
	}
}

// dbCorpus is a synthetic home for both paths: the fixture session, a session with long texts, secrets, usage and
// a subagent, and a codex rollout. All rows are loaded unless a test marks lines as pending.
func dbCorpus(t *testing.T) (env, []string, map[string]int) {
	t.Helper()
	e := fakeHome(t)
	key := "sk" + "-proj-" + strings.Repeat("x9Y8", 6)
	words := strings.Repeat("слово  за\tсловом\n", 300)
	other := writeReviewClaude(t, e, "-home-x-proj", dbOtherSession,
		`{"type":"agent-name","agentName":"второй","sessionId":"x"}`,
		userRecord("2026-10-25T10:10:00.000Z", "простое решение: "+words+" и ключ "+key+" в конце"),
		agentRecord("2026-10-25T10:10:05.000Z", "Готово, ключ "+key+" спрятан"),
		userRecord("2026-10-25T10:10:06.000Z", "password="+strings.Repeat("x9", 1500)+" потом текст"),
		userRecord("2026-10-25T10:10:07.000Z", "тикет PROJ-1234 и костыль"),
		// texts where the server's regular expressions and lower case part from Go's regexp
		userRecord("2026-10-25T10:10:07.100Z", "an error, a terror"),
		userRecord("2026-10-25T10:10:07.200Z", "an Xrror"),
		agentRecord("2026-10-25T10:10:07.300Z", "all ok\n"),
		agentRecord("2026-10-25T10:10:07.400Z", "PASS\nok, then pass ok"),
		agentRecord("2026-10-25T10:10:07.500Z", "PASS ok"),
		userRecord("2026-10-25T10:10:07.600Z", "foo\nwfor"),
		userRecord("2026-10-25T10:10:07.700Z", "fooы wfor_ wforы"),
		userRecord("2026-10-25T10:10:07.800Z", "ſecret here"),
		userRecord("2026-10-25T10:10:07.900Z", "api_\u212Aey and ΣΟΦΟΣ and İd"),
		usageRecord("m1", "claude-opus-5-5", "часть один", 10, 1000, 20000, 1),
		usageRecord("m1", "claude-opus-5-5", "часть два", 10, 1000, 20000, 300),
		usageRecord("m2", "claude-sonnet-4-5", "другой ответ", 5, 0, 100, 40),
		// the price of an hour's cache write and of fast mode is not in the token columns
		`{"type":"assistant","timestamp":"2026-10-25T10:10:08Z","requestId":"req-m3","message":{"id":"m3","model":"claude-opus-5-5",`+
			`"role":"assistant","content":[{"type":"text","text":"быстрый ответ"}],"usage":{"input_tokens":7,"cache_creation_input_tokens":5000,`+
			`"cache_read_input_tokens":0,"output_tokens":90,"speed":"fast","cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":5000}}}}`,
		userRecord("2026-10-25T10:20:00.000Z", "needle first"),
		agentRecord("2026-10-25T10:20:01.000Z", "a1"),
		agentRecord("2026-10-25T10:20:02.000Z", "a2"),
		userRecord("2026-10-25T10:20:03.000Z", "u3"),
		agentRecord("2026-10-25T10:20:04.000Z", "a4 needle"),
		agentRecord("2026-10-25T10:20:05.000Z", "a5"),
		userRecord("2026-10-25T10:20:06.000Z", "u6"),
		agentRecord("2026-10-25T10:20:07.000Z", "a7"),
		userRecord("2026-10-25T10:20:08.000Z", "NEEDLE last ёжик"),
		userRecord("2026-10-25T10:20:09.000Z", "needle alpha"),
		agentRecord("2026-10-25T10:20:10.000Z", "neighbour"),
		userRecord("2026-10-25T10:20:11.000Z", "needle beta"),
		userRecord("2026-10-25T10:20:12.000Z", "хвост: sub ещё не в базе"),
		agentRecord("2026-10-25T10:20:13.000Z", "Готово и хвост"),
	)
	sub := filepath.Join(e.home, ".claude", "projects", "-home-x-proj", dbOtherSession, "subagents", "agent-a1b2c3d4.jsonl")
	writeWorkFile(t, sub, []string{
		userRecord("2026-10-25T10:10:30.000Z", "задача субагенту: sub"),
		agentRecord("2026-10-25T10:10:40.000Z", "Сделал простое решение"),
	})
	if err := os.WriteFile(strings.TrimSuffix(sub, ".jsonl")+".meta.json", []byte(`{"description":"Synthetic subagent"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	codex, err := os.ReadFile("../../transcript/testdata/codex.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(e.home, ".local", "state", "worker", "w1", "codex", "sessions", "2026", "10", "25",
		"rollout-2026-10-25T10-47-55-0199aaaa-0000-7000-8000-000000000001.jsonl")
	writeWorkFile(t, rollout, strings.Split(strings.TrimSuffix(string(codex), "\n"), "\n"))
	main := filepath.Join(e.home, ".claude", "projects", "-home-x-proj", sessionID+".jsonl")
	return e, []string{main, other, sub, rollout}, map[string]int{}
}

var dbForms = [][]string{
	{"--all", "--grep", "needle", "-C", "1", "--max", "0"},
	{"--all", "--grep", "needle", "-C", "3", "--max", "0"},
	{"--all", "--grep", "needle", "-C", "2", "--max", "3"},
	{"--all", "--grep", "needle", "-C", "1", "--role", "user", "--max", "0"},
	{"--all", "--grep", "needle", "-C", "1", "--role", "agent", "--max", "0"},
	{"--all", "--grep", "needle", "-C", "1", "--role", "all", "--max", "0"},
	{"--all", "--grep", "needle", "-C", "2", "--since", "2026-10-25T10:20:02Z", "--until", "2026-10-25T10:20:06Z", "--max", "0"},
	{"--all", "--grep", "sub|прост", "-C", "2", "--role", "all", "--max", "0"},
	{"--all", "--grep", "ёжик|готово", "-C", "1", "--role", "all", "--max", "0"},
	{"--all", "--grep", "exec|ls", "-C", "1", "--codex", "--role", "all", "--max", "0"},
	{"--all", "--grep", "needle", "-C", "1", "--width", "10", "--max", "0"},
	{"--all", "--grep", "sub", "-C", "1", "--no-subagents", "--role", "all", "--max", "0"},
	{"--all", "--grep", "ответ", "-C", "1", "--usage"},
	{dbOtherSession[:8], "--grep", "sub", "-C", "1", "--subagents", "--role", "all", "--max", "0"},
	{"7f7f7f7f", "--grep", "needle", "-C", "1", "--max", "0"},
	{"--all", "--grep", "needle", "--raw", "alpha", "-C", "1", "--max", "0"},
	{"--all", "--grep", "sub"},
	{"--all", "--role", "all", "--max", "0"},
	{"--all", "--role", "all", "--max", "0", "--width", "30"},
	{"--all", "--role", "all", "--max", "0", "--width", "100"},
	{"--all", "--role", "all", "--grep", "прост[а-я]* решени|готов", "--max", "0"},
	{"--all", "--grep", "[A-Z]{2,6}-[0-9]{3,5}"},
	{"--all", "--role", "all", "--grep", "(?-i:Готово)", "--max", "0"},
	{"--all", "--role", "all", "--grep", "100%_"},
	{"--all", "--role", "all", "--grep", "[^e]rror", "--max", "0"},
	{"--all", "--role", "all", "--grep", `foo\S`, "--max", "0"},
	{"--all", "--role", "all", "--grep", `wfor\W`, "--max", "0"},
	{"--all", "--role", "agent", "--grep", "ok$", "--max", "0"},
	{"--all", "--role", "all", "--grep", "(?-i)PASS.ok", "--max", "0"},
	{"--all", "--role", "all", "--grep", "secret|password|api[_-]?key", "--width", "0", "--max", "0"},
	{"--all", "--role", "all", "--grep", "σοφος|İd", "--max", "0"},
	{"--all", "--raw", "tool_use", "--role", "all", "--max", "0"},
	{"--all", "--raw", `"name":"Bash"|exec_command`, "--role", "all", "--codex", "--max", "0"},
	{"--all", "--codex", "--role", "all", "--max", "0"},
	{"--all", "--role", "all", "--max", "0", "--since", "2026-10-25T10:01:00Z", "--until", "2026-10-25T10:10:06Z"},
	{"--all", "--usage"},
	{"--all", "--usage", "--grep", "ответ"},
	{sessionID[:8], "--role", "all"},
	{dbOtherSession[:8], "--role", "all", "--subagents", "--max", "0"},
	{dbOtherSession[:8] + ":2"},
	{dbOtherSession[:8] + ":10"},
	{dbOtherSession[:8] + ":4", "--width", "50"},
	{sessionID[:8] + ":999"},
}

// The database path prints what the file path prints for the same loaded corpus: listings, filters, cut and masked
// texts, --usage, sessions with subagents and single events.
func TestDBPrintsAsFiles(t *testing.T) {
	e, paths, _ := dbCorpus(t)
	seven := writeReviewClaude(t, e, "-home-x-proj", "7f7f7f7f-0000-4000-8000-00000000000a",
		userRecord("2026-10-25T10:20:00.000Z", "needle first"),
		agentRecord("2026-10-25T10:20:01.000Z", "a1"),
		agentRecord("2026-10-25T10:20:02.000Z", "a2"),
		userRecord("2026-10-25T10:20:03.000Z", "u3"),
		agentRecord("2026-10-25T10:20:04.000Z", "a4 needle"),
		agentRecord("2026-10-25T10:20:05.000Z", "a5"),
		userRecord("2026-10-25T10:20:06.000Z", "u6"),
		agentRecord("2026-10-25T10:20:07.000Z", "a7"),
		userRecord("2026-10-25T10:20:08.000Z", "NEEDLE last ёжик"),
	)
	paths = append(paths, seven)
	ch := newFakeCH(t, paths, map[string]int{})
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	defer func() {
		// each kind of query is asked: the whole text of a long event, the unparsable lines, the usage records
		for _, name := range []string{"files", "events", "texts", "bad", "usage", "event"} {
			if !slices.Contains(ch.requests(), name) {
				t.Errorf("no %s query", name)
			}
		}
		if slices.Contains(ch.requests(), "usage-raw") {
			t.Error("usage queried raw for price details")
		}
	}()
	// rows with flat_len come as their start past the cut; rows without it (loaded before the column) as before
	// it: texts up to wholeText whole and cut here, and with 0 every long one as its start and the server's count
	// of its flattened length
	defer func(whole int) { wholeText = whole }(wholeText)
	for _, noFlatLen := range []bool{false, true} {
		ch.noFlatLen = noFlatLen
		for _, whole := range []int{wholeText, 0} {
			wholeText = whole
			for _, args := range dbForms {
				e.backend = backendFiles
				codeF, outF, _ := runTL(t, e, args...)
				e.backend = backendDB
				codeD, outD, errD := runTL(t, e, args...)
				if strings.Contains(errD, "instead") {
					t.Errorf("no flat_len %v, whole %d, %q: the database path fell back: %s", noFlatLen, whole, args, errD)
				}
				if codeF != codeD || outF != outD {
					t.Errorf("no flat_len %v, whole %d, %q: files exit %d, db exit %d\nfiles:\n%s\ndb:\n%s", noFlatLen, whole, args, codeF, codeD, outF, outD)
				}
				if outD == "" {
					t.Errorf("database %q: empty answer", args)
				}
			}
		}
	}
}

// A long text that flattens to the width is shown whole and read whole; one character more and it is cut from its
// start. Trailing spaces make the text longer than the cut.
func TestDBStartOfTextAtTheWidth(t *testing.T) {
	const width = 60
	for _, flat := range []int{width, width + 1} {
		t.Run(strconv.Itoa(flat), func(t *testing.T) {
			e, paths, unloaded := dbCorpus(t)
			marker := fmt.Sprintf("edge-flat-%d", flat)
			text := marker + " " + strings.Repeat("y", flat-len(marker)-1) + strings.Repeat(" ", 2*textCut(width))
			if got := utf8.RuneCountInString(transcript.Short(text, 0)); got != flat || utf8.RuneCountInString(text) <= textCut(width) {
				t.Fatalf("fixture flattens to %d, want %d; %d characters, want more than %d", got, flat, utf8.RuneCountInString(text), textCut(width))
			}
			f, err := os.OpenFile(paths[0], os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := fmt.Fprintln(f, agentRecord("2026-10-25T11:00:00Z", text)) // agent text keeps its trailing spaces
			if err := errors.Join(writeErr, f.Close()); err != nil {
				t.Fatal(err)
			}
			ch := newFakeCH(t, paths, unloaded)
			srv := httptest.NewServer(ch)
			defer srv.Close()
			t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
			args := []string{paths[0], "--grep", marker, "--role", "agent", "--width", strconv.Itoa(width)}
			e.backend = backendFiles
			wantCode, wantOut, _ := runTL(t, e, args...)
			e.backend = backendDB
			code, out, stderr := runTL(t, e, args...)
			if code != wantCode || out != wantOut || strings.Contains(stderr, "reading files instead") {
				t.Fatalf("files=(%d,%q), db=(%d,%q,%q)", wantCode, wantOut, code, out, stderr)
			}
			ch.mu.Lock()
			defer ch.mu.Unlock()
			if !slices.ContainsFunc(ch.eventRunes, func(n int) bool { return n > flat && n <= textCut(width) }) {
				t.Fatalf("the text did not come cut from the events query: %v", ch.eventRunes)
			}
			if whole := slices.Contains(ch.names, "texts"); whole != (flat <= width) {
				t.Fatalf("flattened to %d at width %d: the whole text read again %v, want %v", flat, width, whole, flat <= width)
			}
		})
	}
}

// A text whose start loses the cut to masking, a secret at the end of the first window, comes whole by a second
// query and is cut as the files cut it, though its row has flat_len and it came as its start.
func TestDBMaskedStartReadsWholeText(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	marker := "masked-edge-fixture "
	f, err := os.OpenFile(paths[0], os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	// width 60: the first window is 572 characters, the start 1144; the secret covers both and masks to a label
	text := marker + "password=" + strings.Repeat("x9", 900) + " потом текст " + strings.Repeat("хвост ", 100)
	_, writeErr := fmt.Fprintln(f, userRecord("2026-10-25T11:00:00Z", text))
	if err := errors.Join(writeErr, f.Close()); err != nil {
		t.Fatal(err)
	}
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	args := []string{paths[0], "--grep", marker, "--role", "user", "--width", "60"}
	e.backend = backendFiles
	wantCode, wantOut, _ := runTL(t, e, args...)
	e.backend = backendDB
	code, out, stderr := runTL(t, e, args...)
	if code != wantCode || out != wantOut || !strings.Contains(out, "потом") || strings.Contains(stderr, "reading files instead") {
		t.Fatalf("masked start differs: files=(%d,%q), db=(%d,%q,%q)", wantCode, wantOut, code, out, stderr)
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if !slices.Contains(ch.names, "texts") || slices.Contains(ch.eventRunes, utf8.RuneCountInString(text)) {
		t.Fatalf("the text should come as its start and then whole: queries %v, characters %v", ch.names, ch.eventRunes)
	}
}

func TestDBFreshnessLineAndNoTranscriptReads(t *testing.T) {
	e, paths, _ := dbCorpus(t)
	unloaded := map[string]int{paths[1]: 2}
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	through := time.Now().Add(-time.Minute).Truncate(time.Second)
	state := dbState{Heartbeat: time.Now().Format(time.RFC3339Nano), DBThrough: through, Home: e.home}
	b, _ := json.Marshal(state)
	statePath := filepath.Join(os.Getenv("XDG_STATE_HOME"), "akit", "tlfeed", "state.json")
	if err := os.WriteFile(statePath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	// The files table remains queryable after every transcript path disappears; db search must not stat/open them.
	for _, path := range paths {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	opts, err := parseArgs([]string{"--all", "--grep", "sub"}, e.now)
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code, ok, err := runOutputDB(opts, e, nil, &out, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || code != 0 {
		t.Fatalf("db answer code=%d ok=%v: %s", code, ok, stderr.String())
	}
	want := "данные актуальны на " + through.Local().Format("15:04:05")
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("freshness line %q missing from %q", want, stderr.String())
	}
	if !strings.Contains(out.String(), "sub") {
		t.Fatalf("database answer empty: %q", out.String())
	}
	if strings.Contains(out.String(), "хвост") {
		t.Fatalf("database read the unloaded file tail: %q", out.String())
	}
}

func TestDBFallsBackForFilesMissingFromFilesTable(t *testing.T) {
	e, paths, _ := dbCorpus(t)
	ch := newFakeCH(t, paths, map[string]int{})
	ch.files = ch.files[:1]
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	e.backend = backendFiles
	wantCode, _, _ := runTL(t, e, dbOtherSession[:8], "--role", "all")
	e.backend = backendDB
	code, out, stderr := runTL(t, e, dbOtherSession[:8], "--role", "all")
	if wantCode != 0 || code != 0 || !strings.Contains(out, "== "+dbOtherSession[:8]) || !strings.Contains(stderr, "no transcript with this id in the database") {
		t.Fatalf("missing files-table row fallback failed: files code=%d, db=(%d,%q,%q)", wantCode, code, out, stderr)
	}
}

func TestDBFindRotatedTranscriptByID(t *testing.T) {
	e, paths, _ := dbCorpus(t)
	ch := newFakeCH(t, paths, map[string]int{})
	path := paths[1]
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	e.backend = backendDB
	for _, args := range [][]string{{dbOtherSession[:8]}, {dbOtherSession[:8] + ":2"}} {
		code, out, stderr := runTL(t, e, args...)
		if code != 0 || !strings.Contains(out, "простое решение") || strings.Contains(stderr, "no transcript with this id") {
			t.Errorf("rotated transcript %v: code=%d out=%q stderr=%q", args, code, out, stderr)
		}
	}
}

func TestDBFindPrefersDiskTranscriptToStalePrefix(t *testing.T) {
	e, paths, _ := dbCorpus(t)
	ch := newFakeCH(t, paths, map[string]int{})
	stale := *ch.files[0]
	stale.session = dbOtherSession[:8] + "-stale"
	stale.path = filepath.Join(t.TempDir(), stale.session+".jsonl")
	stale.id += 1
	if stale.id == ch.files[1].id || len(stale.events) == 0 || stale.events[0].ev.Shown() == ch.files[1].events[0].ev.Shown() {
		t.Fatalf("fixture must have distinct file ids and event data: disk=%+v stale=%+v", ch.files[1], &stale)
	}
	ch.files = append(ch.files, &stale)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	e.backend = backendDB
	code, out, stderr := runTL(t, e, dbOtherSession[:8])
	if code != 0 || !strings.Contains(out, "простое решение") || strings.Contains(out, "привет, сделай sub") || strings.Contains(stderr, "ambiguous") {
		t.Fatalf("disk transcript did not win over stale row: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

func TestDBSubagentsUseStoredFilesWhenRotated(t *testing.T) {
	e, paths, _ := dbCorpus(t)
	ch := newFakeCH(t, paths, map[string]int{})
	if err := os.Remove(paths[1]); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(paths[2]); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	e.backend = backendDB
	code, out, stderr := runTL(t, e, dbOtherSession[:8], "--subagents", "--role", "all", "--max", "0")
	if code != 0 || !strings.Contains(out, "задача субагенту: sub") || strings.Contains(stderr, "reading files instead") {
		t.Fatalf("rotated db subagent missing: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

func TestDBFindAmbiguousRotatedPrefix(t *testing.T) {
	e, paths, _ := dbCorpus(t)
	ch := newFakeCH(t, paths, map[string]int{})
	stale := *ch.files[1]
	stale.session = "5e6f7a8b-1111-4000-8000-000000000003"
	stale.path = filepath.Join(t.TempDir(), stale.session+".jsonl")
	ch.files = append(ch.files, &stale)
	if err := os.Remove(paths[1]); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	e.backend = backendDB
	code, _, stderr := runTL(t, e, dbOtherSession[:8])
	if code != exitUsage || !strings.Contains(stderr, "ambiguous") {
		t.Fatalf("two rotated matches: code=%d stderr=%q", code, stderr)
	}
}

func TestDBFindMissingIDKeepsExistingError(t *testing.T) {
	e, paths, _ := dbCorpus(t)
	ch := newFakeCH(t, paths, map[string]int{})
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	e.backend = backendDB
	code, _, stderr := runTL(t, e, "ffffff00")
	if code == 0 || !strings.Contains(stderr, "no transcript with this id in the database") {
		t.Fatalf("missing ID error changed: code=%d stderr=%q", code, stderr)
	}
}

func TestDBDoesNotReadUnloadedTail(t *testing.T) {
	e, paths, _ := dbCorpus(t)
	unloaded := map[string]int{paths[1]: 2}
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	e.backend = backendDB
	code, out, stderr := runTL(t, e, "--all", "--role", "all", "--grep", "хвост", "--max", "0")
	if code != exitNone || strings.Contains(out, " user хвост:") || strings.Contains(stderr, "reading files instead") {
		t.Fatalf("db included un-ingested tail: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

func TestDBUntilUsesEarliestStoredEvent(t *testing.T) {
	cutoff := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	d := &dbPath{opts: options{all: true, until: cutoff}, e: env{now: cutoff}}
	w := d.allFilePredicate()
	if !strings.Contains(w.sql, "first_ts <= {file_until:DateTime64(3, 'UTC')}") || w.params["file_until"] == "" {
		t.Fatalf("file predicate does not bound first event by --until: SQL=%q params=%v", w.sql, w.params)
	}
	before := &dbFile{mtime: cutoff, first: cutoff.Add(-time.Second)}
	after := &dbFile{mtime: cutoff, first: cutoff.Add(time.Second)}
	if !d.selectedAll(before, cutoff.Add(-time.Hour)) || d.selectedAll(after, cutoff.Add(-time.Hour)) {
		t.Fatal("--until did not select sessions by their first event time")
	}
}

func TestDBWideListingUsesAWholeLimitAtLeastAsWideAsItsCut(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	marker := "long-width-fixture "
	f, err := os.OpenFile(paths[0], os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := fmt.Fprintln(f, userRecord("2026-10-25T11:00:00Z", marker+strings.Repeat("x", 68_000)))
	if err := errors.Join(writeErr, f.Close()); err != nil {
		t.Fatal(err)
	}
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	args := []string{paths[0], "--grep", marker, "--role", "user", "--width", "33000"}
	e.backend = backendFiles
	wantCode, wantOut, _ := runTL(t, e, args...)
	e.backend = backendDB
	code, out, stderr := runTL(t, e, args...)
	wantWhole := strconv.Itoa(max(wholeText, textCut(33000)))
	if code != wantCode || out != wantOut || strings.Contains(stderr, "reading files instead") {
		t.Fatalf("wide listing differs: files=(%d,%q), db=(%d,%q,%q)", wantCode, wantOut, code, out, stderr)
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if !slices.Contains(ch.eventWhole, wantWhole) {
		t.Fatalf("events whole limits %v, want %s", ch.eventWhole, wantWhole)
	}
}

// A row loaded before flat_len, with 0 there, is read as before the column: a text up to wholeText comes whole
// and is cut here, though it is longer than the cut.
func TestDBShortCutReadsWholeText(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	marker := "whole-text-fixture "
	f, err := os.OpenFile(paths[0], os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	text := marker + strings.Repeat("x", 5000) + " unique-tail-marker"
	_, writeErr := fmt.Fprintln(f, userRecord("2026-10-25T11:00:00Z", text))
	if err := errors.Join(writeErr, f.Close()); err != nil {
		t.Fatal(err)
	}
	ch := newFakeCH(t, paths, unloaded)
	ch.noFlatLen = true
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	args := []string{paths[0], "--grep", marker, "--role", "user", "--width", "80"}
	e.backend = backendFiles
	wantCode, wantOut, _ := runTL(t, e, args...)
	e.backend = backendDB
	code, out, stderr := runTL(t, e, args...)
	if code != wantCode || out != wantOut || strings.Contains(stderr, "reading files instead") {
		t.Fatalf("short-cut listing differs: files=(%d,%q), db=(%d,%q,%q)", wantCode, wantOut, code, out, stderr)
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if !slices.Contains(ch.eventRunes, utf8.RuneCountInString(text)) {
		t.Fatalf("a text below the whole-text limit of a row without flat_len did not come whole: %v characters", ch.eventRunes)
	}
	if slices.Contains(ch.names, "texts") {
		t.Fatalf("a text that came whole was read again: %v", ch.names)
	}
}

// A row with flat_len comes as the start of its text when the text is longer than the cut, below wholeText too:
// the listing cuts it by the stored length and prints what the files give, without reading the text again.
func TestDBCutsByStoredFlatLen(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	marker := "stored-length-fixture "
	f, err := os.OpenFile(paths[0], os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	// runs of spaces and line breaks: the flattened length differs from the length of the text
	text := marker + strings.Repeat("слово  за\r\nсловом 　", 400) + " unique-tail-marker"
	_, writeErr := fmt.Fprintln(f, userRecord("2026-10-25T11:00:00Z", text))
	if err := errors.Join(writeErr, f.Close()); err != nil {
		t.Fatal(err)
	}
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	for _, width := range []string{"60", "80", "200"} {
		args := []string{paths[0], "--grep", marker, "--role", "user", "--width", width}
		e.backend = backendFiles
		wantCode, wantOut, _ := runTL(t, e, args...)
		e.backend = backendDB
		code, out, stderr := runTL(t, e, args...)
		if code != wantCode || out != wantOut || !strings.Contains(out, "…(+") || strings.Contains(stderr, "reading files instead") {
			t.Fatalf("width %s: listing by the stored length differs: files=(%d,%q), db=(%d,%q,%q)", width, wantCode, wantOut, code, out, stderr)
		}
		w, _ := strconv.Atoi(width)
		ch.mu.Lock()
		runes, names := slices.Clone(ch.eventRunes), slices.Clone(ch.names)
		ch.eventRunes, ch.names = nil, nil
		ch.mu.Unlock()
		if len(runes) == 0 || slices.Max(runes) > textCut(w) {
			t.Fatalf("width %s: the listing carried %v characters of the texts, want at most the cut %d", width, runes, textCut(w))
		}
		if slices.Contains(names, "texts") {
			t.Fatalf("width %s: a text cut by its stored length was read again: %v", width, names)
		}
	}
}

// A database that does not answer leaves the call to the files, with a warning.
func TestDBUnavailableReadsFiles(t *testing.T) {
	e, _, _ := dbCorpus(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	args := []string{"--all", "--grep", "sub"}
	e.backend = backendFiles
	codeF, outF, _ := runTL(t, e, args...)
	e.backend = backendDB
	codeD, outD, errD := runTL(t, e, args...)
	if codeF != codeD || outF != outD {
		t.Errorf("files exit %d, unavailable db exit %d\n%s\n%s", codeF, codeD, outF, outD)
	}
	if !strings.Contains(errD, "database unavailable") || !strings.Contains(errD, "reading files instead") {
		t.Errorf("no warning: %q", errD)
	}
}

// A server that takes the connection and does not answer sends the call to the files after the bound of the
// first query, not after the bound of the whole call.
func TestDBHungServerReadsFiles(t *testing.T) {
	e, _, _ := dbCorpus(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	old := filesTimeout
	filesTimeout = 200 * time.Millisecond
	defer func() { filesTimeout = old }()
	e.backend = backendDB
	start := time.Now()
	code, out, errOut := runTL(t, e, sessionID[:8])
	if code != 0 || out == "" || !strings.Contains(errOut, "reading files instead") {
		t.Errorf("exit %d, out %q, err %q", code, out, errOut)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("a hung server held the call %s", took)
	}
}

// A server error is a fallback too, not an answer.
func TestDBServerErrorReadsFiles(t *testing.T) {
	e, _, _ := dbCorpus(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Code: 60. DB::Exception: Unknown table expression identifier 'files'", http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	e.backend = backendDB
	code, out, errOut := runTL(t, e, sessionID[:8])
	if code != 0 || out == "" || !strings.Contains(errOut, "code") || !strings.Contains(errOut, "reading files instead") {
		t.Errorf("exit %d, out %q, err %q", code, out, errOut)
	}
}

func TestDBUsageIncompleteSessionFallsBackToFiles(t *testing.T) {
	e, paths, _ := dbCorpus(t)
	rollout := filepath.Join(e.home, ".codex", "sessions", "2026", "10", "25", "rollout-2026-10-25T10-20-00-0199bbbb-0000-7000-8000-000000000001.jsonl")
	writeWorkFile(t, rollout, extRolloutRows("/work/extproj", "внешний запуск"))
	ch := newFakeCH(t, append(paths, rollout), map[string]int{})
	ch.legacyUsage = true
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	state := dbState{Heartbeat: time.Now().Format(time.RFC3339Nano), DBThrough: time.Now(), Home: e.home}
	b, _ := json.Marshal(state)
	statePath := filepath.Join(os.Getenv("XDG_STATE_HOME"), "akit", "tlfeed", "state.json")
	if err := os.WriteFile(statePath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	// a Codex token report loaded before the usage columns existed has neither an id nor a model
	for _, args := range [][]string{{"--all", "--usage"}, {rollout, "--usage"}} {
		e.backend = backendFiles
		codeFiles, outFiles, _ := runTL(t, e, args...)
		e.backend = backendDB
		codeDB, outDB, warning := runTL(t, e, args...)
		if codeFiles != 0 || codeDB != codeFiles || outDB != outFiles || !strings.Contains(warning, "usage session metadata is incomplete") || !strings.Contains(warning, "reading files instead") {
			t.Errorf("%q: incomplete usage did not fall back: files=(%d,%q), db=(%d,%q,%q)", args, codeFiles, outFiles, codeDB, outDB, warning)
		}
	}
}

// usageSameAsFiles runs tl --usage through both paths, requires the same answer and returns it.
func usageSameAsFiles(t *testing.T, e env, args ...string) (int, string) {
	t.Helper()
	args = append([]string{"--usage"}, args...)
	e.backend = backendFiles
	codeF, outF, _ := runTL(t, e, args...)
	e.backend = backendDB
	codeD, outD, errD := runTL(t, e, args...)
	if codeF != codeD || outF != outD || strings.Contains(errD, "instead") {
		t.Errorf("%q: files (%d):\n%s\ndb (%d, %s):\n%s", args, codeF, outF, codeD, errD, outD)
	}
	return codeD, outD
}

// A window counts a reply by its events inside the window alone: one is enough, and the tokens are of the event with
// the most output among them, the first of equal ones.
func TestDBUsageWindowSelectsReplyVersionsByEventTime(t *testing.T) {
	e := fakeHome(t)
	project := filepath.Join(e.home, ".claude", "projects", "-home-x-proj")
	at := func(record, clock string) string { return strings.ReplaceAll(record, "10:00:00Z", clock) }
	version := func(clock string, read, out int64) string {
		return at(usageRecord("windowed", "claude-opus-5-5", clock, 10, 0, read, out), clock)
	}
	var paths []string
	session := func(n int, rows ...string) string {
		path := filepath.Join(project, fmt.Sprintf("abab123%d-0000-4000-8000-000000000001.jsonl", n))
		writeWorkFile(t, path, rows)
		paths = append(paths, path)
		return path
	}
	grows := session(1, version("10:00:00.000Z", 1, 2), version("10:01:00.000Z", 2, 20))
	equal := session(2, version("10:00:00.000Z", 100, 20), version("10:01:00.000Z", 200, 20), version("10:02:00.000Z", 300, 20))
	shrinks := session(3, version("10:00:00.000Z", 1, 20), version("10:01:00.000Z", 2, 2))
	repeats := session(4, version("10:00:00.000Z", 1, 20), version("10:01:00.000Z", 1, 20), version("10:02:00.000Z", 1, 20))
	returns := session(6, version("10:00:00.000Z", 100, 20), version("10:01:00.000Z", 200, 20), version("10:02:00.000Z", 100, 20))
	outOfOrder := session(5, version("10:01:00.000Z", 200, 20), version("10:00:00.000Z", 100, 20))
	ch := newFakeCH(t, paths, map[string]int{})
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)

	const day = "2026-10-25T"
	for _, tc := range []struct {
		name, path, since, until string
		read, out                string // "" — no reply in the window
	}{
		{"whole", grows, "", "", "2", "20"},
		{"until between", grows, "", "10:00:30Z", "1", "2"},
		{"since between", grows, "10:00:30Z", "", "2", "20"},
		{"until at the first event", grows, "", "10:00:00.000Z", "1", "2"},
		{"until before the first event", grows, "", "09:59:59.999Z", "", ""},
		{"since at the last event", grows, "10:01:00.000Z", "", "2", "20"},
		{"since after the last event", grows, "10:01:00.001Z", "", "", ""},
		{"between the events", grows, "10:00:00.001Z", "10:00:59.999Z", "", ""},
		{"equal, whole", equal, "", "", "100", "20"},
		{"equal, until", equal, "", "10:00:30Z", "100", "20"},
		{"equal, since", equal, "10:00:30Z", "", "200", "20"},
		{"equal, both", equal, "10:00:30Z", "10:01:30Z", "200", "20"},
		{"equal, last alone", equal, "10:01:30Z", "", "300", "20"},
		{"shrinks, whole", shrinks, "", "", "1", "20"},
		{"shrinks, since", shrinks, "10:00:30Z", "", "2", "2"},
		{"repeats, until", repeats, "", "10:00:30Z", "1", "20"},
		{"repeats, middle", repeats, "10:00:30Z", "10:01:30Z", "1", "20"},
		{"returns, whole", returns, "", "", "100", "20"},
		{"returns, since", returns, "10:00:30Z", "", "200", "20"},
		{"out of order, whole", outOfOrder, "", "", "100", "20"},
	} {
		args := []string{tc.path}
		if tc.since != "" {
			args = append(args, "--since", day+tc.since)
		}
		if tc.until != "" {
			args = append(args, "--until", day+tc.until)
		}
		code, out := usageSameAsFiles(t, e, args...)
		if tc.out == "" {
			if code != exitNone {
				t.Errorf("%s: exit %d, want %d:\n%s", tc.name, code, exitNone, out)
			}
			continue
		}
		fields := strings.Split(strings.Split(out, "\n")[1], "\t")
		if len(fields) < 6 || fields[1] != "1" || fields[4] != tc.read || fields[5] != tc.out {
			t.Errorf("%s: want 1 reply, cache read %s, output %s:\n%s", tc.name, tc.read, tc.out, out)
		}
	}
}

// A reply belongs to the session that has its first event in the window, whichever session has the most output;
// replies without ids are counted at every event, a repeated token report of Codex is not a call.
func TestDBUsageCountsRepliesAsFiles(t *testing.T) {
	e := fakeHome(t)
	project := filepath.Join(e.home, ".claude", "projects", "-home-x-proj")
	at := func(record, clock string) string { return strings.ReplaceAll(record, "10:00:00Z", clock) }
	noIDs := func(record string) string {
		return strings.NewReplacer(`"requestId":"req-",`, "", `"id":"",`, "").Replace(record)
	}
	const one, two = "abab1111-0000-4000-8000-000000000001", "abab2222-0000-4000-8000-000000000001"
	paths := []string{
		filepath.Join(project, one+".jsonl"),
		filepath.Join(project, two+".jsonl"),
		// the copies of one subagent under two sessions: the later one has the most output
		filepath.Join(project, one, "subagents", "agent-c0c0c0c0.jsonl"),
		filepath.Join(project, two, "subagents", "agent-c0c0c0c0.jsonl"),
		filepath.Join(e.home, ".local", "state", "worker", "w1", "codex", "sessions", "2026", "10", "25", "rollout-2026-10-25T10-20-00-0199cccc-0000-7000-8000-000000000001.jsonl"),
		filepath.Join(project, sessionID+".jsonl"),
	}
	writeWorkFile(t, paths[0], []string{userRecord("2026-10-25T09:59:00Z", "one"), usageRecord("shared", "claude-opus-5-5", "parent", 10, 0, 0, 3)})
	writeWorkFile(t, paths[1], []string{
		userRecord("2026-10-25T09:59:00Z", "two"),
		noIDs(usageRecord("", "claude-opus-5-5", "no ids", 10, 0, 0, 7)),
		noIDs(usageRecord("", "claude-opus-5-5", "no ids again", 10, 0, 0, 7)),
		strings.NewReplacer(`"requestId":"req-"`, `"requestId":"req-only"`, `"id":"",`, "", `"model":"",`, "").Replace(usageRecord("", "", "request only", 10, 0, 0, 5)),
		strings.NewReplacer(`"requestId":"req-message",`, "", `"model":"",`, "").Replace(usageRecord("message", "", "message id only", 10, 0, 0, 6)),
	})
	writeWorkFile(t, paths[2], []string{usageRecord("shared", "claude-opus-5-5", "copy", 10, 0, 0, 1), at(usageRecord("shared", "claude-opus-5-5", "copy", 10, 0, 0, 4), "10:02:00Z")})
	writeWorkFile(t, paths[3], []string{at(usageRecord("shared", "claude-opus-5-5", "copy", 10, 0, 0, 9), "10:01:00Z")})
	rollout := extRolloutRows("/work/proj", "запуск")
	writeWorkFile(t, paths[4], append(rollout, rollout[len(rollout)-1]))
	ch := newFakeCH(t, paths, map[string]int{})
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	for _, tc := range []struct {
		replies string
		args    []string
	}{
		{"7", []string{"--all", "--codex"}},
		{"6", []string{"--all"}},
		{"1", []string{"--all", "--since", "2026-10-25T10:00:30Z"}},
		{"1", []string{"--all", "--since", "2026-10-25T10:01:30Z"}},
		{"2", []string{one, "--subagents"}},
		{"1", []string{paths[4]}},
	} {
		_, out := usageSameAsFiles(t, e, tc.args...)
		rows := strings.Split(out, "\n")
		if i := slices.IndexFunc(rows, func(r string) bool { return strings.HasPrefix(r, "total\t") }); i < 0 || strings.Split(rows[i], "\t")[1] != tc.replies {
			t.Errorf("%q: want %s replies:\n%s", tc.args, tc.replies, out)
		}
	}
	for _, sql := range ch.usageSQL {
		if n := strings.Count(sql, "model, reply_id, request_id, in_tok, out_tok, cache_read, cache_write, cache_write_5m, cache_write_1h, fast, geo"); n != 2 {
			t.Errorf("the usage columns are %d times in the query, want in SELECT and GROUP BY: %s", n, sql)
		}
	}
}

// Every form of a session and of a reference means the same through both paths.
func TestDBSessionReferencePrefixes(t *testing.T) {
	e, paths, _ := dbCorpus(t)
	ch := newFakeCH(t, paths, map[string]int{})
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	id, tilded := dbOtherSession[:8], "~/"+strings.TrimPrefix(paths[1], e.home+string(filepath.Separator))
	for _, tc := range []struct {
		arg  string
		code int
	}{
		{id, 0},
		{id + ":2", 0},
		{"id:" + id, 0},
		{"id:" + id + ":2", 0},
		{paths[1], 0},
		{paths[1] + ":2", 0},
		{"id:" + paths[1] + ":2", 0},
		{tilded, 0},
		{tilded + ":2", 0},
		{"id:" + tilded, 0},
		// a second line or a second prefix is not a part of the form
		{id + ":5:2", exitUsage},
		{"id:" + id + ":5:2", exitUsage},
		{"id:id:" + id + ":2", exitUsage},
		{"id:id:" + id, exitUsage},
		{"id:" + id + ":", exitUsage},
		{"id::2", exitUsage},
		{"id:", exitUsage},
	} {
		e.backend = backendFiles
		codeF, outF, errF := runTL(t, e, tc.arg)
		e.backend = backendDB
		codeD, outD, errD := runTL(t, e, tc.arg)
		if codeF != tc.code || codeD != tc.code || outF != outD || tc.code == 0 && strings.Contains(errD, "instead") {
			t.Errorf("%q: want exit %d: files=(%d,%q), db=(%d,%q,%q)", tc.arg, tc.code, codeF, outF, codeD, outD, errD)
		}
		// a reference prints its id without the prefix; an empty id is named so on both paths
		if strings.Contains(outF, " id:") || tc.arg == "id::2" && (!strings.Contains(errF, "empty id") || !strings.Contains(errD, "empty id")) {
			t.Errorf("%q: files=(%q,%q), db=(%q,%q)", tc.arg, outF, errF, outD, errD)
		}
	}
}

func TestDBDataQueryErrorsReadFiles(t *testing.T) {
	for _, query := range []string{"events", "link_names"} {
		t.Run(query, func(t *testing.T) {
			e, paths, unloaded := dbCorpus(t)
			ch := newFakeCH(t, paths, unloaded)
			ch.fail = map[string]bool{query: true}
			srv := httptest.NewServer(ch)
			defer srv.Close()
			t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
			args := []string{"--all", "--role", "all", "--grep", "sub", "--max", "0"}
			e.backend = backendFiles
			codeF, outF, _ := runTL(t, e, args...)
			e.backend = backendDB
			codeD, outD, errD := runTL(t, e, args...)
			if codeD != codeF || outD != outF || !strings.Contains(errD, "synthetic query failure") || !strings.Contains(errD, "reading files instead") {
				t.Fatalf("query error did not fall back: files=(%d,%q), db=(%d,%q,%q)", codeF, outF, codeD, outD, errD)
			}
		})
	}
}

func TestChArrayParam(t *testing.T) {
	in := []string{`a'b`, `c\d`, "", "е"}
	if got := chArray(chStrings(in)); !slices.Equal(got, in) {
		t.Errorf("%q", got)
	}
}

// liveDB runs against a real server only when TL_LIVE_DB is set: the SQL the fake server does not run.
func liveDB(t *testing.T) *chClient {
	t.Helper()
	if os.Getenv("TL_LIVE_DB") == "" {
		t.Skip("TL_LIVE_DB is not set")
	}
	c, err := newCHClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The length the server counts for a long text is the length of transcript.Short.
func TestLiveFlatLen(t *testing.T) {
	c := liveDB(t)
	for _, text := range []string{" a  b\n\nc ", " x　y ", "слово\t\tслово\n", "", "\n\n", "a​b"} {
		rows, err := c.SelectTSV(t.Context(), chQuery{Name: "test-flatlen", SQL: "SELECT " + flatLenSQL + " FROM (SELECT {t:String} AS text)",
			Params: map[string]string{"t": chString(text), "ws": chString(spaceClass + "+"), "trim": chString("^" + spaceClass + "+|" + spaceClass + "+$")}})
		if err != nil {
			t.Fatal(err)
		}
		if want := strconv.Itoa(len([]rune(transcript.Short(text, 0)))); len(rows) != 1 || rows[0][0] != want {
			t.Errorf("%q: server %v, Short %s", text, rows, want)
		}
	}
}

// A real server answers the projection of a listing as the fake does: by flat_len where it is stored, the old way
// where it is 0; the fake knows the projection the listing sends.
func TestLiveEventText(t *testing.T) {
	if eventTextSQL != wantEventTextSQL {
		t.Fatalf("the listing sends a projection the fake does not know:\n%s\nwant\n%s", eventTextSQL, wantEventTextSQL)
	}
	c := liveDB(t)
	const cut, whole = 20, 40
	texts := []string{"", "  ", "short", strings.Repeat("ab ", 7), strings.Repeat("ab ", 10), strings.Repeat("слово\r\n ", 6),
		strings.Repeat("x 　y ", 12), strings.Repeat(" \n", 30)}
	for _, text := range texts {
		for _, stored := range []int{0, len([]rune(transcript.Short(text, 0))), 7} {
			rows, err := c.SelectTSV(t.Context(), chQuery{Name: "test-event-text", SQL: "SELECT " + eventTextSQL + " FROM (SELECT {t:String} AS text, {f:UInt32} AS flat_len)",
				Params: map[string]string{"t": chString(text), "f": strconv.Itoa(stored), "cut": strconv.Itoa(cut), "whole": strconv.Itoa(whole),
					"ws": chString(spaceClass + "+"), "trim": chString("^" + spaceClass + "+|" + spaceClass + "+$")}})
			if err != nil {
				t.Fatal(err)
			}
			wantText, wantFlat := eventText(text, stored, cut, whole)
			if len(rows) != 1 || len(rows[0]) != 2 || rows[0][0] != wantText || rows[0][1] != strconv.Itoa(wantFlat) {
				t.Errorf("%q, flat_len %d: server %q, fake %q %d", text, stored, rows, wantText, wantFlat)
			}
		}
	}
}

// The condition of --grep over a real server holds where Go's (?i) regexp matches: the semantics of RE2 there
// (a dot and a newline, $ and a final newline, negated classes, \S and \W) and of lowerUTF8 against Go's case
// folding (ſ, the Kelvin sign, σ and ς, İ).
func TestLiveGrepCondition(t *testing.T) {
	c := liveDB(t)
	for _, c2 := range grepCases {
		cond, params, err := grepCondition(c2.pattern)
		if err != nil {
			t.Fatal(err)
		}
		params["t"] = chString(c2.text)
		// hay here has no index: its tokenizer, which hasAllTokens takes from the index on events.hay, is named
		cond = liveTrigrams.ReplaceAllString(cond, "hasAllTokens(hay, $1, 'ngrams(3)')")
		rows, err := c.SelectTSV(t.Context(), chQuery{Name: "test-grep",
			SQL: "SELECT " + cond + " FROM (SELECT '' AS who, {t:String} AS text, lowerUTF8(concat(who, ' ', text)) AS hay)", Params: params})
		if err != nil {
			t.Fatal(err)
		}
		want := regexp.MustCompile("(?i)" + c2.pattern).MatchString(" " + c2.text)
		if len(rows) != 1 || (rows[0][0] == "1") != want {
			t.Errorf("%q over %q: server %v, Go %v", c2.pattern, c2.text, rows, want)
		}
	}
}

var liveTrigrams = regexp.MustCompile(`hasAllTokens\(hay, (\{\w+:Array\(String\)\})\)`)

// The trigrams of the --grep prefilter are the tokens of the index on hay the loader creates: hasAllTokens takes
// the tokenizer of a column from its index, and without it would cut hay into words and miss a literal inside one.
func TestGrepTrigramsMatchTheIndexOnHay(t *testing.T) {
	ddl, err := os.ReadFile("../tlfeed/ddl.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ddl), "INDEX hay_ng hay TYPE text(tokenizer = ngrams(3))") {
		t.Error("cmd/tlfeed/ddl.go has no ngrams(3) text index on events.hay; grepCondition's trigrams need it")
	}
	cond, params, err := grepCondition("Wfor|мЕра")
	if err != nil {
		t.Fatal(err)
	}
	if want := "(hasAllTokens(hay, {hay0_0:Array(String)}) OR hasAllTokens(hay, {hay0_1:Array(String)}))"; !strings.HasPrefix(cond, want) {
		t.Errorf("condition %q, want it to start with %q", cond, want)
	}
	if got := chArray(params["hay0_0"]); !slices.Equal(got, []string{"wfo", "for"}) {
		t.Errorf("trigrams of Wfor %q", got)
	}
	if got := chArray(params["hay0_1"]); !slices.Equal(got, []string{"мер", "ера"}) {
		t.Errorf("trigrams of мЕра %q", got)
	}
	// a choice too short for a trigram keeps its group on LIKE
	if cond, _, _ := grepCondition("wfor|ab"); !strings.Contains(cond, "hay LIKE {hay0_0:String} OR hay LIKE {hay0_1:String}") {
		t.Errorf("short choice: %q", cond)
	}
}

// grepCases are patterns over texts where RE2, hyperscan, lowerUTF8 and Go's regexp may part.
var grepCases = []struct{ pattern, text string }{
	{"прост[а-я]* решени|КОСТЫЛ", "Простое решение"}, {`[A-Z]{2,6}-[0-9]{3,5}`, "тикет ab-1234"}, {"Мера", "МЕРА"},
	{"[^e]rror", "an Error"}, {"[^e]rror", "an Xrror"}, {"[^e]rror", "terror"},
	{`foo\S`, "foo\n"}, {`foo\S`, "foo\v"}, {`foo\S`, "fooы"}, {`wfor\W`, "wforы"}, {`wfor\W`, "wfor_"}, {`wfor\W`, "wfor"},
	{"ok$", "all ok\n"}, {"ok$", "all ok"}, {"(?-i)PASS.ok", "PASS\nok"}, {"(?-i)PASS.ok", "PASS ok"}, {"(?-i)PASS.ok", "pass ok"},
	{"secret", "ſecret"}, {"secret", "SECRET"}, {"api[_-]?key", "api_\u212Aey"}, {"σοφος", "ΣΟΦΟΣ"}, {"σοφοσ", "ΣΟΦΟΣ"},
	{"İd", "İd"}, {"id", "İd"}, {"İd", "id"}, {"ı", "I"}, {"100%_", "100%_ x"}, {"100%_", "100x_"},
}

// The condition the fake server evaluates gives what Go's regexp gives on the same cases.
func TestFakeGrepCondition(t *testing.T) {
	ch := &fakeCH{t: t}
	f := &chFile{id: 1, kind: "claude"}
	for _, c2 := range grepCases {
		cond, params, err := grepCondition(c2.pattern)
		if err != nil {
			t.Fatal(err)
		}
		q := url.Values{"param_files": {"[1]"}, "param_kinds": {"['claude']"}, "param_roles": {"['user']"}}
		for k, v := range params {
			q.Set("param_"+k, v)
		}
		e := chEvent{ev: transcript.Event{Role: transcript.User, Text: c2.text}}
		want := regexp.MustCompile("(?i)" + c2.pattern).MatchString(" " + c2.text)
		if got := ch.match(q, cond, f, e); got != want {
			t.Errorf("%q over %q: fake server %v, Go %v (%s)", c2.pattern, c2.text, got, want, cond)
		}
	}
}

func TestFakeGrepFoldedAlternateKeepsUnfoldableBranch(t *testing.T) {
	const pattern = "ſ|s"
	cond, params, err := grepCondition(pattern)
	if err != nil {
		t.Fatal(err)
	}
	params["t"] = chString("ſ")
	q := url.Values{"param_files": {"[1]"}, "param_kinds": {"['claude']"}, "param_roles": {"['user']"}}
	for k, v := range params {
		q.Set("param_"+k, v)
	}
	ch := &fakeCH{t: t}
	got := ch.match(q, cond, &chFile{id: 1, kind: "claude"}, chEvent{ev: transcript.Event{Role: transcript.User, Text: "ſ"}})
	if !got || !regexp.MustCompile("(?i)"+pattern).MatchString("ſ") {
		t.Fatalf("folded alternative rejected its long-s branch: condition %q, got %v", cond, got)
	}
}

// rawSummaryLine runs tl over paths on the files or the database path and returns its last output line.
func rawSummaryLine(t *testing.T, e env, paths []string, backend string, args ...string) string {
	t.Helper()
	e.backend = backend
	if backend == backendDB {
		srv := httptest.NewServer(newFakeCH(t, paths, map[string]int{}))
		defer srv.Close()
		t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	}
	code, out, errOut := runTL(t, e, args...)
	if code != 0 && code != exitNone {
		t.Fatalf("exit %d: %s\n%s", code, errOut, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	return lines[len(lines)-1]
}

// TestRawSummaryKeepsToTheWindow: the summary of a window does not change when matched lines are added outside it.
func TestRawSummaryKeepsToTheWindow(t *testing.T) {
	e := fakeHome(t)
	path := writeReviewClaude(t, e, "-home-x-window", "6a7b8c9d-0000-4000-8000-000000000007",
		rawModelRecord("2020-01-01T10:00:00Z", "window-model", false),
		`{"type":"api-request","timestamp":"2026-10-25T10:00:00Z","model":"window-model"}`,
		rawModelRecord("2026-10-25T10:00:01Z", "window-model", false),
	)
	const want = "agent); 1 line(s) matched --raw only in hidden roles (think, cmd): --role all"
	for _, backend := range []string{backendFiles, backendDB} {
		t.Run(backend, func(t *testing.T) {
			got := rawSummaryLine(t, e, []string{path}, backend, "--all", "--raw", "window-model",
				"--since", "2020-01-01T00:00:00Z", "--until", "2020-01-02T00:00:00Z", "--max", "0")
			if !strings.HasSuffix(got, want) {
				t.Errorf("summary of a 2020 window:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// TestRawSummaryMatchesLinesWithoutNewline: the summary applies --raw to a line without its newline, as the event
// listing and the database loader do.
func TestRawSummaryMatchesLinesWithoutNewline(t *testing.T) {
	e := fakeHome(t)
	path := writeReviewClaude(t, e, "-home-x-newline", "6a7b8c9d-0000-4000-8000-000000000008",
		`{"type":"api-request","model":"empty-model"}`,
		rawModelRecord("2026-10-25T10:10:00Z", "hidden-model", false),
	)
	for _, backend := range []string{backendFiles, backendDB} {
		for _, test := range []struct{ raw, want string }{
			{`empty-model"}\n`, "agent)"},
			{`hidden-model.*}$`, "agent); 1 line(s) matched --raw only in hidden roles (think, cmd): --role all"},
		} {
			t.Run(backend+"/"+test.raw, func(t *testing.T) {
				if got := rawSummaryLine(t, e, []string{path}, backend, "--all", "--raw", test.raw, "--max", "0"); !strings.HasSuffix(got, test.want) {
					t.Errorf("summary of --raw %s:\n got %s\nwant %s", test.raw, got, test.want)
				}
			})
		}
	}
}
