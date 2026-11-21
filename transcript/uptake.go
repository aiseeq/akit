package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

type uptakeRecord struct {
	Type      string         `json:"type"`
	Timestamp string         `json:"timestamp"`
	Payload   *uptakePayload `json:"payload"`
}

type uptakePayload struct {
	payload
	Item *uptakeCommandItem `json:"item"`
}

type uptakeCommandItem struct {
	codexCommandItem
	AggregatedOutput json.RawMessage `json:"aggregated_output"`
}

var (
	uptakeCommandItemMarker        = []byte(`"CommandExecution"`)
	uptakeEscapedCommandItemMarker = []byte(`"CommandExecut\u0069on"`)
)

// ReadUptakeOnePass reads events, current turn models, and completed commands for uptake in one file pass.
func ReadUptakeOnePass(path string, fn func(Event), commandFn func(CodexCommandResult), modelFn func(string, int)) (Meta, error) {
	f, err := os.Open(path)
	if err != nil {
		return Meta{}, fmt.Errorf("transcript: %w", err)
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	rd := reader{claude: newClaudeParser(), codex: newCodexParser(), fn: fn}
	head, err := r.Peek(codexHead)
	if err != nil && !errors.Is(err, io.EOF) {
		return Meta{}, fmt.Errorf("transcript %s: %w", path, err)
	}
	if bytes.Contains(head, []byte(`"type":"session_meta"`)) {
		rd.codex.eventUser, rd.codex.eventAgent, err = codexMessages(f)
		if err != nil {
			return Meta{}, fmt.Errorf("transcript %s: %w", path, err)
		}
	}
	model := ""
	for {
		raw, readErr := readLine(r)
		if len(raw) > 0 {
			rd.lineNo++
			model = uptakeLine(raw, &rd, model, commandFn, modelFn)
		} else if readErr == nil {
			rd.lineNo++
			if modelFn != nil {
				modelFn(model, rd.lineNo)
			}
		}
		if readErr != nil {
			rd.meta.Originator, rd.meta.Account = rd.codex.originator, rd.codex.firstAccount
			if errors.Is(readErr, io.EOF) {
				return rd.meta, nil
			}
			return rd.meta, fmt.Errorf("transcript %s: %w", path, readErr)
		}
	}
}

func uptakeLine(raw []byte, rd *reader, model string, commandFn func(CodexCommandResult), modelFn func(string, int)) string {
	if name, ok := agentName(raw); ok {
		rd.meta.Name = name
		if modelFn != nil {
			modelFn(model, rd.lineNo)
		}
		return model
	}
	if skipLine(raw) {
		if modelFn != nil {
			modelFn(model, rd.lineNo)
		}
		return model
	}

	commandCandidate := commandFn != nil && (bytes.Contains(raw, uptakeCommandItemMarker) || bytes.Contains(raw, uptakeEscapedCommandItemMarker))
	rec, parsed, err := decodeUptakeRecord(raw, commandCandidate)
	if err != nil { //nolint:silent-error-handling // Preserve a turn_context model; rd.badLine below reports the malformed row.
		if next, ok := modelFromRawLine(raw); ok {
			model = next
		}
	} else if rec.Type == "turn_context" && rec.Payload != nil && rec.Payload.Model != "" {
		model = rec.Payload.Model
	}
	if modelFn != nil {
		modelFn(model, rd.lineNo)
	}
	if commandCandidate && parsed.Payload != nil && uptakeCommandResultAllowed(parsed.Payload, err) {
		if result, ok := uptakeCommandResult(parsed.Payload, rd.lineNo); ok {
			commandFn(result)
		}
	}
	if err != nil {
		rd.badLine(raw, false, err)
		return model
	}

	events, codex, parseErr := parseRecordKind(&rec, raw, rd.claude, rd.codex)
	if parseErr != nil {
		rd.badLine(raw, codex, parseErr)
		return model
	}
	rd.emitParsed(raw, codex, events)
	return model
}

func decodeUptakeRecord(raw []byte, commandCandidate bool) (record, uptakeRecord, error) {
	if !commandCandidate {
		var rec record
		err := json.Unmarshal(raw, &rec)
		return rec, uptakeRecord{}, err
	}
	var parsed uptakeRecord
	err := json.Unmarshal(raw, &parsed)
	if parsed.Payload == nil {
		return record{}, parsed, err
	}
	p := parsed.Payload.payload
	if parsed.Payload.Item != nil {
		item := parsed.Payload.Item.codexCommandItem
		p.Item = &item
	}
	return record{Type: parsed.Type, Timestamp: parsed.Timestamp, Payload: &p}, parsed, err
}

func uptakeCommandResultAllowed(pl *uptakePayload, decodeErr error) bool {
	if pl == nil || pl.Type != "item_completed" || pl.Item == nil {
		return false
	}
	if decodeErr == nil {
		return true
	}
	var typeErr *json.UnmarshalTypeError
	if !errors.As(decodeErr, &typeErr) {
		return false
	}
	field := strings.ToLower(typeErr.Field)
	switch field {
	case "timestamp":
		return true
	case "type", "payload.type", "payload.started_at_ms", "payload.completed_at_ms":
		return false
	}
	return !strings.HasPrefix(field, "payload.item")
}

func uptakeCommandResult(pl *uptakePayload, line int) (CodexCommandResult, bool) {
	it := pl.Item
	output := ""
	if len(it.AggregatedOutput) > 0 && json.Unmarshal(it.AggregatedOutput, &output) != nil {
		return CodexCommandResult{}, false
	}
	return commandResult(line, pl.StartedAtMS, pl.CompletedAtMS, it.codexCommandItem, output)
}

func modelFromRawLine(raw []byte) (string, bool) {
	var v struct {
		Type    string `json:"type"`
		Payload struct {
			Type  string `json:"type"`
			Model string `json:"model"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &v) == nil && v.Type == "turn_context" && v.Payload.Model != "" {
		return v.Payload.Model, true
	}
	return "", false
}
