package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestWaitCodexPollOfAnotherSessionDoesNotEndCommand(t *testing.T) {
	e := fakeHome(t)
	rows := []string{
		`{"timestamp":"2026-10-25T10:00:00Z","type":"session_meta","payload":{"id":"synthetic"}}`,
		codexExecRow("10:00:00", "a1", fmt.Sprintf(codexStart, "first")),
		codexExecOutput(t, "10:00:30", "a1", `{"session_id":11,"output":"running"}`),
		codexExecRow("10:00:40", "b1", fmt.Sprintf(codexStart, "second")),
		codexExecOutput(t, "10:01:10", "b1", `{"session_id":22,"output":"running"}`),
		codexExecRow("10:01:12", "p1", fmt.Sprintf(codexPoll, 22)),
		codexExecOutput(t, "10:01:13", "p1", "done\n"),
		codexExecRow("10:01:14", "p2", fmt.Sprintf(codexPoll, 11)),
		codexExecOutput(t, "10:01:30", "p2", `{"session_id":11,"output":"still running"}`),
		codexExecRow("10:01:31", "p3", fmt.Sprintf(codexPoll, 11)),
		codexExecOutput(t, "10:01:40", "p3", "done\n"),
		codexExecRow("10:02:00", "c1", fmt.Sprintf(codexStart, "short")),
		codexExecOutput(t, "10:02:01", "c1", "ok\n"),
	}
	path := writeWaitTranscript(t, e, "rollout-other-session.jsonl", strings.Join(rows, "\n")+"\n")
	code, out, errOut := runTL(t, e, "--wait", "tk done", "--q", "50", path)
	if code != 0 || out != "33000\n" || !strings.Contains(errOut, "3 wait event(s)") {
		t.Fatalf("code=%d out=%q stderr=%q", code, out, errOut)
	}
}
