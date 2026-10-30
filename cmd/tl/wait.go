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
	switch ev.Role {
	case transcript.Cmd:
		command, bg := waitCommand(ev)
		if command != "" {
			call := t.calls[ev.CallID]
			if call.count == 0 {
				call.command, call.at = command, ev.Time
			}
			call.count++
			call.bg = call.bg || bg
			t.calls[ev.CallID] = call
		}
	case transcript.Out:
		call, ok := t.calls[ev.CallID]
		if !ok || call.count != 1 || call.bg {
			return
		}
		if matchedWaitCommand(call.command, t.prog, t.sub) {
			if d := ev.Time.Sub(call.at); d >= 0 {
				t.waits = append(t.waits, d)
			}
		}
		delete(t.calls, ev.CallID)
	}
}

func reportWait(res result, opts options, stdout, stderr io.Writer) int {
	waits := res.waits
	if len(waits) < 20 {
		fmt.Fprintf(stderr, "tl: %d wait event(s); need at least 20\n", len(waits))
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
	found, err := shellcmd.Calls(command)
	var nested *shellcmd.NestedError
	if err != nil && !errors.As(err, &nested) {
		return false
	}
	programs := 0
	for _, call := range found {
		if call.Name == prog && (sub == "" || len(call.Args) > 0 && call.Args[0] == sub) && !call.Background {
			programs++
			continue
		}
		if !waitWrappers[call.Name] {
			return false
		}
	}
	return programs == 1
}
