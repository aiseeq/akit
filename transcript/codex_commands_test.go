package transcript

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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

// codexFollowRows: a session with one user message, a completed shell command, and lines that must not
// produce a command result (another item type, no command, item_started, the marker only inside a note).
var codexFollowRows = []string{
	`{"timestamp":"2026-10-25T10:00:00.000Z","type":"session_meta","payload":{"id":"s1","cwd":"/w","originator":"codex-tui"}}`,
	`{"timestamp":"2026-10-25T10:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"привет"}]}}`,
	`{"type":"event_msg","payload":{"type":"item_completed","started_at_ms":1000,"completed_at_ms":2000,"item":{"type":"CommandExecution","command":["sh","-lc","echo ok"],"exit_code":0}}}`,
	`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"Other","command":["sh"],"exit_code":0}}}`,
	`{"type":"event_msg","payload":{"type":"item_completed","note":"no command"}}`,
	`{"type":"event_msg","payload":{"type":"item_started","item":{"type":"CommandExecution","command":["sh"],"exit_code":0}}}`,
	`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"Other","note":"CommandExecution"}}}`,
	`{"type":"event_msg","payload":{"type":"item_completed","started_at_ms":3000,"completed_at_ms":0,"item":{"type":"CommandExecution","command":["sh","-lc","false"],"exit_code":1}}}`,
}

var codexFollowWant = []CodexCommandResult{
	{Line: 3, Started: time.UnixMilli(1000), Completed: time.UnixMilli(2000), Command: "echo ok", ExitCode: 0},
	{Line: 8, Started: time.UnixMilli(3000), Completed: time.UnixMilli(0), Command: "false", ExitCode: 1},
}

func TestFollowCodexCommands(t *testing.T) {
	data := []byte(joinLines(codexFollowRows))
	wantEvents, _ := readPrefix(t, data, len(data))
	if len(wantEvents) == 0 {
		t.Fatal("fixture gives no events")
	}
	path := filepath.Join(t.TempDir(), "rollout-synthetic.jsonl")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var readCommands []CodexCommandResult
	if _, err := ReadWithCodexCommandsDiscardOutput(path, func(Event) {}, func(r CodexCommandResult) { readCommands = append(readCommands, r) }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(readCommands, codexFollowWant) {
		t.Fatalf("ReadWithCodexCommandsDiscardOutput = %+v, want %+v", readCommands, codexFollowWant)
	}
	for _, step := range []int{1, 2, 3, len(codexFollowRows)} {
		f := NewFollow()
		var gotEvents []Event
		var gotCommands []CodexCommandResult
		if err := f.OnCodexCommands(func(r CodexCommandResult) { gotCommands = append(gotCommands, r) }); err != nil {
			t.Fatal(err)
		}
		lines := strings.SplitAfter(string(data), "\n")
		for i := 0; i < len(codexFollowRows); i += step {
			chunk := strings.Join(lines[i:min(i+step, len(codexFollowRows))], "")
			if err := f.Feed([]byte(chunk), func(e Event) { gotEvents = append(gotEvents, e) }); err != nil {
				t.Fatalf("step %d: %v", step, err)
			}
		}
		if !reflect.DeepEqual(gotEvents, wantEvents) {
			t.Fatalf("step %d: events = %v, want %v", step, gotEvents, wantEvents)
		}
		if !reflect.DeepEqual(gotCommands, codexFollowWant) {
			t.Fatalf("step %d: commands = %+v, want %+v", step, gotCommands, codexFollowWant)
		}
	}
}

func TestFollowCodexCommandsAfterFeedRejected(t *testing.T) {
	f := NewFollow()
	if err := f.Feed([]byte(codexFollowRows[0]+"\n"), func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if err := f.OnCodexCommands(func(CodexCommandResult) {}); err == nil {
		t.Fatal("callback registration after the first fed line accepted")
	}
}

func TestFollowSkipThroughSkipsCodexCommandCallback(t *testing.T) {
	rows := []string{
		`{"type":"event_msg","payload":{"type":"item_completed","started_at_ms":1000,"completed_at_ms":2000,"item":{"type":"CommandExecution","command":["sh","first"],"exit_code":0}}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","started_at_ms":3000,"completed_at_ms":4000,"item":{"type":"CommandExecution","command":["sh","second"],"exit_code":0}}}`,
	}
	data := []byte(joinLines(rows))
	f := NewFollow()
	if err := f.SkipThrough(1); err != nil {
		t.Fatal(err)
	}
	var got []CodexCommandResult
	if err := f.OnCodexCommands(func(r CodexCommandResult) { got = append(got, r) }); err != nil {
		t.Fatal(err)
	}
	if err := f.Feed(data, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Line != 2 || got[0].Command != "second" {
		t.Fatalf("callback results = %+v, want only line 2", got)
	}
}

func joinLines(lines []string) string {
	return strings.Join(lines, "\n") + "\n"
}
