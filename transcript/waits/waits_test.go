package waits

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/aiseeq/akit/price"
	"github.com/aiseeq/akit/transcript"
)

// events — события одного файла по порядку; at — секунды от начала.
type events struct {
	t0    time.Time
	line  int
	evs   []transcript.Event
	calls int
}

func newEvents() *events { return &events{t0: time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)} }

func (s *events) add(e transcript.Event, at int) string {
	s.line++
	e.Line, e.Time = s.line, s.t0.Add(time.Duration(at)*time.Second)
	s.evs = append(s.evs, e)
	return e.CallID
}

// bash — вызов Bash с ответом reply (расход 100 на ответ); bg — run_in_background.
func (s *events) bash(cmd string, at int, reply string, bg bool) string {
	s.calls++
	raw, _ := json.Marshal(map[string]any{"command": cmd, "run_in_background": bg})
	return s.add(transcript.Event{Role: transcript.Cmd, Who: "Bash", Text: cmd, CallID: fmt.Sprintf("c%d", s.calls), Raw: raw,
		Reply: &transcript.Reply{ID: reply, Usage: &price.Usage{Input: 10, CacheRead: 80, CacheWrite: 5, Output: 5}}}, at)
}

func (s *events) tool(who, text string, raw any, at int) string {
	s.calls++
	r, _ := json.Marshal(raw)
	return s.add(transcript.Event{Role: transcript.Cmd, Who: who, Text: text, CallID: fmt.Sprintf("c%d", s.calls), Raw: r}, at)
}

func (s *events) out(id, text string, at int) {
	s.add(transcript.Event{Role: transcript.Out, CallID: id, Text: text}, at)
}

// run — все события через распознаватель и Close.
func (s *events) run() []Wait {
	r := New()
	var got []Wait
	for _, e := range s.evs {
		got = append(got, r.Add(e)...)
	}
	return append(got, r.Close()...)
}

func one(t *testing.T, got []Wait) Wait {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("ждали одно ожидание, %d: %+v", len(got), got)
	}
	return got[0]
}

// Суб-агент ждёт лог по правилу 270 с: два вызова упёрлись в timeout, третий дождался; Edit после — конец.
func TestLoopRepeats(t *testing.T) {
	s := newEvents()
	cmd := `timeout 270 bash -c 'until grep -q "== done" /tmp/x/test.log; do sleep 5; done'; tail -3 /tmp/x/test.log`
	a := s.bash(cmd, 0, "r1", false)
	s.out(a, "", 270)
	b := s.bash(cmd, 275, "r2", false)
	s.out(b, "", 545)
	c := s.bash(cmd, 550, "r3", false)
	s.out(c, "ok", 580)
	s.tool("Edit", "f.go", map[string]string{"file_path": "f.go"}, 600)
	w := one(t, s.run())
	if w.How != Loop || w.Calls != 3 || w.Repeats != 2 || w.Outcome != Done || w.Class != "test" || w.Line != 1 ||
		w.Tokens != 300 || w.End.Sub(w.Start) != 580*time.Second || len(w.CallIDs) != 3 {
		t.Errorf("цикл с повторами: %+v", w)
	}
}

// Последний вызов упёрся в timeout и агент ушёл делать другое — исход timeout.
func TestLoopTimeout(t *testing.T) {
	s := newEvents()
	a := s.bash(`timeout 60 bash -c 'while kill -0 123; do sleep 10; done'`, 0, "r1", false)
	s.out(a, "", 60)
	s.bash("ls", 70, "r2", false)
	if w := one(t, s.run()); w.Outcome != Timeout || w.Calls != 1 {
		t.Errorf("timeout: %+v", w)
	}
}

func TestWfor(t *testing.T) {
	s := newEvents()
	a := s.bash(`cd ~/git/x && wfor --timeout 4m line '^(ok|FAIL)' test.log`, 0, "r1", false)
	s.out(a, "wfor: not yet after 4m0s: line ...", 240)
	b := s.bash(`wfor line '^(ok|FAIL)' test.log`, 241, "r1", false)
	s.out(b, "wfor: done in 12s", 253)
	s.bash("wfor -h", 260, "r2", false) // справка — не ожидание, конец цепочки
	w := one(t, s.run())
	if w.How != Wfor || w.Calls != 2 || w.Outcome != Done || w.Tokens != 100 {
		t.Errorf("wfor в два шага, один ответ — расход один раз: %+v", w)
	}
}

// Не ожидания: цикл в теле heredoc, короткий sleep, ввод в запущенную команду codex.
func TestNotWaits(t *testing.T) {
	s := newEvents()
	s.bash("cat > t.sh <<'EOF'\nwhile true; do sleep 5; done\nEOF\nchmod +x t.sh", 0, "r1", false)
	s.bash("sleep 1 && ls", 1, "r2", false)
	s.bash(`for f in a b; do mv $f ${f/sleep-trees/dream-trees}; done`, 1, "r2", false)
	s.tool("exec", `const r = await tools.write_stdin({session_id:5,chars:"y\n",yield_time_ms:1000})`, "x", 2)
	s.tool("write_stdin", `{"session_id":5,"chars":"q"}`, map[string]any{"session_id": 5, "chars": "q"}, 3)
	if got := s.run(); len(got) != 0 {
		t.Errorf("не ожидания: %+v", got)
	}
}

func TestSleepAndScript(t *testing.T) {
	s := newEvents()
	a := s.bash("sleep 30; date", 0, "r1", false)
	s.out(a, "", 30)
	s.tool("Read", "x", map[string]string{}, 31)
	b := s.bash("~/bin/wait-deploy.sh prod", 40, "r2", false)
	s.out(b, "", 100)
	got := s.run()
	if len(got) != 2 || got[0].How != Sleep || got[0].Outcome != Done || got[1].How != Script || got[1].Class != "deploy" {
		t.Errorf("sleep и скрипт: %+v", got)
	}
}

// Фоновая команда: класс цикла, который ждёт её вывод, — по её команде; цикл в фоне кончается уведомлением.
func TestBackground(t *testing.T) {
	s := newEvents()
	a := s.bash("go test ./...", 0, "r1", true)
	s.out(a, "Command running in background with ID: b123. Output is being written to: /t/tasks/b123.output", 1)
	b := s.bash(`timeout 600 bash -c 'until grep -q EXIT /t/tasks/b123.output; do sleep 15; done'`, 2, "r2", true)
	s.out(b, "Command running in background with ID: b124.", 3)
	note := func(id, status, code string, at int) {
		s.add(transcript.Event{Role: transcript.Note, Who: "task-notification", Text: "<task-notification>\n<task-id>x</task-id>\n<tool-use-id>" +
			id + "</tool-use-id>\n<status>" + status + "</status>\n<summary>Background command completed (exit code " + code + ")</summary>"}, at)
	}
	note(a, "completed", "0", 300)
	note(b, "failed", "124", 602)
	got := s.run()
	if len(got) != 2 {
		t.Fatalf("два фоновых: %+v", got)
	}
	if w := got[0]; w.How != Background || w.Class != "test" || w.Outcome != Done || w.End.Sub(w.Start) != 300*time.Second {
		t.Errorf("фоновая команда: %+v", w)
	}
	if w := got[1]; w.How != Loop || w.Class != "test" || w.Outcome != Timeout {
		t.Errorf("цикл в фоне: %+v", w)
	}
}

// Bash не уложился в таймаут инструмента и ушёл в фон: ожидание длится до уведомления.
func TestMovedToBackground(t *testing.T) {
	s := newEvents()
	a := s.bash(`until [ -s /t/out.json ]; do sleep 20; done`, 0, "r1", false)
	s.out(a, "Command did not complete within its 120s timeout and was moved to the background (ID: b9).", 120)
	s.bash("ls", 121, "r2", false)
	s.add(transcript.Event{Role: transcript.Note, Who: "task-notification", Text: "<tool-use-id>" + a + "</tool-use-id><status>completed</status>"}, 400)
	if w := one(t, s.run()); w.How != Loop || w.Outcome != Done || w.End.Sub(w.Start) != 400*time.Second {
		t.Errorf("ушёл в фон: %+v", w)
	}
}

// codex: команда ушла в сессию, Luna опрашивает её пустыми write_stdin; расход — token_count хода вызова.
func TestCodexStdinPoll(t *testing.T) {
	s := newEvents()
	tokens := func(n int64, at int) {
		s.add(transcript.Event{Role: transcript.Note, Who: "token_count", Tokens: &transcript.Tokens{Last: &transcript.TokenUsage{Total: n}}}, at)
	}
	a := s.tool("exec", "tools/repo/gotest.sh -run TestX ./bot/...", "x", 0)
	tokens(1000, 0)
	s.out(a, "идёт, session 77", 10)
	poll := `const r = await tools.write_stdin({session_id:77,yield_time_ms:30000,max_output_tokens:3000}); text(r.output)`
	for i, at := range []int{11, 42, 73} {
		if i == 1 {
			s.tool("exec", "ps -o pid,etime -p 123", "x", at-1) // проверка между опросами — то же ожидание
		}
		id := s.tool("exec", poll, "x", at)
		tokens(500, at)
		out := "идёт, session 77"
		if i == 2 {
			out = "ok  bot 41s"
		}
		s.out(id, out, at+30)
	}
	// Ячейка exec печатает ответ инструмента JSON: команда идёт — есть session_id и нет exit_code. Опросы двух сессий
	// чередуются, между ними — короткая команда (у неё exit_code): ни команду сессии, ни её ожидание она не меняет.
	j := s.tool("exec", "go list -m all", "x", 100)
	s.out(j, `{"chunk_id":"a1","wall_time_seconds":10.0,"session_id":20238,"original_token_count":0,"output":""}`, 101)
	jpoll := `const r = await tools.write_stdin({session_id:20238,chars:"",yield_time_ms:30000}); text(JSON.stringify(r))`
	for i, at := range []int{102, 133, 164} {
		if i == 1 {
			q := s.tool("exec", "rg -n x a.go", "x", at-1)
			s.out(q, `{"chunk_id":"b5","wall_time_seconds":0.000007,"exit_code":0,"output":"1:x"}`, at-1)
		}
		id := s.tool("exec", jpoll, "x", at)
		out := `{"chunk_id":"c","wall_time_seconds":30.0,"session_id":20238,"original_token_count":0,"output":""}`
		if i == 2 {
			out = `{"chunk_id":"d","wall_time_seconds":3.0,"exit_code":0,"original_token_count":5,"output":"ok"}`
		}
		s.out(id, out, at+30)
	}
	b := s.tool("exec", "go build ./...", "x", 210)
	s.out(b, "Process running with session ID 78", 220)
	c := s.tool("write_stdin", `{"session_id":78}`, map[string]any{"session_id": 78}, 221)
	s.out(c, "Process running with session ID 78", 251)
	s.tool("exec", `const patch = "*** Begin Patch\n*** Update File: a.go"`, "x", 260)
	got := s.run()
	if len(got) != 3 {
		t.Fatalf("три ожидания: %+v", got)
	}
	if w := got[1]; w.How != StdinPoll || w.Calls != 3 || w.Outcome != Done || w.Command != "go list -m all" {
		t.Errorf("опросы через JSON ячейки: %+v", w)
	}
	got[1] = got[2]
	if w := got[0]; w.How != StdinPoll || w.Calls != 3 || w.Outcome != Done || w.Class != "test" || w.Tokens != 1500 ||
		w.Command != "tools/repo/gotest.sh -run TestX ./bot/..." {
		t.Errorf("опросы codex: %+v", w)
	}
	if w := got[1]; w.How != StdinPoll || w.Class != "build" || w.Outcome != None || w.End.Sub(w.Start) != 30*time.Second {
		t.Errorf("правка посреди опросов: %+v", w)
	}
}

func TestSendAndWakeup(t *testing.T) {
	s := newEvents()
	s.tool("SendMessage", "→ main: готово", map[string]string{"to": "uds:/run/user/1000/cc-socks/1.sock"}, 0)
	s.tool("SendMessage", "→ reviewer: вопрос", map[string]string{"to": "reviewer"}, 1)
	w := s.tool("ScheduleWakeup", "1200s", map[string]int{"delaySeconds": 1200}, 2)
	s.out(w, "scheduled", 2)
	s.add(transcript.Event{Role: transcript.Peer, Who: "reviewer", From: "uds:/run/user/1000/cc-socks/2.sock", Text: "ответ"}, 60)
	s.add(transcript.Event{Role: transcript.Peer, Who: "main", From: "uds:/run/user/1000/cc-socks/1.sock", Text: "ок"}, 90)
	got := s.run()
	if len(got) != 3 {
		t.Fatalf("письма и будильник: %+v", got)
	}
	// Письмо reviewer будит будильник раньше, чем пришёл ответ main.
	if got[0].How != Wakeup || got[0].End.Sub(got[0].Start) != 58*time.Second {
		t.Errorf("будильник: %+v", got[0])
	}
	if got[1].How != Send || got[1].Class != "agent" || got[1].End.Sub(got[1].Start) != 59*time.Second {
		t.Errorf("письмо по имени: %+v", got[1])
	}
	if got[2].How != Send || got[2].End.Sub(got[2].Start) != 90*time.Second {
		t.Errorf("письмо по адресу: %+v", got[2])
	}
}

// Open — строка самого раннего незаконченного; после конца — нет; Close отдаёт незаконченные с исходом none.
func TestOpenClose(t *testing.T) {
	s := newEvents()
	s.tool("SendMessage", "→ main", map[string]string{"to": "main"}, 0)
	a := s.bash(`timeout 270 bash -c 'until [ -f /t/x ]; do sleep 5; done'`, 1, "r1", false)
	r := New()
	for _, e := range s.evs {
		r.Add(e)
	}
	if line, ok := r.Open(); !ok || line != 1 {
		t.Errorf("Open: %d %v", line, ok)
	}
	r.Add(transcript.Event{Role: transcript.Out, CallID: a, Line: 3})
	r.Add(transcript.Event{Role: transcript.Peer, Who: "main", Line: 4})
	if line, ok := r.Open(); !ok || line != 2 {
		t.Errorf("цикл без следующего вызова ещё открыт (может прийти повтор): %d %v", line, ok)
	}
	got := r.Close()
	if len(got) != 1 || got[0].How != Loop || got[0].Outcome != Done {
		t.Errorf("Close: %+v", got)
	}
	if _, ok := r.Open(); ok {
		t.Error("после Close открытых нет")
	}
	r.Add(transcript.Event{Role: transcript.Cmd, Who: "Monitor", CallID: "m1", Line: 9})
	if got := r.Close(); len(got) != 1 || got[0].Outcome != None || !got[0].End.IsZero() {
		t.Errorf("без конца — none: %+v", got)
	}
}

func TestClass(t *testing.T) {
	r := New()
	for cmd, want := range map[string]string{
		"go -C tool test ./cmd/farm/...":                      "test",
		"gofmt -w a.go && go -C tool test ./cmd/farm/...":     "test",
		"go -C tool vet ./...":                                "test",
		"/home/user/bin/glint check ./cmd/... ./internal/...": "test",
		"bash /x/fighters/cleanup-dead/accept-luna1.sh":       "test",
		"go -C tool build -o /tmp/farm ./cmd/farm":            "build",
		"graft exec --heavy -- go -C tool test ./...":         "graft",
		"go run ./cmd/testdata-gen":                           "other",
	} {
		if got := r.class(StdinPoll, cmd); got != want {
			t.Errorf("%q: %s, ждали %s", cmd, got, want)
		}
	}
}

// wfor бойца codex дольше 30 с exec_command: его сессию опрашивают пустым write_stdin - одно ожидание wfor.
func TestCodexWforPolled(t *testing.T) {
	s := newEvents()
	a := s.tool("exec", "wfor exit 4242 -t 10m", "x", 0)
	s.out(a, `{"chunk_id":"a","wall_time_seconds":30.0,"session_id":5,"output":""}`, 30)
	poll := `const r = await tools.write_stdin({session_id:5,chars:"",yield_time_ms:30000}); text(JSON.stringify(r))`
	b := s.tool("exec", poll, "x", 31)
	s.out(b, `{"chunk_id":"b","wall_time_seconds":30.0,"session_id":5,"output":""}`, 61)
	c := s.tool("exec", poll, "x", 62)
	s.out(c, `{"chunk_id":"c","wall_time_seconds":9.0,"exit_code":0,"output":"wfor: done in 1m8s: exit 4242"}`, 71)
	got := s.run()
	if len(got) != 1 || got[0].How != Wfor || got[0].Calls != 3 || got[0].Outcome != Done || got[0].End.Sub(got[0].Start) != 71*time.Second {
		t.Errorf("одно ожидание wfor на три вызова: %+v", got)
	}
}

// Следом за дождавшимся wfor - цикл и снова wfor: три ожидания, не повтор.
func TestRepeatSameWay(t *testing.T) {
	s := newEvents()
	a := s.bash("wfor exit 12", 0, "", false)
	s.out(a, "wfor: done in 1s: exit 12", 1)
	b := s.bash("until [ -f done ]; do sleep 5; done", 2, "", false)
	s.out(b, "", 30)
	c := s.bash("wfor exit 13", 31, "", false)
	s.out(c, "wfor: done in 1s: exit 13", 32)
	d := s.bash("wfor exit 14", 33, "", false)
	s.out(d, "wfor: done in 1s: exit 14", 34)
	got := s.run()
	var hows []string
	for _, w := range got {
		hows = append(hows, w.How)
	}
	if !slices.Equal(hows, []string{Wfor, Loop, Wfor, Wfor}) {
		t.Errorf("ожидания: %q", hows)
	}
}

func TestCommandHow(t *testing.T) {
	cases := map[string]string{
		"wfor file /tmp/x --timeout 10m":                Wfor,
		"until test -f x; do sleep 10; done":            Loop,
		"~/bin/wait-deploy.sh prod":                     Script,
		"worker wait 1023-1636-move-late --timeout 60m": Script,
		"cd ~/git/x && ~/bin/worker wait abc":           Script,
		"timeout 3600 worker wait abc":                  Script,
		"sleep 30":                                      Sleep,
		"sleep 2":                                       "",
		"go test ./...":                                 "",
		"worker run task.md":                            "",
		`echo "worker wait later"`:                      "",
		"cat > f.md <<'X'\nworker wait abc\nwfor file y\nsleep 60\nX\n": "",
	}
	for cmd, want := range cases {
		if got := CommandHow(cmd); got != want {
			t.Errorf("CommandHow(%q) = %q, want %q", cmd, got, want)
		}
	}
}
