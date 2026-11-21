package transcript

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadWorkerCodexWindowWithRolesUsesParsedEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-synthetic.jsonl")
	rows := []string{
		`{"type":"session_meta","payload":{"id":"s","cwd":"/work/akit"}}`,
		`{"timestamp":"2026-11-18T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"wanted"}]}}`,
		`{"timestamp":"2026-11-18T11:00:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"unrequested"}]}}`,
	}
	if err := os.WriteFile(path, []byte(joinLines(rows)), 0600); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	until := time.Date(2026, 11, 18, 10, 30, 0, 0, time.UTC)
	var got []Event
	_, err := ReadWorkerCodexWindowWithRoles(path, map[Role]bool{User: true}, since, until, func(ev Event) {
		got = append(got, ev)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Role != User || got[0].Text != "wanted" || got[0].Line != 2 {
		t.Fatalf("events = %+v, want only the in-window user event on line 2", got)
	}
}

func TestReadWorkerCodexWindowWithRolesDeduplicatesTUIInOnePass(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-tui.jsonl")
	rows := []string{
		`{"type":"session_meta","payload":{"id":"s"}}`,
		`{"timestamp":"2026-11-18T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"same user"}]}}`,
		`{"timestamp":"2026-11-18T10:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"same agent"}]}}`,
		`{"timestamp":"2026-11-18T10:00:03Z","type":"event_msg","payload":{"type":"user_message","message":"same user"}}`,
		`{"timestamp":"2026-11-18T10:00:04Z","type":"event_msg","payload":{"type":"agent_message","message":"same agent"}}`,
	}
	if err := os.WriteFile(path, []byte(joinLines(rows)), 0600); err != nil {
		t.Fatal(err)
	}
	var got []Event
	roles := map[Role]bool{User: true, Agent: true}
	if _, err := ReadWorkerCodexWithRoles(path, roles, func(ev Event) { got = append(got, ev) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Role != User || got[0].Text != "same user" || got[0].Line != 4 ||
		got[1].Role != Agent || got[1].Text != "same agent" || got[1].Line != 5 {
		t.Fatalf("events = %+v, want TUI user and agent lines only", got)
	}
}

func TestReadCodexRolesPreservesParseStateAndDiagnostics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-synthetic.jsonl")
	rows := []string{
		`{"type":"session_meta","payload":{"id":"one","cwd":"/work/akit"}}`,
		`{"timestamp":"2026-11-18T09:00:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}}`,
		`{"type":"session_meta","payload":{"id":"two","cwd":"/work/other"}}`,
		`{"timestamp":"2026-11-18T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"wanted"}]}}`,
		`{"type":"noise",`,
		`{"type":"noise",}`,
		`{"type":"noise","payload":{"id":42}}`,
		`{"type":"user","timestamp":"2026-11-18T10:00:00Z","message":{"role":"user","content":"mixed Claude record"}}`,
	}
	if err := os.WriteFile(path, []byte(joinLines(rows)), 0600); err != nil {
		t.Fatal(err)
	}
	var got []Event
	meta, err := ReadWorkerCodexWithRoles(path, map[Role]bool{User: true, Agent: true, Note: true}, func(ev Event) { got = append(got, ev) })
	if err != nil {
		t.Fatal(err)
	}
	if meta.Bad != 3 || meta.FirstBad == nil {
		t.Fatalf("bad lines = %d, first bad = %v; want malformed and wrong typed records counted", meta.Bad, meta.FirstBad)
	}
	if len(got) != 3 {
		t.Fatalf("events = %+v, want agent, user and mixed Claude record", got)
	}
	if got[0].Line != 2 || got[0].Role != Agent || got[0].SessionID != "one" || got[0].Cwd != "/work/akit" {
		t.Fatalf("first session event = %+v, want current session and cwd on line 2", got[0])
	}
	if got[1].Line != 4 || got[1].SessionID != "two" || got[1].Cwd != "/work/other" || got[1].Text != "wanted" {
		t.Fatalf("session transition event = %+v, want current session and cwd on line 4", got[1])
	}
	if got[2].Line != 8 || got[2].Role != Note || got[2].Text != "mixed Claude record" {
		t.Fatalf("mixed Claude event = %+v, want parsed prompt note on line 8", got[2])
	}
}

func TestReadCodexRolesKeepsCompactedRecordAfterTUIEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-tui-compact.jsonl")
	rows := []string{
		`{"type":"session_meta","payload":{"id":"s"}}`,
		`{"timestamp":"2026-11-18T10:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"u"}}`,
		`{"timestamp":"2026-11-18T10:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"a"}}`,
		`{"timestamp":"2026-11-18T10:00:03Z","type":"compacted","payload":{"extra":"response_item"}}`,
	}
	if err := os.WriteFile(path, []byte(joinLines(rows)), 0600); err != nil {
		t.Fatal(err)
	}
	var got []Event
	roles := map[Role]bool{User: true, Agent: true, Note: true}
	if _, err := ReadWorkerCodexWithRoles(path, roles, func(ev Event) { got = append(got, ev) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Line != 2 || got[1].Line != 3 || got[2].Line != 4 || got[2].Role != Note {
		t.Fatalf("events = %+v, want TUI messages and compact note", got)
	}
}

func TestCodexCandidateSkipRequiresValidJSONAndNoStateMarkers(t *testing.T) {
	cases := []struct {
		name string
		line string
		skip bool
	}{
		{"irrelevant valid record", `{"type":"event_msg","payload":{"type":"item_started"}}`, true},
		{"malformed record", `{"type":"noise",`, false},
		{"malformed trailing comma", `{"type":"noise",}`, false},
		{"wrong typed payload", `{"type":"noise","payload":{"id":42}}`, false},
		{"session transition", `{"type":"session_meta","payload":{"id":"next"}}`, false},
		{"TUI marker", `{"type":"event_msg","payload":{"type":"user_message"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, skipped, err := readCodexRoleLine(bufio.NewReader(strings.NewReader(tc.line+"\n")), map[Role]bool{User: true, Agent: true}, false)
			if err != nil || skipped != tc.skip || len(line) == 0 {
				t.Fatalf("readCodexRoleLine = (%q, %t, %v), want skipped=%t", line, skipped, err, tc.skip)
			}
		})
	}
}

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

func TestCodexMessagesSkipsLongLinesWithoutLosingLaterMarkers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-synthetic.jsonl")
	rows := []string{
		`{"padding":"` + strings.Repeat("x", 2<<20) + `"}`,
		`{"type":"event_msg","payload":{"type":"user_message"}}`,
		`{"type":"event_msg","payload":{"type":"agent_message"}}`,
	}
	if err := os.WriteFile(path, []byte(joinLines(rows)), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	user, agent, err := codexMessages(f)
	if err != nil || !user || !agent {
		t.Fatalf("codexMessages = (%t, %t, %v), want both markers", user, agent, err)
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
