package transcript

import (
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

func TestReadUptakeOnePassMatchesCodexCommandResultsWithEscapedType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-synthetic.jsonl")
	line := `{"type":"event_msg","payload":{"type":"item_completed","started_at_ms":1000,"completed_at_ms":2000,"item":{"type":"CommandExecut\u0069on","command":["sh","-lc","tl x"],"exit_code":1,"aggregated_output":"failure"}}}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	want, err := CodexCommandResults(path)
	if err != nil || len(want) != 0 {
		t.Fatalf("old command scan = %+v, err=%v; want escaped type to be skipped", want, err)
	}
	var got []CodexCommandResult
	_, err = ReadUptakeOnePass(path, nil, func(r CodexCommandResult) { got = append(got, r) }, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("one-pass = %+v, err=%v; want escaped type to be skipped", got, err)
	}
}

func TestReadUptakeOnePassMatchesThreeReadersOnReviewCases(t *testing.T) {
	rows := []string{
		`{"type":"response_item","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":["tl x"],"exit_code":1,"aggregated_output":"failure"}}}`,
		`{"timestamp":123,"type":"event_msg","payload":{"type":"item_completed","started_at_ms":"bad","item":{"type":"CommandExecution","command":["tl x"],"exit_code":1,"aggregated_output":"failure"}}}`,
		`{"timestamp":"2026-10-31T13:00:00Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":["tl x"],"exit_code":1,"aggregated_output":"failure"}},"message":42}`,
		`{"timestamp":"2026-10-31T13:00:00Z","type":"system","content":{"type":"CommandExecution"}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","started_at_ms":1000,"completed_at_ms":2000,"item":{"type":"CommandExecut\u0069on","command":["tl x"],"exit_code":1,"aggregated_output":"failure"}}}`,
	}
	for i, row := range rows {
		t.Run(fmt.Sprintf("case_%d", i+1), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout.jsonl")
			if err := os.WriteFile(path, []byte(row+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			wantCommands, err := CodexCommandResults(path)
			if err != nil {
				t.Fatal(err)
			}
			var gotCommands []CodexCommandResult
			var gotEvents []Event
			meta, err := ReadUptakeOnePass(path, func(e Event) { gotEvents = append(gotEvents, e) }, func(r CodexCommandResult) { gotCommands = append(gotCommands, r) }, nil)
			if err != nil {
				t.Fatal(err)
			}
			var wantEvents []Event
			wantMeta, err := Read(path, nil, func(e Event) { wantEvents = append(wantEvents, e) })
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotCommands, wantCommands) || !reflect.DeepEqual(gotEvents, wantEvents) || meta.Bad != wantMeta.Bad {
				t.Fatalf("uptake commands=%+v events=%+v bad=%d; old commands=%+v events=%+v bad=%d", gotCommands, gotEvents, meta.Bad, wantCommands, wantEvents, wantMeta.Bad)
			}
		})
	}
}

func joinLines(lines []string) string {
	return strings.Join(lines, "\n") + "\n"
}
