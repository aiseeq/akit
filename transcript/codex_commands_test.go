package transcript

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCodexCommandResultsRequireCompletedShellItem(t *testing.T) {
	base := `"payload":{"type":"item_completed","started_at_ms":1000,"completed_at_ms":2000,"item":{"type":"CommandExecution","command":["/bin/bash","-lc","echo ok"],"exit_code":0}}`
	rows := []string{
		`{"type":"response_item",` + base + `}`,
		`{"type":"event_msg","payload":{"type":"item_completed","started_at_ms":"wrong","completed_at_ms":2000,"item":{"type":"CommandExecution","command":["sh","-lc","echo malformed"],"exit_code":0},"note":"CommandExecution"}}`,
		`{"type":"event_msg","payload":{"type":"other","item":{"type":"CommandExecution","command":["sh"],"exit_code":0}}}`,
		`{"type":"event_msg","payload":{"type":"item_started","item":{"type":"CommandExecution","command":["sh"],"exit_code":0}}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","note":"CommandExecution"}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"Other","command":["sh"],"exit_code":0},"note":"CommandExecution"}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":[],"exit_code":0}}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":["sh"]}}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":["sh"],"exit_code":-1}}}`,
		`{"type":"event_msg",` + base + `}`,
	}
	path := filepath.Join(t.TempDir(), "rollout-synthetic.jsonl")
	if err := os.WriteFile(path, []byte("\n"+joinLines(rows)), 0600); err != nil {
		t.Fatal(err)
	}
	withOutput, err := CodexCommandResults(path)
	if err != nil {
		t.Fatal(err)
	}
	var discarded []CodexCommandResult
	if _, err := ReadWithCodexCommandsDiscardOutput(path, nil, func(r CodexCommandResult) { discarded = append(discarded, r) }); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string][]CodexCommandResult{"with output": withOutput, "discarding output": discarded} {
		if len(got) != 1 || got[0].Command != "echo ok" || got[0].Line != 11 || got[0].Started.UnixMilli() != 1000 || got[0].Completed.UnixMilli() != 2000 {
			t.Fatalf("%s: results = %+v, want one valid command on line 11", name, got)
		}
	}
}

func TestReadWithCodexCommandsReportsPhysicalLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-synthetic.jsonl")
	line := `{"type":"event_msg","payload":{"type":"item_completed","started_at_ms":1000,"completed_at_ms":2000,"item":{"type":"CommandExecution","command":["sh","-lc","echo ok"],"exit_code":0,"aggregated_output":"large"}}}`
	if err := os.WriteFile(path, []byte("\n"+line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var result CodexCommandResult
	_, err := ReadWithCodexCommandsDiscardOutput(path, nil, func(r CodexCommandResult) { result = r })
	if err != nil {
		t.Fatal(err)
	}
	if result.Line != 2 || result.Output != "" {
		t.Fatalf("result = %+v, want line 2 and discarded output", result)
	}
}

func TestReadWithCodexCommandsRejectsWrongTimestampType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-synthetic.jsonl")
	line := `{"type":"event_msg","payload":{"type":"item_completed","started_at_ms":"wrong","completed_at_ms":2000,"item":{"type":"CommandExecution","command":["sh","-lc","echo ok"],"exit_code":0,"aggregated_output":"CommandExecution"}}}`
	missingItem := `{"type":"event_msg","payload":{"type":"item_completed","note":"CommandExecution"}}`
	if err := os.WriteFile(path, []byte(missingItem+"\n"+line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	called := false
	meta, err := ReadWithCodexCommandsDiscardOutput(path, nil, func(CodexCommandResult) { called = true })
	if err != nil {
		t.Fatal(err)
	}
	if called || meta.Bad != 1 {
		t.Fatalf("callback=%t bad=%d, want malformed record skipped and counted", called, meta.Bad)
	}
}

func TestReadKeepsEventsWhenAggregatedOutputIsNotString(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-synthetic.jsonl")
	completion := `{"timestamp":"2026-10-31T13:00:00Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":["sh","-lc","echo x"],"exit_code":0,"aggregated_output":["x"]}}}`
	message := `{"timestamp":"2026-10-31T13:00:01Z","type":"response_item","payload":{"type":"message","role":"assistant","content":"still emitted"}}`
	if err := os.WriteFile(path, []byte(completion+"\n"+message+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var ordinary []Event
	meta, err := Read(path, nil, func(e Event) { ordinary = append(ordinary, e) })
	if err != nil || meta.Bad != 0 {
		t.Fatalf("Read meta = %+v, err=%v; want Bad=0", meta, err)
	}
	var discarded []CodexCommandResult
	var discardedEvents []Event
	meta, err = ReadWithCodexCommandsDiscardOutput(path, func(e Event) { discardedEvents = append(discardedEvents, e) }, func(r CodexCommandResult) {
		discarded = append(discarded, r)
	})
	if err != nil || meta.Bad != 0 || len(discarded) != 1 || discarded[0].Output != "" || !reflect.DeepEqual(ordinary, discardedEvents) {
		t.Fatalf("discard read: results=%+v meta=%+v err=%v; want completion without output and Bad=0", discarded, meta, err)
	}
	var outputEvents []Event
	var outputResults []CodexCommandResult
	meta, err = ReadUptakeOnePass(path, func(e Event) { outputEvents = append(outputEvents, e) }, func(r CodexCommandResult) {
		outputResults = append(outputResults, r)
	}, nil)
	if err != nil || meta.Bad != 0 || len(outputResults) != 0 || !reflect.DeepEqual(ordinary, outputEvents) {
		t.Fatalf("with-output read: results=%+v meta=%+v err=%v; want no command result, same events and Bad=0", outputResults, meta, err)
	}
	withOutput, err := CodexCommandResults(path)
	if err != nil || len(withOutput) != 0 {
		t.Fatalf("CodexCommandResults = %+v, err=%v; want no result when output has wrong type", withOutput, err)
	}
}

func TestReadWithCodexCommandsKeepsCompletionWithNumericTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-synthetic.jsonl")
	line := `{"timestamp":123,"type":"event_msg","payload":{"type":"item_completed","started_at_ms":1000,"completed_at_ms":2000,"item":{"type":"CommandExecution","command":["sh","-lc","tl x"],"exit_code":1,"aggregated_output":"failure"}}}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	want, err := CodexCommandResults(path)
	if err != nil || len(want) != 1 {
		t.Fatalf("old reader results=%+v err=%v; want one", want, err)
	}
	var got []CodexCommandResult
	meta, err := ReadUptakeOnePass(path, nil, func(r CodexCommandResult) { got = append(got, r) }, nil)
	if err != nil || len(got) != 1 || got[0] != want[0] {
		t.Fatalf("new reader results=%+v meta=%+v err=%v; want old result %+v", got, meta, err, want[0])
	}
}

func TestReadUptakeOnePassResetsSessionAfterFallbackMessage(t *testing.T) {
	rows := []string{
		`{"type":"session_meta","payload":{"id":"a"}}`,
		`{"timestamp":"2026-10-31T13:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}}`,
		`{"type":"session_meta","payload":{"id":"b"}}`,
		`{"timestamp":"2026-10-31T13:00:01Z","type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"1","arguments":"{\"command\":\"ls\"}"}}`,
	}
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(joinLines(rows)), 0600); err != nil {
		t.Fatal(err)
	}
	var got []Event
	if _, err := ReadUptakeOnePass(path, func(e Event) { got = append(got, e) }, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Role != Cmd || got[1].SessionID != "b" {
		t.Fatalf("events=%+v; command should belong to session b", got)
	}
}

func TestReadUptakeOnePassMatchesThreeReadersOnCanonicalGeneratedRollouts(t *testing.T) {
	for seed := range 12 {
		rows := []string{`{"type":"session_meta","payload":{"id":"a"}}`}
		for session := 0; session < 2+seed%3; session++ {
			if session > 0 {
				rows = append(rows, fmt.Sprintf(`{"type":"session_meta","payload":{"id":"s%d"}}`, session))
			}
			if (seed+session)%2 == 0 {
				rows = append(rows, `{"timestamp":"2026-10-31T13:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"user"}]}}`)
			}
			model := fmt.Sprintf("gpt-%d", seed+session)
			rows = append(rows, fmt.Sprintf(`{"type":"turn_context","payload":{"model":%q}}`, model))
			rows = append(rows, `{"timestamp":"2026-10-31T13:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":1}}}}`)
			rows = append(rows, `{"type":"event_msg","payload":{"type":"item_completed","started_at_ms":1000,"completed_at_ms":2000,"item":{"type":"CommandExecution","command":["sh","-lc","echo ok"],"exit_code":0,"aggregated_output":"ok"}}}`)
		}
		rows = append(rows, `{"type":"turn_context","payload":{"model":"gpt-cut"`)
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		if err := os.WriteFile(path, []byte(joinLines(rows)), 0600); err != nil {
			t.Fatal(err)
		}
		var wantEvents, gotEvents []Event
		wantMeta, err := Read(path, nil, func(e Event) { wantEvents = append(wantEvents, e) })
		if err != nil {
			t.Fatal(err)
		}
		wantCommands, err := CodexCommandResults(path)
		if err != nil {
			t.Fatal(err)
		}
		var wantModels []string
		model := ""
		for n, row := range rows {
			var rec record
			if json.Unmarshal([]byte(row), &rec) == nil && rec.Type == "turn_context" && rec.Payload != nil && rec.Payload.Model != "" {
				model = rec.Payload.Model
			}
			wantModels = append(wantModels, model)
			_ = n
		}
		var gotCommands []CodexCommandResult
		var gotModels []string
		gotMeta, err := ReadUptakeOnePass(path, func(e Event) { gotEvents = append(gotEvents, e) }, func(r CodexCommandResult) { gotCommands = append(gotCommands, r) }, func(m string, _ int) { gotModels = append(gotModels, m) })
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotEvents, wantEvents) || !reflect.DeepEqual(gotCommands, wantCommands) || !reflect.DeepEqual(gotModels, wantModels) || gotMeta.Bad != wantMeta.Bad {
			t.Fatalf("seed %d: events %v/%v commands %v/%v models %v/%v bad %d/%d", seed, gotEvents, wantEvents, gotCommands, wantCommands, gotModels, wantModels, gotMeta.Bad, wantMeta.Bad)
		}
	}
}

func TestReadUptakeOnePassToleratesNoncanonicalReviewRows(t *testing.T) {
	rows := []string{`{"type":"session_meta","payload":{"id":"a"}}`, `{"type":"turn_context","payload":{"type":42,"model":"new"}}`, `{"type":"turn_context","payload":{"model":"new"}}`, `{"type":"turn_con\\u0074ext","payload":{"model":"escaped"}}`, `{"type":"event_msg", "payload":{"type":"user_message","message":"event"}}`}
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(joinLines(rows)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadUptakeOnePass(path, func(Event) {}, nil, func(string, int) {}); err != nil {
		t.Fatal(err)
	}
}

func joinLines(lines []string) string {
	return strings.Join(lines, "\n") + "\n"
}
