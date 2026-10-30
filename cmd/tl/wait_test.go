package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeWaitTranscript(t *testing.T, e env, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func claudeWaitRows(specs []struct {
	command string
	delay   int
	bg      bool
	output  bool
}) string {
	var rows []string
	base := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	sec := 0
	for i, s := range specs {
		id := fmt.Sprintf("call%d", i)
		input := fmt.Sprintf(`{"command":%q,"run_in_background":%t}`, s.command, s.bg)
		rows = append(rows, fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":%s}]}}`, base.Add(time.Duration(sec)*time.Second).Format(time.RFC3339), id, input))
		sec += s.delay
		if s.output {
			result := fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]}}`, base.Add(time.Duration(sec)*time.Second).Format(time.RFC3339), id)
			rows = append(rows, result)
		}
	}
	return strings.Join(rows, "\n") + "\n"
}

func TestWaitClaudePercentilesAndFiltering(t *testing.T) {
	e := fakeHome(t)
	specs := make([]struct {
		command string
		delay   int
		bg      bool
		output  bool
	}, 0, 25)
	for i := 1; i <= 20; i++ {
		specs = append(specs, struct {
			command string
			delay   int
			bg      bool
			output  bool
		}{command: "tk done task | head", delay: i, output: true})
	}
	specs = append(specs,
		struct {
			command string
			delay   int
			bg      bool
			output  bool
		}{command: "tk done task && other", delay: 1, output: true},
		struct {
			command string
			delay   int
			bg      bool
			output  bool
		}{command: "tk done task", delay: 1, bg: true, output: true},
		struct {
			command string
			delay   int
			bg      bool
			output  bool
		}{command: "tk done task", delay: 1, output: false},
	)
	path := writeWaitTranscript(t, e, "wait-claude.jsonl", claudeWaitRows(specs))
	code, out, errOut := runTL(t, e, "--wait", "tk done", "--q", "50", path)
	if code != 0 || out != "10000\n" {
		t.Errorf("q50 code=%d out=%q stderr=%q", code, out, errOut)
	}
	code, out, errOut = runTL(t, e, "--wait=tk done", "--q=95", path)
	if code != 0 || out != "19000\n" {
		t.Errorf("q95 code=%d out=%q stderr=%q", code, out, errOut)
	}
	code, out, errOut = runTL(t, e, "--wait", "tk note", "--q", "95", path)
	if code != 3 || out != "" || !strings.Contains(errOut, "0 wait event") {
		t.Errorf("SUB code=%d out=%q stderr=%q", code, out, errOut)
	}
}

func TestWaitCodexAndInsufficientData(t *testing.T) {
	e := fakeHome(t)
	var rows []string
	rows = append(rows, `{"timestamp":"2026-10-25T10:00:00Z","type":"session_meta","payload":{"id":"synthetic"}}`)
	for i := 1; i <= 19; i++ {
		rows = append(rows, fmt.Sprintf(`{"timestamp":"2026-10-25T10:00:%02dZ","type":"response_item","payload":{"type":"function_call","call_id":"c%d","name":"exec_command","arguments":%q}}`, i, i, fmt.Sprintf(`{"cmd":"tk done item | head"}`)))
		rows = append(rows, fmt.Sprintf(`{"timestamp":"2026-10-25T10:00:%02dZ","type":"response_item","payload":{"type":"function_call_output","call_id":"c%d","output":"ok"}}`, i+1, i))
	}
	path := writeWaitTranscript(t, e, "rollout-synthetic.jsonl", strings.Join(rows, "\n")+"\n")
	code, out, errOut := runTL(t, e, "--wait", "tk done", path)
	if code != 3 || out != "" || !strings.Contains(errOut, "19 wait event") {
		t.Errorf("code=%d out=%q stderr=%q", code, out, errOut)
	}
}

func TestWaitIgnoresNonShellAndMultipleCodexCommands(t *testing.T) {
	e := fakeHome(t)
	grep := strings.Join([]string{
		`{"type":"assistant","timestamp":"2026-10-25T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"grep1","name":"Grep","input":{"pattern":"sub"}}]}}`,
		`{"type":"user","timestamp":"2026-10-25T10:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"grep1","content":"match"}]}}`,
	}, "\n") + "\n"
	grepPath := writeWaitTranscript(t, e, "wait-grep.jsonl", grep)
	code, out, errOut := runTL(t, e, "--wait", "sub", grepPath)
	if code != 3 || out != "" || !strings.Contains(errOut, "0 wait event") {
		t.Errorf("Grep code=%d out=%q stderr=%q", code, out, errOut)
	}

	codeText := `const a = await tools.exec_command({cmd:"tk done one"}); const b = await tools.exec_command({cmd:"tk done two"});`
	codex := strings.Join([]string{
		`{"timestamp":"2026-10-25T10:00:00Z","type":"session_meta","payload":{"id":"synthetic"}}`,
		fmt.Sprintf(`{"timestamp":"2026-10-25T10:00:01Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"exec1","name":"exec","input":%q}}`, codeText),
		`{"timestamp":"2026-10-25T10:00:03Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"exec1","output":"ok"}}`,
	}, "\n") + "\n"
	codexPath := writeWaitTranscript(t, e, "rollout-multiple.jsonl", codex)
	code, out, errOut = runTL(t, e, "--wait", "tk done", codexPath)
	if code != 3 || out != "" || !strings.Contains(errOut, "0 wait event") {
		t.Errorf("multiple commands code=%d out=%q stderr=%q", code, out, errOut)
	}
}

func TestWaitUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--wait", "tk", "--usage"}, {"--wait", "tk", "--role", "cmd"},
		{"--wait", "tk", "--max", "5"}, {"--q", "50"},
		{"--wait", "tk", "--q", "100"}, {"--wait", "a b c"},
	} {
		if _, err := parseArgs(args, fakeHome(t).now); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
}
