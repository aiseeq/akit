package main

import (
	"fmt"
	"strings"
	"testing"
)

// The word "session" in the output of a Claude Bash call is no codex session:
// the call ends at its output as before.
func TestWaitClaudeOutputMentioningSessionEndsAtOutput(t *testing.T) {
	e := fakeHome(t)
	var rows []string
	for i, text := range []string{"идёт, session 42", "session_id: 77", "codex session 5 archived", "ok"} {
		rows = append(rows,
			fmt.Sprintf(`{"type":"assistant","timestamp":"2026-10-25T10:0%d:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t%d","name":"Bash","input":{"command":"tk done x%d"}}]}}`, i, i, i),
			fmt.Sprintf(`{"type":"user","timestamp":"2026-10-25T10:0%d:0%dZ","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t%d","content":%q}]}}`, i, i+1, i, text))
	}
	path := writeWaitTranscript(t, e, "claude-session-word.jsonl", strings.Join(rows, "\n")+"\n")
	code, out, errOut := runTL(t, e, "--wait", "tk done", "--q", "50", path)
	if code != 0 || out != "2000\n" || !strings.Contains(errOut, "4 wait event(s)") {
		t.Errorf("code=%d out=%q stderr=%q", code, out, errOut)
	}
}

// A codex command left without a final poll does not end when another command
// later gets the same session id: the abandoned wait is dropped.
func TestWaitCodexReusedSessionIDDoesNotEndAbandonedCommand(t *testing.T) {
	e := fakeHome(t)
	rows := []string{
		`{"timestamp":"2026-10-25T10:00:00Z","type":"session_meta","payload":{"id":"synthetic"}}`,
		codexExecRow("10:00:00", "a1", fmt.Sprintf(codexStart, "abandoned")),
		codexExecOutput(t, "10:00:30", "a1", `{"wall_time_seconds":30.0,"session_id":5150,"output":""}`),
		codexExecRow("10:01:00", "s1", `const r = await tools.exec_command({cmd:"sleep 300"}); text(r.output); if (r.session_id) text(`+"`session_id=${r.session_id}`"+`)`),
		codexExecOutput(t, "10:01:30", "s1", `{"wall_time_seconds":30.0,"session_id":5150,"output":""}`),
		codexExecRow("10:06:00", "s2", fmt.Sprintf(codexPoll, 5150)),
		codexExecOutput(t, "10:06:40", "s2", "done\n"),
	}
	for i := 1; i <= 3; i++ {
		rows = append(rows,
			codexExecRow(fmt.Sprintf("10:1%d:00", i), fmt.Sprintf("c%d", i), fmt.Sprintf(codexStart, "ok")),
			codexExecOutput(t, fmt.Sprintf("10:1%d:02", i), fmt.Sprintf("c%d", i), "ok\n"))
	}
	path := writeWaitTranscript(t, e, "rollout-reused-session.jsonl", strings.Join(rows, "\n")+"\n")
	code, out, errOut := runTL(t, e, "--wait", "tk done", "--q", "50", path)
	if code != 0 || out != "2000\n" || !strings.Contains(errOut, "3 wait event(s)") {
		t.Errorf("code=%d out=%q stderr=%q", code, out, errOut)
	}
}
