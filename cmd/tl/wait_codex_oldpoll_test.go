package main

import (
	"fmt"
	"strings"
	"testing"
)

// The older codex form polls with a function_call write_stdin. The wait ends at
// its final answer on the first read and on a read served from the wait cache.
func TestWaitCodexOldFormPollColdAndWarm(t *testing.T) {
	e := fakeHome(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	call := func(at, id, name, args string) string {
		return fmt.Sprintf(`{"timestamp":"2026-10-25T%sZ","type":"response_item","payload":{"type":"function_call","call_id":%q,"name":%q,"arguments":%q}}`, at, id, name, args)
	}
	out := func(at, id, text string) string {
		return fmt.Sprintf(`{"timestamp":"2026-10-25T%sZ","type":"response_item","payload":{"type":"function_call_output","call_id":%q,"output":%q}}`, at, id, text)
	}
	rows := []string{
		`{"timestamp":"2026-10-25T10:00:00Z","type":"session_meta","payload":{"id":"synthetic"}}`,
		call("10:00:00", "a1", "exec_command", `{"cmd":"tk done long"}`),
		out("10:00:30", "a1", "Process running with session ID 4242\nOutput:\nrunning"),
		call("10:00:31", "a2", "write_stdin", `{"session_id":4242,"chars":""}`),
		out("10:00:50", "a2", "Process exited with code 0\nOutput:\ndone"),
		call("10:01:00", "b1", "exec_command", `{"cmd":"tk done b"}`),
		out("10:01:40", "b1", "ok"),
		call("10:02:00", "c1", "exec_command", `{"cmd":"tk done c"}`),
		out("10:02:01", "c1", "ok"),
	}
	path := writeWaitTranscript(t, e, "rollout-old-poll.jsonl", strings.Join(rows, "\n")+"\n")
	args := []string{"--wait", "tk done", "--q", "50", path}
	for _, read := range []string{"cold", "warm"} {
		code, got, errOut := runTL(t, e, args...)
		if code != 0 || got != "40000\n" || !strings.Contains(errOut, "3 wait event(s)") {
			t.Errorf("%s: code=%d out=%q stderr=%q", read, code, got, errOut)
		}
	}
}
