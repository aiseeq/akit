package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
)

// fakeCH answers the queries of the database path the way ClickHouse would over tables the loader filled from
// the transcripts of a synthetic home: it evaluates the conditions the client sends (parameters, LIKE, the regular
// expressions, the raw prefilter) in Go. Every request must be a read-only POST.
type fakeCH struct {
	t           *testing.T
	files       []*chFile
	mu          sync.Mutex
	names       []string      // log_comment of each request, in order
	eventWhole  []string      // whole-text limits used by the event listing
	eventSQL    []string      // event projections, for the text-preservation contract
	gate        chan struct{} // when set, requests wait for it to close
	fail        map[string]bool
	legacyUsage bool
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
		case isCodexRollout(path):
			f.kind = "codex"
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
				f.events = append(f.events, chEvent{ev: ev, seq: seq, off: uint64(len(f.events))})
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
		if f.kind == "codex" {
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
	}
	gate := s.gate
	s.mu.Unlock()
	if gate != nil {
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
		// Claude replies and Codex token reports loaded before usage_session existed both carry an empty one
		if !strings.Contains(sql, "usage_session = '' AND (reply_id != '' OR (kind = 'codex' AND who = 'token_count'))") {
			s.t.Errorf("legacy usage query misses incomplete session rows: %s", sql)
		}
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
	case "link_names":
		for _, f := range s.files {
			row(f.path, f.session, f.kind)
		}
	case "shadow_files":
		for _, f := range s.files {
			row(f.path, f.lines)
		}
	case "usage":
		if !strings.Contains(sql, "SELECT usage_session, reply_id, if(reply_id = '', request_id, '') FROM events") {
			s.t.Errorf("usage query does not use the canonical session key: %s", sql)
		}
		type usageKey struct{ session, reply, request string }
		best := map[usageKey]struct {
			f                     *chFile
			e                     chEvent
			u                     *price.Usage
			model, reply, request string
		}{}
		for _, f := range s.files {
			for _, e := range f.events {
				if !s.match(q, sql, f, e) {
					continue
				}
				var u *price.Usage
				var model, reply, request string
				if e.ev.Reply != nil {
					model, reply, request, u = e.ev.Reply.Model, e.ev.Reply.ID, e.ev.Reply.RequestID, e.ev.Reply.Usage
				} else if e.ev.Tokens != nil {
					model, reply, u = e.ev.Tokens.Model, fmt.Sprintf("codex:%d", e.ev.Line), e.ev.Tokens.PriceUsage()
				}
				if u == nil || reply == "" && request == "" {
					continue
				}
				session := f.session
				if f.kind == "codex" {
					session = f.path
				} else if f.parent != "" {
					session = f.parent
				}
				if reply != "" {
					request = ""
				}
				k := usageKey{session, reply, request}
				old, ok := best[k]
				if !ok || u.Output > old.u.Output {
					best[k] = struct {
						f                     *chFile
						e                     chEvent
						u                     *price.Usage
						model, reply, request string
					}{f, e, u, model, reply, request}
				}
			}
		}
		for k, b := range best {
			_ = k
			fast := "0"
			if b.u.Fast {
				fast = "1"
			}
			row(b.f.id, ms(b.e.ev.Time), k.session, b.model, b.reply, b.request, b.u.Input, b.u.Output, b.u.CacheRead, b.u.CacheWrite,
				b.u.Write5m, b.u.Write1h, fast, b.u.Geo)
		}
	case "events":
		for _, f := range s.files {
			for _, e := range f.events {
				if !s.match(q, sql, f, e) {
					continue
				}
				ev := e.ev
				text, flat := ev.Shown(), 0
				if strings.Contains(sql, "substringUTF8(text") {
					cut, _ := strconv.Atoi(param("cut"))
					whole, _ := strconv.Atoi(param("whole"))
					truncateWhole := strings.Contains(sql, "lengthUTF8(text) <= {whole:UInt32}, substringUTF8(text")
					if r := []rune(text); len(r) > whole || truncateWhole && len(r) > cut {
						text, flat = string(r[:cut]), len([]rune(transcript.Short(ev.Shown(), 0)))
					}
				}
				row(f.id, ev.Line, e.seq, ms(ev.Time), ev.Role, ev.Who, ev.Subtype, text, flat, "all_1_1_0", e.off)
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

func fakeFileMatches(q url.Values, sql string, f *chFile) bool {
	param := func(name string) string { return q.Get("param_" + name) }
	if v := param("file_since"); v != "" && f.mtime.Before(chParseTime(nil, v)) {
		return false
	}
	if v := param("file_until"); v != "" && f.first.After(chParseTime(nil, v)) {
		return false
	}
	if strings.Contains(sql, " AND kind != 'codex' AND") && f.kind == "codex" || strings.Contains(sql, " AND kind != 'sub' AND") && f.kind == "sub" {
		return false
	}
	if project := param("project"); project != "" {
		if f.kind == "codex" {
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

// match is the WHERE of the events queries: the same conditions ClickHouse evaluates.
func (s *fakeCH) match(q url.Values, sql string, f *chFile, e chEvent) bool {
	param := func(name string) string { return q.Get("param_" + name) }
	ev := e.ev
	if !slices.Contains(chArray(param("files")), strconv.FormatUint(f.id, 10)) || !slices.Contains(chArray(param("kinds")), f.kind) ||
		!slices.Contains(chArray(param("roles")), string(ev.Role)) {
		return false
	}
	ts := time.UnixMilli(ms(ev.Time))
	if v := param("since"); v != "" && ts.Before(chParseTime(s.t, v)) {
		return false
	}
	if v := param("until"); v != "" && ts.After(chParseTime(s.t, v)) {
		return false
	}
	if strings.Contains(sql, "reply_id != '' OR request_id != ''") {
		if ev.Reply != nil && ev.Reply.ID == "" && ev.Reply.RequestID == "" || ev.Reply == nil && ev.Tokens == nil {
			return false
		}
	}
	text := ev.Who + " " + ev.Shown()
	hay := lowerUTF8(text)
	grep := sql
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
		userRecord("2026-10-25T10:11:00.000Z", "хвост: sub ещё не в базе"),
		agentRecord("2026-10-25T10:11:05.000Z", "Готово и хвост"),
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
	// texts up to wholeText come whole and are cut here; with 0 every long one comes as its start and the server's
	// count of its flattened length
	defer func(whole int) { wholeText = whole }(wholeText)
	for _, whole := range []int{wholeText, 0} {
		wholeText = whole
		for _, args := range dbForms {
			e.backend = backendFiles
			codeF, outF, _ := runTL(t, e, args...)
			e.backend = backendDB
			codeD, outD, errD := runTL(t, e, args...)
			if strings.Contains(errD, "instead") {
				t.Errorf("whole %d, %q: the database path fell back: %s", whole, args, errD)
			}
			if codeF != codeD || outF != outD {
				t.Errorf("whole %d, %q: files exit %d, db exit %d\nfiles:\n%s\ndb:\n%s", whole, args, codeF, codeD, outF, outD)
			}
			if outF == "" {
				t.Errorf("%q: empty answer", args)
			}
		}
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
	for _, sql := range ch.eventSQL {
		if strings.Contains(sql, "lengthUTF8(text) <= {whole:UInt32}, text, substringUTF8") {
			return
		}
	}
	t.Fatalf("event query does not preserve whole text below the whole-text limit: %v", ch.eventSQL)
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
	ch := newFakeCH(t, paths, map[string]int{})
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
	e.backend = backendFiles
	codeFiles, outFiles, _ := runTL(t, e, "--all", "--usage")
	e.backend = backendDB
	codeDB, outDB, warning := runTL(t, e, "--all", "--usage")
	if codeDB != codeFiles || outDB != outFiles || !strings.Contains(warning, "usage session metadata is incomplete") || !strings.Contains(warning, "reading files instead") {
		t.Fatalf("incomplete usage did not fall back: files=(%d,%q), db=(%d,%q,%q)", codeFiles, outFiles, codeDB, outDB, warning)
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
