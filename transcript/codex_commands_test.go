package transcript

import (
	"os"
	"path/filepath"
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

func joinLines(lines []string) string {
	return strings.Join(lines, "\n") + "\n"
}
