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
)

var waitWrappers = map[string]bool{
	"head": true, "tail": true, "cut": true, "grep": true, "sed": true, "jq": true,
	"wc": true, "sort": true, "uniq": true, "cd": true, "echo": true, "true": true,
	"cat": true, "printf": true, "tr": true, "column": true, "date": true,
}

type waitCall struct {
	command   string
	at        time.Time
	line      int
	message   string
	tool      string
	sidechain bool
	bg        bool
	done      bool
	result    time.Time
	refused   bool
	count     int
	calls     []cachedShellCall
	invalid   bool
	parsed    bool
}

// waitTracker сводит вызов команды с его выводом по ходу чтения одного файла: в памяти только вызовы, ждущие
// вывода, и найденные ожидания.
type waitTracker struct {
	prog, sub string
	calls     map[string]*waitCall // CallID → вызов
	messages  map[string][]*waitCall
	waits     []time.Duration
}

func newWaitTracker(opts options) *waitTracker {
	return &waitTracker{prog: opts.waitProg, sub: opts.waitSub, calls: map[string]*waitCall{}, messages: map[string][]*waitCall{}}
}

func (t *waitTracker) event(ev transcript.Event) {
	command, bg := "", false
	if ev.Role == transcript.Cmd {
		command, bg = waitCommand(ev)
	}
	t.eventCall(cachedWaitEvent{
		Role: ev.Role, CallID: ev.CallID, Time: ev.Time, Line: ev.Line, MessageID: ev.MessageID,
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
	if event.Command == "" {
		return
	}
	call := t.calls[event.CallID]
	if call == nil {
		call = &waitCall{}
	}
	if call.count == 0 {
		call.command, call.at, call.line = event.Command, event.Time, event.Line
		call.message, call.tool, call.sidechain = event.MessageID, event.Who, event.Sidechain
	}
	call.count++
	call.bg = call.bg || event.BG
	if call.count == 1 && event.MessageID != "" && !event.Sidechain && isSerialTool(event.Who) {
		t.messages[event.MessageID] = append(t.messages[event.MessageID], call)
	}
	t.calls[event.CallID] = call
}

func (t *waitTracker) trackResult(event cachedWaitEvent) {
	call := t.calls[event.CallID]
	if call == nil || call.done {
		return
	}
	call.done, call.result = true, event.Time
	call.refused = event.Error && permissionRefusal(event.Raw)
	delete(t.calls, event.CallID)
	defer t.forgetMessage(call.message)
	if call.count != 1 || call.bg || call.refused || !call.matches(t.prog, t.sub) {
		return
	}
	start := call.start(t.messages[call.message], event.Time)
	if duration := event.Time.Sub(start); duration >= 0 {
		t.waits = append(t.waits, duration)
	}
}

// forgetMessage убирает сообщение, все вызовы которого уже получили результат: его вызовы больше ничьё начало не
// сдвинут, а держать их в памяти на чтении всех транскриптов — расти без предела. Вызов того же сообщения, записанный
// позже результата предшественника, начинается после него и без записи.
func (t *waitTracker) forgetMessage(id string) {
	calls, ok := t.messages[id]
	if !ok {
		return
	}
	for _, c := range calls {
		if !c.done {
			return
		}
	}
	delete(t.messages, id)
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
	if first && event.Role == transcript.Cmd {
		call := t.calls[event.CallID]
		call.calls, call.invalid, call.parsed = event.Calls, event.Invalid, true
		t.calls[event.CallID] = call
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
