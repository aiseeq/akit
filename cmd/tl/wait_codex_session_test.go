package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// codexExecRow is an exec call of codex: JavaScript that calls one tool.
func codexExecRow(at, callID, code string) string {
	return fmt.Sprintf(`{"timestamp":"2026-10-25T%sZ","type":"response_item","payload":{"type":"custom_tool_call","call_id":%q,"name":"exec","input":%q}}`, at, callID, code)
}

// codexExecOutput is the answer of an exec call, as codex writes it: a header
// and the tool result in text parts.
func codexExecOutput(t *testing.T, at, callID string, parts ...string) string {
	t.Helper()
	type part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	items := []part{{"input_text", "Script completed\nWall time 30.0 seconds\nOutput:\n"}}
	for _, p := range parts {
		items = append(items, part{"input_text", p})
	}
	out, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"timestamp":"2026-10-25T%sZ","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":%q,"output":%s}}`, at, callID, out)
}

const (
	codexStart = `const r = await tools.exec_command({cmd:"tk done %s"}); text(JSON.stringify(r))`
	codexPoll  = `const r = await tools.write_stdin({session_id:%d,yield_time_ms:270000}); text(JSON.stringify(r))`
)

// A codex command still running after the exec yield (30 s by default) answers
// with a session id; the agent polls it with write_stdin until it ends. The wait
// lasts until the poll that answers without the session (the command ended),
// not until the first answer.
func TestWaitCodexCommandLastsUntilItsSessionEnds(t *testing.T) {
	e := fakeHome(t)
	rows := []string{
		`{"timestamp":"2026-10-25T10:00:00Z","type":"session_meta","payload":{"id":"synthetic"}}`,
		// 75 s: two polls, the first still running.
		codexExecRow("10:00:00", "a1", fmt.Sprintf(codexStart, "long")),
		codexExecOutput(t, "10:00:30", "a1", `{"wall_time_seconds":30.0,"session_id":68634,"output":"gate running\n"}`),
		codexExecRow("10:00:32", "a2", fmt.Sprintf(codexPoll, 68634)),
		codexExecOutput(t, "10:01:00", "a2", "still running\n", "session_id=68634"),
		codexExecRow("10:01:01", "a3", fmt.Sprintf(codexPoll, 68634)),
		codexExecOutput(t, "10:01:15", "a3", "done\n"),
		// 40 s: one poll.
		codexExecRow("10:02:00", "b1", fmt.Sprintf(codexStart, "middle")),
		codexExecOutput(t, "10:02:30", "b1", `{"wall_time_seconds":30.0,"session_id":70001,"output":""}`),
		codexExecRow("10:02:31", "b2", fmt.Sprintf(codexPoll, 70001)),
		codexExecOutput(t, "10:02:40", "b2", "ok\n"),
		// 1 s: ended before the yield.
		codexExecRow("10:03:00", "c1", fmt.Sprintf(codexStart, "short")),
		codexExecOutput(t, "10:03:01", "c1", "ok\n"),
	}
	path := writeWaitTranscript(t, e, "rollout-session.jsonl", strings.Join(rows, "\n")+"\n")
	for _, tc := range []struct{ q, out string }{{"50", "40000\n"}, {"1", "1000\n"}} {
		code, out, errOut := runTL(t, e, "--wait", "tk done", "--q", tc.q, path)
		if code != 0 || out != tc.out || !strings.Contains(errOut, "3 wait event(s)") {
			t.Errorf("q=%s: code=%d out=%q stderr=%q", tc.q, code, out, errOut)
		}
	}
}

// A command whose session never ends in the transcript (the agent stopped
// polling) is no finished wait: it is not counted as its first answer.
func TestWaitCodexUnfinishedSessionIsNotCounted(t *testing.T) {
	e := fakeHome(t)
	rows := []string{`{"timestamp":"2026-10-25T10:00:00Z","type":"session_meta","payload":{"id":"synthetic"}}`}
	for i := 1; i <= 3; i++ {
		rows = append(rows,
			codexExecRow(fmt.Sprintf("10:0%d:00", i), fmt.Sprintf("s%d", i), fmt.Sprintf(codexStart, "ok")),
			codexExecOutput(t, fmt.Sprintf("10:0%d:02", i), fmt.Sprintf("s%d", i), "ok\n"))
	}
	rows = append(rows,
		codexExecRow("10:05:00", "u1", fmt.Sprintf(codexStart, "unfinished")),
		codexExecOutput(t, "10:05:30", "u1", `{"wall_time_seconds":30.0,"session_id":71000,"output":""}`))
	path := writeWaitTranscript(t, e, "rollout-unfinished.jsonl", strings.Join(rows, "\n")+"\n")
	code, out, errOut := runTL(t, e, "--wait", "tk done", "--q", "50", path)
	if code != 0 || out != "2000\n" || !strings.Contains(errOut, "3 wait event(s)") {
		t.Errorf("code=%d out=%q stderr=%q", code, out, errOut)
	}
}
