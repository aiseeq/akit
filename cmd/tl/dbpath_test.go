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
	id               uint64
	path, name, kind string
	ingested         int64
	lines, evLines   int
	mtime, loaded    time.Time
	events           []chEvent
	raw              []string // the lines, from 1
	bad              int
}

type chEvent struct {
	ev  transcript.Event
	off uint64
}

// newFakeCH loads the transcripts; unloaded names files whose last lines the loader has not read yet.
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
		for i, l := range lines {
			if i < n {
				f.ingested += int64(len(l))
			}
			f.raw = append(f.raw, strings.TrimRight(l, "\r\n"))
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		f.mtime = st.ModTime()
		switch {
		case isCodexRollout(path):
			f.kind = "codex"
		case strings.Contains(path, string(filepath.Separator)+"subagents"+string(filepath.Separator)):
			f.kind = "sub"
		default:
			f.kind = "claude"
		}
		// the events of the unloaded lines are there too, as after a pass whose clean row is not written yet: the
		// files row says what counts
		meta, err := transcript.Read(path, nil, func(ev transcript.Event) {
			f.events = append(f.events, chEvent{ev: ev, off: uint64(len(f.events))})
		})
		if err != nil {
			t.Fatal(err)
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
			row(f.id, f.path, f.name, f.ingested, f.lines, f.evLines, f.mtime.Unix(), f.loaded.UnixMilli(), f.bad)
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
				if !json.Valid([]byte(raw)) {
					row(f.id, i+1, raw)
				}
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
	hay := strings.ToLower(ev.Who + " " + ev.Shown())
	switch {
	case strings.Contains(sql, "hay LIKE {grep:String}"):
		like := tsvUnescape(param("grep"))
		lit := strings.NewReplacer(`\%`, `%`, `\_`, `_`, `\\`, `\`).Replace(strings.TrimSuffix(strings.TrimPrefix(like, "%"), "%"))
		if !strings.Contains(hay, lit) {
			return false
		}
	case strings.Contains(sql, "multiMatchAny(hay"):
		if !regexp.MustCompile(chArray(param("grep"))[0]).MatchString(hay) {
			return false
		}
	case strings.Contains(sql, "match(hay, {grep:String})"):
		if !regexp.MustCompile(tsvUnescape(param("grep"))).MatchString(hay) {
			return false
		}
	case strings.Contains(sql, "match(concat(who, ' ', text), {grep:String})"):
		if !regexp.MustCompile(tsvUnescape(param("grep"))).MatchString(ev.Who + " " + ev.Shown()) {
			return false
		}
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
// a subagent, and a codex rollout. Its last two lines are not loaded yet.
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
	codex, err := os.ReadFile("../../transcript/testdata/codex.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(e.home, ".local", "state", "worker", "w1", "codex", "sessions", "2026", "10", "25",
		"rollout-2026-10-25T10-47-55-0199aaaa-0000-7000-8000-000000000001.jsonl")
	writeWorkFile(t, rollout, strings.Split(strings.TrimSuffix(string(codex), "\n"), "\n"))
	main := filepath.Join(e.home, ".claude", "projects", "-home-x-proj", sessionID+".jsonl")
	return e, []string{main, other, sub, rollout}, map[string]int{other: 2}
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

// The database path prints what the file path prints, over the same corpus: listings, filters, cut and masked
// texts, the lines the loader has not read, --usage, sessions with subagents and single events.
func TestDBPrintsAsFiles(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	ch := newFakeCH(t, paths, unloaded)
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

func TestLowerPattern(t *testing.T) {
	for _, c := range []struct{ in, lowered, literal string }{
		{"Мера", "мера", "мера"},
		{"wfor", "wfor", "wfor"},
		{`прост[а-я]* решени|КОСТЫЛ`, "", ""}, // checked by matching below
		{`\S+Error`, "", ""},
	} {
		lowered, literal, err := lowerPattern(c.in)
		if err != nil {
			t.Fatal(err)
		}
		if c.lowered != "" && (lowered != c.lowered || literal != c.literal) {
			t.Errorf("%q: %q %q, want %q %q", c.in, lowered, literal, c.lowered, c.literal)
		}
		if strings.Contains(lowered, "(?i") {
			t.Errorf("%q: lowered %q keeps case folding", c.in, lowered)
		}
		re := regexp.MustCompile(lowered)
		orig := regexp.MustCompile("(?i)" + c.in)
		for _, s := range []string{"мера", "МЕРА", "wfor", "простое решение", "КОСТЫЛЬ", "костыль", "runtime Error", "x Error", " Error"} {
			if l := strings.ToLower(s); orig.MatchString(s) != re.MatchString(l) {
				t.Errorf("%q over %q: (?i) %v, lowered over lower case %v", c.in, s, orig.MatchString(s), re.MatchString(l))
			}
		}
	}
	for pattern, want := range map[string]bool{"(?-i:Готово)": true, "готово": false, "(?-i:123)": false, "[^a]": false, "(?-i)[a-z]": true} {
		if got, err := caseSensitiveParts(pattern); err != nil || got != want {
			t.Errorf("caseSensitiveParts(%q) %v %v, want %v", pattern, got, err, want)
		}
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

// multiMatchAny over the lower-case text matches what (?i) matches.
func TestLiveGrepLowered(t *testing.T) {
	c := liveDB(t)
	for _, c2 := range []struct{ pattern, text string }{{"прост[а-я]* решени|КОСТЫЛ", "Простое решение"}, {`[A-Z]{2,6}-[0-9]{3,5}`, "тикет ab-1234"}, {"Мера", "МЕРА"}} {
		cond, params, err := grepCondition(c2.pattern, grepFast)
		if err != nil {
			t.Fatal(err)
		}
		params["t"] = chString(c2.text)
		rows, err := c.selectTSV(t.Context(), chQuery{name: "test-grep", sql: "SELECT " + cond + " FROM (SELECT lowerUTF8(concat('', ' ', {t:String})) AS hay)", params: params})
		if err != nil {
			t.Fatal(err)
		}
		want := regexp.MustCompile("(?i)" + c2.pattern).MatchString(" " + c2.text)
		if len(rows) != 1 || (rows[0][0] == "1") != want {
			t.Errorf("%q over %q: server %v, Go %v", c2.pattern, c2.text, rows, want)
		}
	}
}
