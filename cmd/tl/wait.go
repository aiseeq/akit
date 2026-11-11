package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/akit/transcript"
	transcriptwaits "github.com/aiseeq/akit/transcript/waits"
)

var waitWrappers = map[string]bool{
	"head": true, "tail": true, "cut": true, "grep": true, "sed": true, "jq": true,
	"wc": true, "sort": true, "uniq": true, "cd": true, "echo": true, "true": true,
	"cat": true, "printf": true, "tr": true, "column": true, "date": true,
}

type waitCall struct {
	command   string
	at        time.Time
	line      int // порядок вызова в файле (seq трекера)
	message   string
	sidechain bool
	bg        bool
	done      bool
	result    time.Time
	refused   bool
	count     int
	calls     []cachedShellCall
	invalid   bool
	parsed    bool
	poll      bool
	codex     bool // вызов codex: только его вывод называет запущенную сессию
	session   string
}

// waitTracker сводит вызов команды с его выводом по ходу чтения одного файла: в памяти только вызовы, ждущие
// вывода, и найденные ожидания.
type waitTracker struct {
	prog, sub string
	calls     map[string]*waitCall   // CallID → вызов
	messages  map[string][]*waitCall // вызовы текущего сообщения: предшественники для начала соседей
	current   string                 // id текущего сообщения
	seq       int                    // порядок вызовов: номер строки у нескольких вызовов одной строки совпал бы
	waits     []time.Duration
	sessions  map[string]*waitCall
}

func newWaitTracker(opts options) *waitTracker {
	return &waitTracker{prog: opts.waitProg, sub: opts.waitSub, calls: map[string]*waitCall{}, messages: map[string][]*waitCall{}, sessions: map[string]*waitCall{}}
}

func (t *waitTracker) event(ev transcript.Event) {
	command, bg := "", false
	if ev.Role == transcript.Cmd {
		command, bg = waitCommand(ev)
	}
	t.eventCall(cachedWaitEvent{
		Role: ev.Role, CallID: ev.CallID, Text: ev.Text, Time: ev.Time, Line: ev.Line, MessageID: ev.MessageID,
		Who: ev.Who, Sidechain: ev.Sidechain, Error: ev.Error, Raw: ev.Raw,
		Command: command, BG: bg,
	})
}

func (t *waitTracker) eventCall(event cachedWaitEvent) {
	if event.Role == transcript.Cmd {
		t.trackCall(event)
	} else if event.Role == transcript.Out {
		t.trackResult(event)
	}
}

func (t *waitTracker) trackCall(event cachedWaitEvent) {
	if session, ok := waitPollSession(event); ok {
		call := &waitCall{poll: true, session: session, at: event.Time}
		t.calls[event.CallID] = call
		return
	}
	if event.MessageID != "" && !event.Sidechain && event.MessageID != t.current {
		// Следующее сообщение модель пишет, получив все результаты прошлого: его вызовы больше ничьё начало не
		// сдвинут, и в памяти остаётся одно сообщение.
		clear(t.messages)
		t.current = event.MessageID
	}
	if event.Command == "" && (event.MessageID == "" || event.Sidechain || !isSerialTool(event.Who)) {
		return
	}
	call := t.calls[event.CallID]
	if call == nil {
		call = &waitCall{}
	}
	if call.count == 0 {
		t.seq++
		call.command, call.at, call.line = event.Command, event.Time, t.seq
		call.message, call.sidechain = event.MessageID, event.Sidechain
		call.codex = transcriptwaits.CodexShell(event.Who)
	}
	call.count++
	call.bg = call.bg || event.BG
	if call.count == 1 && event.MessageID != "" && !event.Sidechain && isSerialTool(event.Who) {
		t.messages[event.MessageID] = append(t.messages[event.MessageID], call)
	}
	t.calls[event.CallID] = call
}

func waitPollSession(event cachedWaitEvent) (string, bool) {
	if event.Who != "write_stdin" && !(event.Who == "exec" && strings.Contains(event.Command, "write_stdin(") && !strings.Contains(event.Command, "exec_command(")) {
		return "", false
	}
	session := transcriptwaits.SessionOf(transcript.Event{Text: event.Command, Raw: event.Raw})
	return session, session != ""
}

func (t *waitTracker) trackResult(event cachedWaitEvent) {
	call := t.calls[event.CallID]
	if call == nil || call.done {
		return
	}
	if call.poll {
		delete(t.calls, event.CallID)
		if _, running := transcriptwaits.Running(event.Text); running {
			return
		}
		wait := t.sessions[call.session]
		delete(t.sessions, call.session)
		if wait != nil {
			t.addWait(wait, event.Time)
		}
		return
	}
	call.done, call.result = true, event.Time
	call.refused = event.Error && permissionRefusal(event.Raw)
	delete(t.calls, event.CallID)
	if session, running := transcriptwaits.Running(event.Text); running && call.codex {
		// Сессию с тем же номером получила новая команда: брошенное ожидание прежней не кончится её опросом.
		delete(t.sessions, session)
		if call.command != "" && call.count == 1 && !call.bg && !call.refused && call.matches(t.prog, t.sub) {
			call.session = session
			t.sessions[session] = call
		}
		return
	}
	if call.command == "" || call.count != 1 || call.bg || call.refused || !call.matches(t.prog, t.sub) {
		return
	}
	t.addWait(call, event.Time)
}

func (t *waitTracker) addWait(call *waitCall, end time.Time) {
	start := call.start(t.messages[call.message], end)
	if duration := end.Sub(start); duration >= 0 {
		t.waits = append(t.waits, duration)
	}
}

func (call *waitCall) matches(prog, sub string) bool {
	if call.parsed {
		return matchedWaitCallData(call.calls, call.invalid, prog, sub)
	}
	return matchedWaitCommand(call.command, prog, sub)
}

func (call *waitCall) start(priorCalls []*waitCall, result time.Time) time.Time {
	start := call.at
	if call.message == "" || call.sidechain {
		return start
	}
	for _, prior := range priorCalls {
		if prior.line < call.line && prior.done && !prior.refused && !prior.result.After(result) && prior.result.After(start) {
			start = prior.result
		}
	}
	return start
}

func (t *waitTracker) cachedEvent(event cachedWaitEvent) {
	call := t.calls[event.CallID]
	first := event.Role == transcript.Cmd && (call == nil || call.count == 0)
	t.eventCall(event)
	if call := t.calls[event.CallID]; first && call != nil && event.Command != "" {
		call.calls, call.invalid, call.parsed = event.Calls, event.Invalid, true
	}
}

var parallelTools = map[string]bool{
	"Read": true, "Grep": true, "Glob": true, "WebFetch": true, "WebSearch": true, "Agent": true,
	"Task": true, "TaskCreate": true, "TaskGet": true, "TaskList": true, "TaskOutput": true, "TaskStop": true,
}

func isSerialTool(tool string) bool {
	return !parallelTools[tool] && !strings.HasPrefix(tool, "Task")
}

func permissionRefusal(raw []byte) bool {
	text := strings.ToLower(string(raw))
	for _, phrase := range []string{
		"doesn't want to proceed", "does not want to proceed", "rejected by user", "user rejected permission",
		"permission denied by user", "request interrupted by user", "blocked by a hook", "blocked by hook",
		"hook denied", "denied by permission hook", "denied by a permission hook",
		"pretooluse:bash hook error",
	} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// minWaitEvents — сколько событий нужно перцентилю: хвост (q выше 50) по меньшему числу событий не перцентиль,
// а медиане хватает трёх.
func minWaitEvents(q int) int {
	if q <= 50 {
		return 3
	}
	return 20
}

func reportWait(res result, opts options, stdout, stderr io.Writer) int {
	waits := res.waits
	if need := minWaitEvents(opts.q); len(waits) < need {
		fmt.Fprintf(stderr, "tl: %d wait event(s); --q %d needs at least %d\n", len(waits), opts.q, need)
		return 3
	}
	fmt.Fprintf(stderr, "tl: %d wait event(s)\n", len(waits))
	slices.Sort(waits)
	index := int(math.Ceil(float64(opts.q)*float64(len(waits))/100)) - 1
	fmt.Fprintln(stdout, waits[index].Milliseconds())
	return 0
}

func waitCommand(ev transcript.Event) (string, bool) {
	if ev.Who == "Bash" {
		var input struct {
			Command         string `json:"command"`
			RunInBackground bool   `json:"run_in_background"`
		}
		if json.Unmarshal(ev.Raw, &input) == nil {
			if input.Command != "" {
				return input.Command, input.RunInBackground
			}
			return ev.Text, input.RunInBackground
		}
	}
	if ev.Who != "exec" && ev.Who != "exec_command" && ev.Who != "shell" {
		return "", false
	}
	command := ev.Text
	if command == "" {
		var input struct {
			Cmd     string `json:"cmd"`
			Command string `json:"command"`
		}
		if json.Unmarshal(ev.Raw, &input) == nil {
			command = input.Cmd
			if command == "" {
				command = input.Command
			}
		}
	}
	return command, false
}

func matchedWaitCommand(command, prog, sub string) bool {
	calls, invalid := parseWaitCalls(command)
	return matchedWaitCallData(calls, invalid, prog, sub)
}

func parseWaitCalls(command string) ([]cachedShellCall, bool) {
	found, err := shellcmd.Calls(command)
	var nested *shellcmd.NestedError
	invalid := err != nil && !errors.As(err, &nested)
	calls := make([]cachedShellCall, 0, len(found))
	for _, call := range found {
		c := cachedShellCall{Name: call.Name, Background: call.Background}
		if len(call.Args) > 0 {
			c.Arg0, c.HasArg = call.Args[0], true
		}
		calls = append(calls, c)
	}
	return calls, invalid
}

func matchedWaitCallData(found []cachedShellCall, invalid bool, prog, sub string) bool {
	if invalid {
		return false
	}
	programs := 0
	for _, call := range found {
		if call.Name == prog && (sub == "" || call.HasArg && call.Arg0 == sub) && !call.Background {
			programs++
			continue
		}
		if !waitWrappers[call.Name] {
			return false
		}
	}
	return programs == 1
}
