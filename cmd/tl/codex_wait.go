package main

import (
	"strings"
	"time"

	"github.com/aiseeq/akit/transcript"
)

type codexWaitResult struct {
	command string
	end     time.Time
}

type codexWaitCall struct {
	line int
	call string
	text string
	at   time.Time
	out  time.Time
}

type codexWaitMatcher struct {
	byEvent map[codexWaitEventKey][]codexWaitResult
	next    map[codexWaitEventKey]int
	byCall  map[string]time.Time
}

type codexWaitEventKey struct {
	line int
	call string
	text string
}

func codexCallsFromCache(events []cachedWaitEvent) []codexWaitCall {
	outputs := make(map[string]time.Time)
	for i := range events {
		if events[i].Role == transcript.Out {
			outputs[events[i].CallID] = events[i].Time
		}
	}
	calls := make([]codexWaitCall, 0, len(events))
	for i := range events {
		event := &events[i]
		if event.Role == transcript.Cmd {
			calls = append(calls, codexWaitCall{line: event.Line, call: event.CallID, text: event.Command, at: event.Time, out: outputs[event.CallID]})
		}
	}
	return calls
}

func matchCodexWaitResults(events []codexWaitCall, results []transcript.CodexCommandResult) codexWaitMatcher {
	matcher := codexWaitMatcher{byEvent: map[codexWaitEventKey][]codexWaitResult{}, next: map[codexWaitEventKey]int{}, byCall: map[string]time.Time{}}
	normalized := make([]string, len(events))
	for i := range events {
		normalized[i] = normalizeCodexCommand(events[i].text)
	}
	pending := make(map[string][]int)
	positions := make([]int, len(events))
	nextCall := 0
	for _, result := range results {
		for nextCall < len(events) && events[nextCall].line <= result.Line {
			key := codexCommandWord(normalized[nextCall])
			positions[nextCall] = len(pending[key])
			pending[key] = append(pending[key], nextCall)
			nextCall++
		}
		command := normalizeCodexCommand(result.Command)
		key := codexCommandWord(command)
		candidate := -1
		for _, index := range pending[key] {
			call := events[index]
			if call.at.IsZero() || call.out.IsZero() || result.Started.Before(call.at.Add(-time.Second)) || result.Started.After(call.out.Add(time.Second)) || !strings.HasPrefix(command, normalized[index]) {
				continue
			}
			if candidate < 0 || call.line > events[candidate].line || call.line == events[candidate].line && index > candidate {
				candidate = index
			}
		}
		if candidate >= 0 {
			key := codexCommandWord(normalized[candidate])
			list := pending[key]
			position, last := positions[candidate], len(list)-1
			list[position] = list[last]
			positions[list[position]] = position
			pending[key] = list[:last]
			matcher.record(events[candidate], result)
		}
	}
	matcher.indexCalls(events)
	return matcher
}

func codexCommandWord(command string) string {
	if i := strings.IndexAny(command, " \t\r\n"); i >= 0 {
		return command[:i]
	}
	return command
}

func (m *codexWaitMatcher) record(event codexWaitCall, result transcript.CodexCommandResult) {
	key := codexWaitEventKey{line: event.line, call: event.call, text: event.text}
	m.byEvent[key] = append(m.byEvent[key], codexWaitResult{command: result.Command, end: result.Completed})
}

func (m *codexWaitMatcher) indexCalls(events []codexWaitCall) {
	counts := map[string]int{}
	for i := range events {
		event := &events[i]
		key := codexWaitEventKey{line: event.line, call: event.call, text: event.text}
		if results := m.byEvent[key]; len(results) > 0 && event.call != "" {
			counts[event.call] += len(results)
			m.byCall[event.call] = results[len(results)-1].end
		}
	}
	for callID, count := range counts {
		if count != 1 {
			delete(m.byCall, callID)
		}
	}
}

func normalizeCodexCommand(command string) string {
	return strings.TrimSpace(strings.ReplaceAll(command, `\n`, "\n"))
}

func (m *codexWaitMatcher) take(key codexWaitEventKey) (codexWaitResult, bool) {
	index := m.next[key]
	results := m.byEvent[key]
	if index >= len(results) {
		return codexWaitResult{}, false
	}
	m.next[key] = index + 1
	return results[index], true
}

func (m *codexWaitMatcher) applyCached(event *cachedWaitEvent) {
	if event.Role == transcript.Cmd {
		key := codexWaitEventKey{line: event.Line, call: event.CallID, text: event.Command}
		if result, ok := m.take(key); ok {
			event.Calls, event.Invalid = parseWaitCalls(result.command)
		}
	} else if event.Role == transcript.Out {
		if end := m.byCall[event.CallID]; !end.IsZero() {
			event.Time = end
		}
	}
}
