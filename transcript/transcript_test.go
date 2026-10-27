package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readAll — события файла строками «строка роль who: текст».
func readAll(t *testing.T, path string, keep func([]byte) bool) ([]string, Meta) {
	t.Helper()
	var got []string
	meta, err := Read(path, keep, func(ev Event) {
		who := ""
		if ev.Who != "" {
			who = ev.Who + ": "
		}
		got = append(got, fmt.Sprintf("%d %s %s%s", ev.Line, ev.Role, who, ev.Shown()))
	})
	if err != nil {
		t.Fatal(err)
	}
	return got, meta
}

func TestReadClaude(t *testing.T) {
	got, meta := readAll(t, "testdata/claude.jsonl", nil)
	if meta.Name != "main" {
		t.Errorf("name %q", meta.Name)
	}
	want := []string{
		"3 user привет, сделай sub",
		"4 agent Делаю.",
		"4 cmd Bash: go test ./...",
		"5 out Bash: ok  akit/edit",
		"6 cmd AskUserQuestion: {\"questions\":[]}",
		"7 ask sub (Recommended) ← Какое имя?",
		"8 peer reviewer: имя — sub",
		"9 user и ещё вопрос",
		"10 note task-notification: <task-notification>done</task-notification>",
		"11 note compact: context compacted",
		"12 user смотри скрин\n[image]",
		"14 cmd SendMessage: → main: готово",
	}
	if strings.Join(got, "\n|") != strings.Join(want, "\n|") {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Поля записи, боковая ветка, картинки отдельно от слов, напоминания обвязки вырезаны.
func TestReadClaudeRecordFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	lines := []string{
		`{"type":"user","origin":{"kind":"human"},"uuid":"u1","sessionId":"s1","cwd":"/w/a","timestamp":"2026-10-25T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"глянь<system-reminder>\nобвязка\n</system-reminder>"},{"type":"image","source":{}}]}}`,
		`{"type":"user","origin":{"kind":"human"},"uuid":"u2","timestamp":"2026-10-25T10:00:01Z","message":{"role":"user","content":"<system-reminder>только обвязка</system-reminder>"}}`,
		`{"type":"assistant","uuid":"u3","cwd":"/w/b","isSidechain":true,"timestamp":"2026-10-25T10:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"раз"},{"type":"text","text":"два"}]}}`,
		`{"type":"user","origin":{"kind":"peer"},"timestamp":"2026-10-25T10:00:03Z","message":{"role":"user","content":"<cross-session-message from=\"x\" from-name=\"main\">\nделай\n</cross-session-message>\n<system-reminder>r</system-reminder>"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var evs []Event
	if _, err := Read(path, nil, func(ev Event) { evs = append(evs, ev) }); err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("ждали 4 события (реплика из одной обвязки — не реплика), %d: %+v", len(evs), evs)
	}
	if e := evs[0]; e.Text != "глянь" || e.Images != 1 || e.UUID != "u1" || e.SessionID != "s1" || e.Cwd != "/w/a" || e.Sidechain {
		t.Errorf("реплика: %+v", e)
	}
	if e := evs[0]; e.Shown() != "глянь\n[image]" {
		t.Errorf("показ: %q", e.Shown())
	}
	if a, b := evs[1], evs[2]; a.Text != "раз" || b.Text != "два" || a.UUID != "u3" || b.UUID != "u3" || a.Line != b.Line || !a.Sidechain || a.Cwd != "/w/b" {
		t.Errorf("части ответа — одна запись: %+v %+v", a, b)
	}
	if e := evs[3]; e.Role != Peer || e.Who != "main" || e.From != "x" || e.Text != "делай" {
		t.Errorf("письмо: %+v", e)
	}
}

func TestReadCodex(t *testing.T) {
	got, _ := readAll(t, "testdata/codex.jsonl", nil)
	want := []string{
		"3 user Бриф: добавь тест",
		"4 cmd exec: sub 'a' 'b' f.go",
		"5 out exec: sub: 1 replacement in 1 file: f.go 1 (line 3)\n",
		"6 cmd exec_command: go test ./...",
		"7 out exec_command: ok",
		"9 agent Готово.",
		"10 note compact: context compacted",
	}
	if strings.Join(got, "\n|") != strings.Join(want, "\n|") {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestReadKeepFilter(t *testing.T) {
	got, meta := readAll(t, "testdata/claude.jsonl", func(raw []byte) bool { return bytes.Contains(raw, []byte("sub")) })
	if meta.Name != "main" {
		t.Errorf("name lost with a raw filter: %q", meta.Name)
	}
	if len(got) != 3 || !strings.HasPrefix(got[0], "3 user") || !strings.HasPrefix(got[2], "8 peer") {
		t.Errorf("events: %q", got)
	}
}

func TestShort(t *testing.T) {
	if got := Short("  a\n  b  c ", 0); got != "a ⏎ b c" {
		t.Errorf("got %q", got)
	}
	if got := Short("абвгд", 3); got != "абв…(+2)" {
		t.Errorf("got %q", got)
	}
}

func TestReadBadLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	data := `{"type":"user","timestamp":"2026-10-25T10:00:00Z","origin":{"kind":"human"},"message":{"role":"user","content":"раз"}}
{"type":"user","timestamp":"кривая","origin":{"kind":"human"},"message":{"role":"user","content":"два"}}
{"type":"progress"}
{"type":"user","timestamp":"2026-10-25T10:0`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	got, meta := readAll(t, path, nil)
	if len(got) != 1 || got[0] != "1 user раз" || meta.Bad != 2 {
		t.Errorf("events %q, bad %d", got, meta.Bad)
	}
}

// events — события файла из строк lines.
func events(t *testing.T, lines ...string) ([]Event, Meta) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var evs []Event
	meta, err := Read(path, nil, func(ev Event) { evs = append(evs, ev) })
	if err != nil {
		t.Fatal(err)
	}
	return evs, meta
}

// Расход ответа, рассуждение, id вызова и ошибка вывода, вложение, ход и бриф.
func TestReadClaudeReplyAndCalls(t *testing.T) {
	evs, _ := events(t,
		`{"type":"user","promptId":"p1","timestamp":"2026-10-25T10:00:00Z","message":{"role":"user","content":"Ты суб-агент: сделай X"}}`,
		`{"type":"assistant","requestId":"r1","timestamp":"2026-10-25T10:00:01Z","message":{"id":"m1","model":"claude-opus-5-5","content":[{"type":"thinking","thinking":"","signature":"s"}],"usage":{"input_tokens":5,"output_tokens":1,"cache_read_input_tokens":7,"cache_creation_input_tokens":3,"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_1h_input_tokens":2},"speed":"fast","inference_geo":"us"}}}`,
		`{"type":"assistant","requestId":"r1","timestamp":"2026-10-25T10:00:02Z","message":{"id":"m1","model":"claude-opus-5-5","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"sleep"}}],"usage":{"input_tokens":5,"output_tokens":9}}}`,
		`{"type":"user","promptId":"p1","timestamp":"2026-10-25T10:00:03Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":[{"type":"text","text":"нет скилла"}]}]}}`,
		`{"type":"attachment","timestamp":"2026-10-25T10:00:04Z","attachment":{"type":"hook_success","hookEvent":"PostToolUse","stdout":"ок","toolUseID":"t1"}}`,
		`{"type":"assistant","timestamp":"2026-10-25T10:00:05Z","message":{"id":"m2","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"user","isMeta":true,"promptId":"p2","timestamp":"2026-10-25T10:00:06Z","message":{"role":"user","content":"Stop hook feedback: дальше"}}`,
	)
	got := make([]string, 0, len(evs))
	for _, e := range evs {
		got = append(got, fmt.Sprintf("%d %s %s %s", e.Line, e.Role, e.Who, e.Text))
	}
	want := []string{"1 note prompt Ты суб-агент: сделай X", "2 think  ", "3 cmd Skill {\"skill\":\"sleep\"}",
		"4 out Skill нет скилла", "5 attach hook_success ок", "6 note reply response without text", "7 note meta Stop hook feedback: дальше"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("события:\n%s\nждали:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if evs[0].PromptID != "p1" || evs[6].PromptID != "p2" {
		t.Errorf("ход: %q %q", evs[0].PromptID, evs[6].PromptID)
	}
	r := evs[1].Reply
	if r == nil || r.ID != "m1" || r.RequestID != "r1" || r.Model != "claude-opus-5-5" || r.Usage == nil {
		t.Fatalf("ответ: %+v", r)
	}
	if u := *r.Usage; u.Input != 5 || u.Output != 1 || u.CacheRead != 7 || u.CacheWrite != 3 || u.Write5m != 1 || u.Write1h != 2 || !u.Fast || u.Geo != "us" {
		t.Errorf("расход: %+v", u)
	}
	if c := evs[2]; c.CallID != "t1" || string(c.Raw) != `{"skill":"sleep"}` || c.Reply.StopReason != "tool_use" || c.Reply.Usage.Output != 9 {
		t.Errorf("вызов: %+v %+v", c, c.Reply)
	}
	if o := evs[3]; o.CallID != "t1" || !o.Error || o.Reply != nil || !strings.Contains(string(o.Raw), "нет скилла") {
		t.Errorf("вывод: %+v", o)
	}
	if a := evs[4]; !strings.Contains(string(a.Raw), `"hookEvent":"PostToolUse"`) {
		t.Errorf("вложение: %s", a.Raw)
	}
	if evs[5].Reply == nil || evs[5].Reply.ID != "m2" {
		t.Errorf("ответ без частей потерян: %+v", evs[5])
	}
}

// Сессия из session_meta, папка из turn_context, id вызова, ошибка exec, команды во всех кавычках.
func TestReadCodexSession(t *testing.T) {
	evs, meta := events(t,
		`{"timestamp":"2026-10-25T10:00:00Z","type":"session_meta","payload":{"id":"c1","cwd":"/w/a","originator":"codex-tui"}}`,
		`{"timestamp":"2026-10-25T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"деплой"}]}}`,
		`{"timestamp":"2026-10-25T10:00:02Z","type":"turn_context","payload":{"cwd":"/w/b"}}`,
		"{\"timestamp\":\"2026-10-25T10:00:03Z\",\"type\":\"response_item\",\"payload\":{\"type\":\"custom_tool_call\",\"name\":\"exec\",\"call_id\":\"k1\",\"input\":\"await exec_command({cmd: 'ls -a'}); await exec_command({cmd: `echo \\\"$X\\\"`}); exec_command({cmd: \\\"pwd\\\"})\"}}",
		`{"timestamp":"2026-10-25T10:00:04Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"k1","output":[{"type":"input_text","text":"Script failed\nWall time 0.1 s\nOutput:\nboom"}]}}`,
	)
	if meta.Originator != "codex-tui" {
		t.Errorf("originator %q", meta.Originator)
	}
	got := make([]string, 0, len(evs))
	for _, e := range evs {
		got = append(got, fmt.Sprintf("%s %s %s %s %s %v", e.Role, e.SessionID, e.Cwd, e.CallID, e.Text, e.Error))
	}
	want := []string{"user c1 /w/a  деплой false", "cmd c1 /w/b k1 ls -a false", `cmd c1 /w/b k1 echo "$X" false`,
		"cmd c1 /w/b k1 pwd false", "out c1 /w/b k1 boom true"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("события:\n%s\nждали:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	var code string
	if json.Unmarshal(evs[1].Raw, &code) != nil || !strings.HasPrefix(code, "await exec_command") {
		t.Errorf("ввод exec: %s", evs[1].Raw)
	}
}

// Записи system, toolUseResult у out и ask, реплика из очереди.
func TestReadClaudeSystemResultQueued(t *testing.T) {
	evs, _ := events(t,
		`{"type":"system","subtype":"compact_boundary","timestamp":"2026-10-25T10:00:00Z","content":"Conversation compacted","compactMetadata":{"durationMs":4200}}`,
		`{"type":"system","subtype":"informational","timestamp":"2026-10-25T10:00:01Z","content":"Compacted while idle, before the prompt cache expired"}`,
		`{"type":"system","subtype":"turn_duration","timestamp":"2026-10-25T10:00:02Z","durationMs":900}`,
		`{"type":"assistant","timestamp":"2026-10-25T10:00:03Z","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}},{"type":"tool_use","id":"t2","name":"AskUserQuestion","input":{}}]}}`,
		`{"type":"user","timestamp":"2026-10-25T10:00:04Z","toolUseResult":{"stdout":"a","persistedOutputPath":"/p/1.txt","persistedOutputSize":12},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a"}]}}`,
		`{"type":"user","timestamp":"2026-10-25T10:00:05Z","toolUseResult":{"answers":{"Куда?":"туда"}},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":"answered"}]}}`,
		`{"type":"attachment","timestamp":"2026-10-25T10:00:06Z","attachment":{"type":"queued_command","prompt":"и ещё","origin":{"kind":"human"},"commandMode":"prompt"}}`,
	)
	got := make([]string, 0, len(evs))
	for _, e := range evs {
		got = append(got, fmt.Sprintf("%s %s %s %s", e.Role, e.Who, e.Subtype, e.Text))
	}
	want := []string{"note system compact_boundary Conversation compacted",
		"note system informational Compacted while idle, before the prompt cache expired", "note system turn_duration ",
		"cmd Bash  ls", "cmd AskUserQuestion  {}", "out Bash  a", "ask   туда ← Куда?", "user   и ещё"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("события:\n%s\nждали:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	var meta struct {
		CompactMetadata struct{ DurationMs int64 } `json:"compactMetadata"`
	}
	if json.Unmarshal(evs[0].Raw, &meta) != nil || meta.CompactMetadata.DurationMs != 4200 {
		t.Errorf("запись system целиком: %s", evs[0].Raw)
	}
	if o := evs[5]; !strings.Contains(string(o.Result), `"persistedOutputPath":"/p/1.txt"`) {
		t.Errorf("toolUseResult у out: %s", o.Result)
	}
	if a := evs[6]; !a.Error || string(a.Raw) != `"answered"` || !strings.Contains(string(a.Result), "туда") {
		t.Errorf("ask: error %v raw %s result %s", a.Error, a.Raw, a.Result)
	}
	if q := evs[7]; !q.Queued || !strings.Contains(string(q.Raw), `"commandMode":"prompt"`) {
		t.Errorf("из очереди: %v %s", q.Queued, q.Raw)
	}
	if evs[3].Queued || evs[3].Result != nil {
		t.Error("признаки у чужих событий")
	}
}

// TUI: реплики и текст агента — из event_msg, response_item с ними — нет; расход token_count; сессия — первая.
func TestReadCodexEvents(t *testing.T) {
	evs, meta := events(t,
		`{"timestamp":"2026-10-25T10:00:00Z","type":"session_meta","payload":{"id":"child","cwd":"/w","originator":"codex_exec","creator_account_id":"acc1"}}`,
		`{"timestamp":"2026-10-25T10:00:00Z","type":"session_meta","payload":{"id":"parent","cwd":"/p","originator":"codex-tui","creator_account_id":"acc2"}}`,
		`{"timestamp":"2026-10-25T10:00:01Z","type":"turn_context","payload":{"cwd":"/w","model":"gpt-6-luna"}}`,
		`{"timestamp":"2026-10-25T10:00:02.001Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"посмотри"},{"type":"input_image","image_url":"data:"}]}}`,
		`{"timestamp":"2026-10-25T10:00:02Z","type":"event_msg","payload":{"type":"user_message","message":"посмотри","images":[],"local_images":["/x.png"]}}`,
		`{"timestamp":"2026-10-25T10:00:03Z","type":"event_msg","payload":{"type":"agent_message","message":"смотрю"}}`,
		`{"timestamp":"2026-10-25T10:00:03.001Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"смотрю"}]}}`,
		`{"timestamp":"2026-10-25T10:00:04Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<turn_aborted>\nThe user interrupted"}]}}`,
		`{"timestamp":"2026-10-25T10:00:05Z","type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":null}}`,
		`{"timestamp":"2026-10-25T10:00:06Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":60,"output_tokens":7,"reasoning_output_tokens":3,"total_tokens":107},"last_token_usage":{"input_tokens":50,"total_tokens":52},"model_context_window":258400},"rate_limits":{"primary":{"used_percent":31,"window_minutes":10080,"resets_at":1793082766},"secondary":null,"plan_type":"prolite"}}}`,
	)
	if meta.Originator != "codex_exec" || meta.Account != "acc1" {
		t.Errorf("meta %+v", meta)
	}
	got := make([]string, 0, len(evs))
	for _, e := range evs {
		got = append(got, fmt.Sprintf("%s %s %s %s %d", e.Role, e.SessionID, e.Time.Format("05.000"), e.Text, e.Images))
	}
	want := []string{"user child 02.000 посмотри 1", "agent child 03.000 смотрю 0",
		"note child 06.000 tokens total 107, last 52; limit 31% of 10080 min 0"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("события:\n%s\nждали:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	tk := evs[2].Tokens
	if tk == nil || tk.Model != "gpt-6-luna" || tk.Total.CachedInput != 60 || tk.Total.ReasoningOutput != 3 ||
		tk.ContextWindow != 258400 || tk.Limits.PlanType != "prolite" || tk.Limits.Primary.ResetsAt != 1793082766 ||
		tk.Limits.Secondary != nil {
		t.Errorf("расход: %+v %+v", tk, tk.Limits)
	}
}

// exec без событий user_message и agent_message: реплики и текст агента — из response_item.
func TestReadCodexExecMessages(t *testing.T) {
	evs, _ := events(t,
		`{"timestamp":"2026-10-25T10:00:00Z","type":"session_meta","payload":{"id":"c1","originator":"codex_exec"}}`,
		`{"timestamp":"2026-10-25T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"бриф: agent_message"}]}}`,
		`{"timestamp":"2026-10-25T10:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"готово"}]}}`,
	)
	if len(evs) != 2 || evs[0].Text != "бриф: agent_message" || evs[1].Role != Agent {
		t.Errorf("события %+v", evs)
	}
}

func TestWithoutResults(t *testing.T) {
	evs, _ := events(t,
		`{"type":"assistant","timestamp":"2026-10-25T10:00:00Z","message":{"id":"m1","content":[{"type":"text","text":"пишу \"type\":\"tool_result\""}],"usage":{"output_tokens":1}}}`,
		`{"type":"user","timestamp":"2026-10-25T10:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"x"}]}}`,
	)
	if len(evs) != 2 {
		t.Fatalf("без отсева %d событий", len(evs))
	}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	data := `{"type":"assistant","timestamp":"2026-10-25T10:00:00Z","message":{"id":"m1","content":[{"type":"text","text":"пишу \"type\":\"tool_result\""}],"usage":{"output_tokens":1}}}
{"type":"user","timestamp":"2026-10-25T10:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"x"}]}}
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ := readAll(t, path, WithoutResults)
	if len(got) != 1 || !strings.HasPrefix(got[0], "1 agent") {
		t.Errorf("отсев: %q", got)
	}
}

// Parser по строкам: тот же разбор, что у Read, имя инструмента помнится между вызовами.
func TestParser(t *testing.T) {
	p := NewParser()
	lines := []string{
		`{"type":"agent-name","agentName":"main"}`,
		`{"type":"user","timestamp":"2026-10-25T10:00:00Z","origin":{"kind":"human"},"message":{"role":"user","content":"сделай<system-reminder>шум</system-reminder>"}}`,
		`{"type":"assistant","timestamp":"2026-10-25T10:00:01Z","message":{"id":"m1","content":[{"type":"text","text":"Куда?"},{"type":"tool_use","id":"t2","name":"AskUserQuestion","input":{}}]}}`,
		`{"type":"user","timestamp":"2026-10-25T10:00:02Z","toolUseResult":{"answers":{"Куда?":"туда"}},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"answered"}]}}`,
	}
	var got []string
	for _, l := range lines {
		evs, err := p.Parse([]byte(l))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			got = append(got, fmt.Sprintf("%d %s %s", e.Line, e.Role, e.Text))
		}
	}
	want := []string{"0 user сделай", "0 agent Куда?", "0 cmd {}", "0 ask туда ← Куда?"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("события:\n%s\nждали:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := p.Parse([]byte(`{"type":"user","timest`)); err == nil {
		t.Error("недописанная строка без ошибки")
	}
}

// Копия rollout фермы: session_meta после событий — новая сессия; учётка расхода — последнего session_meta;
// last_token_usage нет — Last nil; пустой вывод exec — тоже событие out.
func TestReadCodexSlices(t *testing.T) {
	tc := func(ts string, total int, last string) string {
		return `{"timestamp":"2026-10-25T10:00:0` + ts + `Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":` +
			fmt.Sprint(total) + `}` + last + `}}}`
	}
	evs, meta := events(t,
		tc("0", 10, `,"last_token_usage":{"total_tokens":0}`),
		`{"timestamp":"2026-10-25T10:00:01Z","type":"session_meta","payload":{"id":"s1","cwd":"/a","creator_account_id":"acc1"}}`,
		`{"timestamp":"2026-10-25T10:00:01Z","type":"session_meta","payload":{"id":"parent","cwd":"/p","creator_account_id":"acc1"}}`,
		`{"timestamp":"2026-10-25T10:00:02Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"c1","input":"true"}}`,
		`{"timestamp":"2026-10-25T10:00:03Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c1","output":"Script completed\nWall time 0.1s\nOutput:\n"}}`,
		tc("4", 20, ""),
		`{"timestamp":"2026-10-25T10:00:05Z","type":"session_meta","payload":{"id":"s2","cwd":"/b","creator_account_id":"acc2"}}`,
		tc("6", 5, `,"last_token_usage":{"total_tokens":5}`),
	)
	if meta.Account != "acc1" {
		t.Errorf("Meta.Account %q, want the first", meta.Account)
	}
	got := make([]string, 0, len(evs))
	for _, e := range evs {
		acc := ""
		if e.Tokens != nil {
			acc = e.Tokens.Account
		}
		got = append(got, fmt.Sprintf("%s %s %s %s %q", e.Role, e.SessionID, e.Cwd, acc, e.Text))
	}
	want := []string{`note    "tokens total 10, last 0"`, `cmd s1 /a  "true"`, `out s1 /a  ""`,
		`note s1 /a acc1 "tokens total 20"`, `note s2 /b acc2 "tokens total 5, last 5"`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("события:\n%s\nждали:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if evs[0].Tokens.Last == nil || evs[3].Tokens.Last != nil {
		t.Error("Last: нули и отсутствие не различаются")
	}
}

// exec_command с пустой командой (так пишут бойцы codex): событие cmd с кодом вызова как есть, вывод — к нему.
func TestReadCodexEmptyCmd(t *testing.T) {
	evs, _ := events(t,
		`{"timestamp":"2026-10-25T10:00:00Z","type":"session_meta","payload":{"id":"s1"}}`,
		`{"timestamp":"2026-10-25T10:00:01Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"const r=await tools.exec_command({cmd:\"\",workdir:\"/w\"});text(r.output)\n"}}`,
		`{"timestamp":"2026-10-25T10:00:02Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c1","output":[{"type":"input_text","text":"Script completed\nWall time 0.1 seconds\nOutput:\n"}]}}`,
		`{"timestamp":"2026-10-25T10:00:03Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"c2","name":"exec","input":"await tools.exec_command({cmd:\"\"}); await tools.exec_command({cmd:\"ls\"})"}}`,
	)
	got := make([]string, 0, len(evs))
	for _, e := range evs {
		got = append(got, fmt.Sprintf("%s %s %s %q", e.Role, e.Who, e.CallID, e.Text))
	}
	want := []string{`cmd exec c1 "const r=await tools.exec_command({cmd:\"\",workdir:\"/w\"});text(r.output)\n"`,
		`out exec c1 ""`, `cmd exec c2 "ls"`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("события:\n%s\nждали:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
