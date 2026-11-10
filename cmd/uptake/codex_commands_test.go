package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcript"
)

// codexScript is a synthetic codex transcript: one exec script runs the same
// utility three times (one call twice with the same arguments), then the
// script output joins all their outputs. Only the per-command completion
// events say which call failed.
func codexScript(failed string) string {
	call := `{"timestamp":"2026-10-31T13:00:00Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"s1","name":"exec","input":"await Promise.all([tools.exec_command({cmd:\"tl aa:1\"}),tools.exec_command({cmd:\"tl bb:2\"}),tools.exec_command({cmd:\"tl aa:1\"})])"}}`
	done := func(cmd, status string, code int, out string) string {
		return `{"timestamp":"2026-10-31T13:00:01Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":["/bin/bash","-lc","` + cmd + `"],"status":"` + status + `","exit_code":` + strconv.Itoa(code) + `,"aggregated_output":"` + out + `"}}}`
	}
	lines := []string{call,
		done("tl aa:1", "completed", 0, "--- event aa:1\\n"),
		done("tl bb:2", "failed", 1, failed),
		done("tl aa:1", "completed", 0, "--- event aa:1 again\\n"),
		`{"timestamp":"2026-10-31T13:00:02Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"s1","output":[{"type":"input_text","text":"Script completed\nWall time 0.1 seconds\nOutput:\n"},{"type":"input_text","text":"--- event aa:1\n` + failed + `--- event aa:1 again\n"}]}}`,
	}
	return strings.Join(lines, "\n")
}

func TestCodexScriptCallsGetTheirOwnResult(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout-synthetic.jsonl")
	writeTranscript(t, path, codexScript(`tl: bb:2: no such session\n`))
	since := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	u := newRun(since, since.Add(24*time.Hour), 1, filepath.Join(root, "out"), root)
	_, _, evs, outs, ok := u.readProcessTranscript(path)
	if !ok {
		t.Fatal("transcript not read")
	}
	var codes, texts []string
	for _, e := range evs {
		if e.Role != transcript.Cmd {
			continue
		}
		o := outs[e.CallID]
		codes = append(codes, e.Text+"="+strconv.Itoa(u.eventExitCode(o)))
		texts = append(texts, strings.TrimSpace(o.Text))
	}
	if got := strings.Join(codes, " "); got != "tl aa:1=0 tl bb:2=1 tl aa:1=0" {
		t.Errorf("codes = %s", got)
	}
	if got := strings.Join(texts, " | "); got != "--- event aa:1 | tl: bb:2: no such session | --- event aa:1 again" {
		t.Errorf("outputs = %s", got)
	}
	u.process(path)
	if n := len(u.refusalSamples["tl"]); n != 1 {
		t.Fatalf("tl refusals = %d, want 1 (only the failed call)", n)
	}
	if ref := u.refusalSamples["tl"][0].command; ref != "tl bb:2" {
		t.Errorf("refusal on %q, want tl bb:2", ref)
	}
}

// Without completion events the code is unknown: a utility's informational
// line is not a refusal, an explicit refusal word still is, and a tk note
// confirmation is a success.
func TestUnknownCodeRefusalOnlyByWords(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	info := "tk: checked from the executor's session\nslug-a noted\n"
	if got := u.toolFault("tk", info, -1, "tk note slug-a 'x'", 0); got != "" {
		t.Errorf("informational tk line at unknown code: fault %q, want none", got)
	}
	if got := u.toolFault("tl", "tl: invalid ref zz\n", -1, "tl zz", 0); got != "refusal" {
		t.Errorf("explicit refusal at unknown code: fault %q, want refusal", got)
	}
	if got := u.toolFault("tl", "tl: bb:2: no such session\n", 1, "tl bb:2", 0); got != "refusal" {
		t.Errorf("own error with code 1: fault %q, want refusal", got)
	}
	if !u.explicitSuccess("tk", info) {
		t.Error("tk note confirmation is not an explicit success")
	}
	if u.explicitSuccess("tk", "tk: slug-a noted later\n") {
		t.Error("a tk message line counted as the note confirmation")
	}
}

// A call that never completed does not take the completion of a later call of
// the same command: it keeps the script output and an unknown code.
func TestCodexCompletionNotTakenFromLaterCall(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout-synthetic.jsonl")
	script := func(id, at string) string {
		return `{"timestamp":"2026-10-31T13:00:` + at + `Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"` + id + `","name":"exec","input":"await tools.exec_command({cmd:\"tl cc:3\"})"}}` + "\n" +
			`{"timestamp":"2026-10-31T13:00:` + at + `Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"` + id + `","output":[{"type":"input_text","text":"Script running\nOutput:\n"}]}}`
	}
	writeTranscript(t, path, script("s1", "01")+"\n"+script("s2", "05")+"\n"+
		`{"timestamp":"2026-10-31T13:00:06Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":["/bin/bash","-lc","tl cc:3"],"status":"completed","exit_code":0,"aggregated_output":"--- event cc:3\n"}}}`)
	since := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	u := newRun(since, since.Add(24*time.Hour), 1, filepath.Join(root, "out"), root)
	_, _, evs, outs, ok := u.readProcessTranscript(path)
	if !ok {
		t.Fatal("transcript not read")
	}
	var got []string
	for _, e := range evs {
		if e.Role == transcript.Cmd {
			got = append(got, strconv.Itoa(u.eventExitCode(outs[e.CallID])))
		}
	}
	if strings.Join(got, " ") != "-1 0" {
		t.Errorf("codes = %v, want [-1 0]: the first call never completed", got)
	}
}
