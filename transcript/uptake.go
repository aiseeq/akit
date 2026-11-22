package transcript

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

// ReadUptakeOnePass reads events, current turn models, and completed commands for uptake in one file pass.
func ReadUptakeOnePass(path string, fn func(Event), commandFn func(CodexCommandResult), modelFn func(string, int)) (Meta, error) {
	f, r, rd, codex, err := openUptake(path)
	if err != nil {
		return Meta{}, err
	}
	defer f.Close()
	events := queueUptakeEvents(rd, fn, codex)
	model := ""
	for {
		raw, readErr := readLine(r)
		if len(raw) > 0 {
			model = uptakeLine(raw, rd, codex, model, commandFn, modelFn)
		} else if readErr == nil {
			rd.lineNo++
			if modelFn != nil {
				modelFn(model, rd.lineNo)
			}
		}
		if readErr != nil {
			rd.meta.Originator, rd.meta.Account = rd.codex.originator, rd.codex.firstAccount
			if errors.Is(readErr, io.EOF) {
				finishUptakeEvents(events, fn, rd, codex)
				return rd.meta, nil
			}
			return rd.meta, fmt.Errorf("transcript %s: %w", path, readErr)
		}
	}
}

func openUptake(path string) (*os.File, *bufio.Reader, *reader, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, false, fmt.Errorf("transcript: %w", err)
	}
	r := bufio.NewReaderSize(f, 1<<20)
	head, err := r.Peek(codexHead)
	if err != nil && !errors.Is(err, io.EOF) {
		f.Close()
		return nil, nil, nil, false, fmt.Errorf("transcript %s: %w", path, err)
	}
	codex := bytes.Contains(head, []byte(`"type":"session_meta"`))
	rd := reader{claude: newClaudeParser(), codex: newCodexParser(), commandFn: func(CodexCommandResult) {}}
	rd.codex.deferMessages = codex
	return f, r, &rd, codex, nil
}

func queueUptakeEvents(rd *reader, fn func(Event), codex bool) *[]Event {
	if fn == nil {
		rd.fn = func(Event) {}
		return nil
	}
	if !codex {
		rd.fn = fn
		return nil
	}
	events := make([]Event, 0)
	queue := &events
	rd.fn = func(event Event) {
		if !rd.codex.queueFallback(event) {
			*queue = append(*queue, event)
		}
	}
	return queue
}

func uptakeLine(raw []byte, rd *reader, codex bool, model string, commandFn func(CodexCommandResult), modelFn func(string, int)) string {
	if codex {
		noteCodexMessages(raw, rd.codex)
		rd.codex.currentLine = rd.lineNo + 1
	}
	if commandFn != nil {
		if result, ok := parseCodexCommandResult(raw, rd.lineNo+1); ok {
			commandFn(result)
		}
	}
	modelUpdates := rd.codex.modelUpdates
	rd.line(raw)
	if rd.codex.modelUpdates != modelUpdates {
		model = rd.codex.model
	}
	if modelFn != nil {
		modelFn(model, rd.lineNo)
	}
	return model
}

func noteCodexMessages(raw []byte, codex *codexParser) {
	codex.eventUser, codex.eventAgent = noteCodexMessageMarkers(raw, codex.eventUser, codex.eventAgent)
}

func finishUptakeEvents(queue *[]Event, fn func(Event), rd *reader, codex bool) {
	if fn == nil {
		return
	}
	if queue == nil {
		return
	}
	if codex {
		*queue = mergeUptakeEvents(*queue, rd.codex.finishUptake())
	}
	for i := range *queue {
		fn((*queue)[i])
	}
}

func mergeUptakeEvents(events, deferred []Event) []Event {
	merged := make([]Event, 0, len(events)+len(deferred))
	i, j := 0, 0
	for i < len(events) && j < len(deferred) {
		if events[i].Line <= deferred[j].Line {
			merged = append(merged, events[i])
			i++
		} else {
			merged = append(merged, deferred[j])
			j++
		}
	}
	merged = append(merged, events[i:]...)
	return append(merged, deferred[j:]...)
}
