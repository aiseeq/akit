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

func TestCodexRoleFastSkipKeepsPotentialRecords(t *testing.T) {
	all := map[Role]bool{User: true, Agent: true, Note: true, Cmd: true, Out: true}
	cases := []struct {
		name  string
		line  string
		roles map[Role]bool
	}{
		{"escaped type", `{"type":"session_\u006deta","payload":{"id":"s"}}`, map[Role]bool{User: true}},
		{"session meta", `{"type":"session_meta"}`, all},
		{"turn context", `{"type":"turn_context"}`, all},
		{"agent name", `{"type":"agent-name","agentName":"helper"}`, all},
		{"user event marker", `{"type":"event_msg","payload":{"type":"user_message"}}`, map[Role]bool{User: true}},
		{"agent event marker", `{"type":"event_msg","payload":{"type":"agent_message"}}`, map[Role]bool{Agent: true}},
		{"user response item", `{"type":"response_item","payload":{"type":"message","role":"user"}}`, map[Role]bool{User: true}},
		{"assistant response item", `{"type":"response_item","payload":{"type":"message","role":"assistant"}}`, map[Role]bool{Agent: true}},
		{"token note", `{"type":"event_msg","payload":{"type":"token_count"}}`, map[Role]bool{Note: true}},
		{"compacted note", `{"type":"event_msg","payload":{"type":"compacted"}}`, map[Role]bool{Note: true}},
		{"function call", `{"type":"response_item","payload":{"type":"function_call"}}`, map[Role]bool{Cmd: true}},
		{"custom tool call", `{"type":"response_item","payload":{"type":"custom_tool_call"}}`, map[Role]bool{Cmd: true}},
		{"function output", `{"type":"response_item","payload":{"type":"function_call_output"}}`, map[Role]bool{Out: true}},
		{"custom tool output", `{"type":"response_item","payload":{"type":"custom_tool_call_output"}}`, map[Role]bool{Out: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if codexLineHasNoRequestedEvents([]byte(tc.line), tc.roles, false) {
				t.Fatalf("candidate line fast-skipped: %s", tc.line)
			}
		})
	}
}

func TestCodexRoleFastSkipDropsUnrequestedLines(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		roles map[Role]bool
	}{
		{"irrelevant record", `{"type":"other","payload":{"text":"ordinary"}}`, map[Role]bool{User: true}},
		{"unrequested note", `{"type":"event_msg","payload":{"type":"token_count"}}`, map[Role]bool{User: true}},
		{"unrequested tool", `{"type":"response_item","payload":{"type":"function_call"}}`, map[Role]bool{User: true}},
		{"not a response message", `{"type":"response_item","payload":{"type":"function_call","role":"assistant"}}`, map[Role]bool{Agent: true}},
		{"message without response item", `{"type":"other","payload":{"type":"message","role":"assistant"}}`, map[Role]bool{Agent: true}},
		{"unrequested response role", `{"type":"response_item","payload":{"type":"message","role":"developer"}}`, map[Role]bool{User: true, Agent: true}},
		{"role outside message", `{"type":"response_item","payload":{"type":"function_call","role":"assistant"}}`, map[Role]bool{Agent: true}},
		{"role only", `{"type":"response_item","payload":{"role":"assistant"}}`, map[Role]bool{Agent: true}},
		{"message outside requested roles", `{"type":"other","payload":{"type":"message"}}`, map[Role]bool{Note: true}},
		{"response message outside requested roles", `{"type":"response_item","payload":{"type":"message"}}`, map[Role]bool{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !codexLineHasNoRequestedEvents([]byte(tc.line), tc.roles, false) {
				t.Fatalf("unrequested line was not fast-skipped: %s", tc.line)
			}
		})
	}
}

func TestReadCodexRoleLineReportsDiscardableRecord(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(`{"type":"other","payload":{"text":"skip"}}` + "\n"))
	line, skipped, err := readCodexRoleLine(r, map[Role]bool{User: true}, false)
	if err != nil || len(line) == 0 || !skipped {
		t.Fatalf("readCodexRoleLine = (%q, %t, %v), want a nonempty skipped record", line, skipped, err)
	}
}

func TestCodexRoleFastSkipDropsResponseItemsAlreadyRepresentedByTUI(t *testing.T) {
	roles := map[Role]bool{User: true, Agent: true}
	duplicate := []byte(`{"type":"response_item","payload":{"type":"message","role":"user"}}`)
	if !codexLineHasNoRequestedEvents(duplicate, roles, true) {
		t.Fatal("response item was not discarded after parser found both TUI markers")
	}
	if codexLineHasNoRequestedEvents(duplicate, roles, false) {
		t.Fatal("response item was discarded without both TUI markers")
	}
	tui := []byte(`{"type":"event_msg","payload":{"type":"user_message"}}`)
	if codexLineHasNoRequestedEvents(tui, roles, true) {
		t.Fatal("TUI user message was discarded with duplicate response items")
	}
}

func TestReadCodexRoleLineSkipKeepsPhysicalLineNumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-synthetic.jsonl")
	rows := []string{
		`{"type":"session_meta","payload":{"id":"s"}}`,
		`{"type":"other","payload":{"text":"skip"}}`,
		`{"timestamp":"2026-11-18T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"wanted"}]}}`,
	}
	if err := os.WriteFile(path, []byte(joinLines(rows)), 0600); err != nil {
		t.Fatal(err)
	}
	var got []Event
	if _, err := ReadWorkerCodexWithRoles(path, map[Role]bool{User: true}, func(ev Event) { got = append(got, ev) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Line != 3 || got[0].Text != "wanted" {
		t.Fatalf("events = %+v, want user event on physical line 3", got)
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
