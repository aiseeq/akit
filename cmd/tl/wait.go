package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
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
	command string
	at      time.Time
	bg      bool
	count   int
	calls   []cachedShellCall
	invalid bool
	parsed  bool
}

// waitTracker сводит вызов команды с его выводом по ходу чтения одного файла: в памяти только вызовы, ждущие
// вывода, и найденные ожидания.
type waitTracker struct {
	prog, sub string
	calls     map[string]waitCall // CallID → вызов
	waits     []time.Duration
}

func newWaitTracker(opts options) *waitTracker {
	return &waitTracker{prog: opts.waitProg, sub: opts.waitSub, calls: map[string]waitCall{}}
}

func (t *waitTracker) event(ev transcript.Event) {
	command, bg := "", false
	if ev.Role == transcript.Cmd {
		command, bg = waitCommand(ev)
	}
	t.eventCall(ev.Role, ev.CallID, ev.Time, command, bg)
}

func (t *waitTracker) eventCall(role transcript.Role, callID string, at time.Time, command string, bg bool) {
	switch role {
	case transcript.Cmd:
		if command != "" {
			call := t.calls[callID]
			if call.count == 0 {
				call.command, call.at = command, at
			}
			call.count++
			call.bg = call.bg || bg
			t.calls[callID] = call
		}
	case transcript.Out:
		call, ok := t.calls[callID]
		if !ok || call.count != 1 || call.bg {
			return
		}
		matched := false
		if call.parsed {
			matched = matchedWaitCallData(call.calls, call.invalid, t.prog, t.sub)
		} else {
			matched = matchedWaitCommand(call.command, t.prog, t.sub)
		}
		if matched {
			if d := at.Sub(call.at); d >= 0 {
				t.waits = append(t.waits, d)
			}
		}
		delete(t.calls, callID)
	}
}

func (t *waitTracker) cachedEvent(event cachedWaitEvent) {
	first := event.Role == transcript.Cmd && t.calls[event.CallID].count == 0
	t.eventCall(event.Role, event.CallID, event.Time, event.Command, event.BG)
	if first && event.Role == transcript.Cmd {
		call := t.calls[event.CallID]
		call.calls, call.invalid, call.parsed = event.Calls, event.Invalid, true
		t.calls[event.CallID] = call
	}
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
