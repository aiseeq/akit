package main

import (
	"bytes"
	"encoding/json"
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

	"github.com/aiseeq/akit/transcript"
)

// fakeCH answers the queries of the database path the way ClickHouse would over tables the loader filled from
// the transcripts of a synthetic home: it evaluates the conditions the client sends (parameters, LIKE, the regular
// expressions, the raw prefilter) in Go. Every request must be a read-only POST.
type fakeCH struct {
	t     *testing.T
	files []*chFile
	mu    sync.Mutex
	names []string      // log_comment of each request, in order
	gate  chan struct{} // when set, requests wait for it to close
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
	off uint64
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
		if sourceUUID.MatchString(filepath.Base(path)) {
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
		meta, err := transcript.Read(path, nil, func(ev transcript.Event) {
			if ev.Line <= n {
				f.events = append(f.events, chEvent{ev: ev, off: uint64(len(f.events))})
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
	gate := s.gate
	s.mu.Unlock()
	if gate != nil {
		<-gate
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
			row(f.path)
		}
	case "shadow_files":
		for _, f := range s.files {
			row(f.path, f.lines)
		}
	case "events", "usage":
		for _, f := range s.files {
			for _, e := range f.events {
				if !s.match(q, sql, f, e) {
					continue
				}
				ev := e.ev
				if name == "usage" {
					model, id, req := "", "", ""
					if u := ev.Reply; u != nil {
						model, id, req = u.Model, u.ID, u.RequestID
					}
					row(f.id, ev.Line, ms(ev.Time), ev.Role, model, id, req, "all_1_1_0", e.off)
					continue
				}
				text, flat := ev.Shown(), 0
				if strings.Contains(sql, "substringUTF8(text") {
					cut, _ := strconv.Atoi(param("cut"))
					if r := []rune(text); len(r) > cut {
						text, flat = string(r[:cut]), len([]rune(transcript.Short(ev.Shown(), 0)))
					}
				}
				row(f.id, ev.Line, ms(ev.Time), ev.Role, ev.Who, ev.Subtype, text, flat, "all_1_1_0", e.off)
			}
		}
	case "usage-raw":
		type key struct {
			file uint64
			id   string
		}
		best := map[key]chEvent{}
		var order []key
		byKey := map[key]*chFile{}
		for _, f := range s.files {
			for _, e := range f.events {
				rp := e.ev.Reply
				if !s.match(q, sql, f, e) || rp == nil {
					continue
				}
				k := key{f.id, rp.ID}
				if k.id == "" {
					k.id = rp.RequestID
				}
				old, ok := best[k]
				if !ok {
					order = append(order, k)
					byKey[k] = f
				}
				if !ok || outTokens(e) > outTokens(old) {
					best[k] = e
				}
			}
		}
		for _, k := range order {
			e := best[k]
			var rec struct {
				Message struct {
					Usage json.RawMessage `json:"usage"`
				} `json:"message"`
			}
			_ = json.Unmarshal([]byte(byKey[k].raw[e.ev.Line-1]), &rec)
			row(k.file, e.ev.Line, string(rec.Message.Usage))
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
					row(ms(e.ev.Time), e.ev.Role, e.ev.Who, e.ev.Shown(), "all_1_1_0", e.off)
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

func outTokens(e chEvent) int64 {
	if e.ev.Reply == nil || e.ev.Reply.Usage == nil {
		return 0
	}
	return e.ev.Reply.Usage.Output
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
	if strings.Contains(sql, "reply_id != '' OR request_id != ''") && (ev.Reply == nil || ev.Reply.ID == "" && ev.Reply.RequestID == "") {
		return false
	}
	text := ev.Who + " " + ev.Shown()
	hay := lowerUTF8(text)
	grep := sql
	if i := strings.Index(grep, "(file_id, line) IN (SELECT"); i >= 0 {
		grep = grep[:i]
	}
	// every condition over the text the query has must hold; the server's own semantics of each
	for _, m := range regexp.MustCompile(`hay LIKE \{(\w+):String\}`).FindAllStringSubmatch(grep, -1) {
		// a group of LIKEs joined by OR holds when one of them does
		if group := orGroup(grep, m[0]); group != nil {
			if !slices.ContainsFunc(group, func(name string) bool { return likeMatch(hay, tsvUnescape(param(name))) }) {
				return false
			}
			continue
		}
		if !likeMatch(hay, tsvUnescape(param(m[1]))) {
			return false
		}
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

// orGroup is the names of the LIKEs of the parenthesized OR group cond stands in, or nil when it stands alone.
func orGroup(sql, cond string) []string {
	for _, g := range regexp.MustCompile(`\(([^()]*)\)`).FindAllStringSubmatch(sql, -1) {
		if strings.Contains(g[1], cond) && strings.Contains(g[1], " OR ") {
			var names []string
			for _, m := range regexp.MustCompile(`hay LIKE \{(\w+):String\}`).FindAllStringSubmatch(g[1], -1) {
				names = append(names, m[1])
			}
			return names
		}
	}
	return nil
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
		for _, name := range []string{"files", "events", "texts", "bad", "usage", "usage-raw", "event"} {
			if !slices.Contains(ch.requests(), name) {
				t.Errorf("no %s query", name)
			}
		}
	}()
	for _, args := range dbForms {
		e.backend = backendFiles
		codeF, outF, _ := runTL(t, e, args...)
		e.backend = backendDB
		codeD, outD, errD := runTL(t, e, args...)
		if strings.Contains(errD, "instead") {
			t.Errorf("%q: the database path fell back: %s", args, errD)
		}
		if codeF != codeD || outF != outD {
			t.Errorf("%q: files exit %d, db exit %d\nfiles:\n%s\ndb:\n%s", args, codeF, codeD, outF, outD)
		}
		if outF == "" {
			t.Errorf("%q: empty answer", args)
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

func TestDBDoesNotSeeFilesMissingFromFilesTable(t *testing.T) {
	e, paths, _ := dbCorpus(t)
	ch := newFakeCH(t, paths, map[string]int{})
	ch.files = ch.files[:1]
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	e.backend = backendDB
	code, out, stderr := runTL(t, e, dbOtherSession[:8], "--role", "all")
	if code != exitUsage || out != "" || !strings.Contains(stderr, "no transcript with this id in the database") {
		t.Fatalf("database exposed a path absent from files FINAL: code=%d out=%q stderr=%q", code, out, stderr)
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
		rows, err := c.selectTSV(t.Context(), chQuery{name: "test-flatlen", sql: "SELECT " + flatLenSQL + " FROM (SELECT {t:String} AS text)",
			params: map[string]string{"t": chString(text), "ws": chString(spaceClass + "+"), "trim": chString("^" + spaceClass + "+|" + spaceClass + "+$")}})
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
		rows, err := c.selectTSV(t.Context(), chQuery{name: "test-grep",
			sql: "SELECT " + cond + " FROM (SELECT '' AS who, {t:String} AS text, lowerUTF8(concat(who, ' ', text)) AS hay)", params: params})
		if err != nil {
			t.Fatal(err)
		}
		want := regexp.MustCompile("(?i)" + c2.pattern).MatchString(" " + c2.text)
		if len(rows) != 1 || (rows[0][0] == "1") != want {
			t.Errorf("%q over %q: server %v, Go %v", c2.pattern, c2.text, rows, want)
		}
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
