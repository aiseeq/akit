package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/akit/transcript"
	"github.com/aiseeq/akit/transcript/waits"
)

func TestParseUptakeArgs(t *testing.T) {
	now := time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC)
	since, until, seed, out, err := parseUptakeArgs([]string{"--since", "2026-10-25T11:00:00Z", "--out=/tmp/out"}, now)
	if err != nil || since.Format(time.RFC3339) != "2026-10-25T11:00:00Z" || !until.Equal(now) || seed != 20261025 || out != "/tmp/out" {
		t.Fatalf("parsed (%s, %s, %d, %q), err %v", since, until, seed, out, err)
	}
	for _, args := range [][]string{{"--since", "bad"}, {"--since", "2026-11-01T02:00:00Z", "--until", "2026-11-01T02:00:00Z"}} {
		if _, _, _, _, err := parseUptakeArgs(args, now); err == nil {
			t.Errorf("parseUptakeArgs(%q) unexpectedly succeeded", args)
		}
	}
}

func TestWriteTablesContent(t *testing.T) {
	since := time.Date(2026, 10, 25, 11, 0, 0, 0, time.UTC)
	until := time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC)
	u := newRun(since, until, 20261025, t.TempDir(), t.TempDir())
	u.counts["2026-10-25\tclaude-main\tbusy\tcalls"] = 1
	u.counts["2026-10-25\tclaude-main\tsub\tcalls"] = 1
	u.counts["2026-10-25\tclaude-main\ttl\tcalls"] = 1
	u.counts["2026-10-31\tclaude-main\tbusy\tcrash"] = 1
	u.errorStats["busy\tnew"] = &errorStat{calls: 1, fail: 1}
	u.errorStats["sub\tnew"] = &errorStat{calls: 1, fail: 1, repeat: 1}
	u.errorStats["tl\tnew"] = &errorStat{calls: 1}
	u.tlNewEmptyCount = 1
	u.tlNewEmpty = []example{{kind: "errpair-tl-new", ref: "synthetic:1", reason: "no events"}}
	u.tkPairAdd("tk new", true)
	u.editAttempts = []editAttempt{{tool: "sub", agent: "claude-main", file: "a.go", bytes: 20, success: true}}
	u.examples = []example{{kind: "sub-ok", ref: "synthetic:2", agent: "claude-main", command: "sub", output: "ok"}}
	if err := u.writeTables(); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"counts.tsv":   {"busy\tcalls\t1", "busy\tcrash\t1"},
		"examples.tsv": {"sub-ok\tsynthetic:2"},
		"errors.tsv":   {"busy\tnew\t1\t1"},
		"repeats.tsv":  {"sub\tnew\t1\t1"},
		"tk-pair.tsv":  {"tk new\t1\t1"},
		"tl-empty.tsv": {"new\t1\t1\t0"},
		"sub-cost.tsv": {"sub\t1"},
		"report.md":    {"| busy |", "errors.tsv", "repeats.tsv", "tk-pair.tsv", "tl-empty.tsv", "sub-cost.tsv"},
	}
	for name, fragments := range want {
		data, err := os.ReadFile(filepath.Join(u.outputDir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, fragment := range fragments {
			if !strings.Contains(string(data), fragment) {
				t.Errorf("%s missing %q:\n%s", name, fragment, data)
			}
		}
	}
}

func TestSyntheticClaudeAndCodexTranscripts(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	since := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	claude := filepath.Join(home, ".claude", "projects", "demo", "session.jsonl")
	codex := filepath.Join(home, ".local", "state", "worker", "sample", "codex", "sessions", "2026", "10", "31", "rollout-synthetic.jsonl")
	writeTranscript(t, claude, `{"parentUuid":null,"type":"user","timestamp":"2026-10-31T12:00:00Z","message":{"role":"user","content":"run"}}`+"\n"+
		`{"parentUuid":"a","type":"assistant","timestamp":"2026-10-31T12:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"busy"}}]}}`+"\n"+
		`{"parentUuid":"b","type":"user","timestamp":"2026-10-31T12:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"b1","is_error":true,"content":"busy: synthetic failure"}]}}`)
	writeTranscript(t, codex, `{"timestamp":"2026-10-31T13:00:00Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"c1","arguments":"{\"cmd\":\"sub 'old' 'new' file.go\"}"}}`+"\n"+
		`{"timestamp":"2026-10-31T13:00:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"sub: expected 1, found 0"}}`)
	for _, path := range []string{claude, codex} {
		if err := os.Chtimes(path, until, until); err != nil {
			t.Fatal(err)
		}
	}
	u := newRun(since, until, 20261025, filepath.Join(root, "out"), home)
	if err := u.runUptake(since, until); err != nil {
		t.Fatal(err)
	}
	checks := map[string][]string{
		"counts.tsv":   {"busy\tcalls\t1", "sub\tcalls\t1"},
		"errors.tsv":   {"busy\tnew", "sub\tnew"},
		"examples.tsv": {"refusal-sub", "sub 'old' 'new' file.go"},
		"repeats.tsv":  {"sub\tnew"},
		"tk-pair.tsv":  {"tk new"},
		"tl-empty.tsv": {"old\t0\t0\t0"},
		"sub-cost.tsv": {"sub\t0"},
		"report.md":    {"| busy |", "| sub |"},
	}
	for name, fragments := range checks {
		data, err := os.ReadFile(filepath.Join(u.outputDir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, fragment := range fragments {
			if !strings.Contains(string(data), fragment) {
				t.Errorf("%s missing %q:\n%s", name, fragment, data)
			}
		}
	}
}

func writeTranscript(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunClassifiers(t *testing.T) {
	u := newRun(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), 1, t.TempDir(), t.TempDir())
	for _, tc := range []struct {
		tool, out string
		code      int
		want      string
	}{
		{"sub", "sub: dry run, would make 2 replacements", 0, "dry-run"},
		{"sub", "sub: 1 replacement in 1 file", 0, "success"},
		{"sub", "expected 1, found 2", 1, "mismatch"},
		{"sub", "expected 1, found 0", 1, "not-found"},
		{"sub", "sub: unknown flag", 2, "error"},
		{"tl", "tl: no events", 1, "no-events"},
		{"tl", "tl: 2 event(s)", 0, "events"},
		{"wfor", "wfor: done in 2s", 0, "done"},
		{"wfor", "wfor: not yet after 2s", 1, "not-yet"},
		{"wfor", "wfor: unknown flag", 2, "error"},
	} {
		if got := u.outcome(tc.tool, tc.out, tc.code); got != tc.want {
			t.Errorf("outcome(%q, %q) = %q, want %q", tc.tool, tc.out, got, tc.want)
		}
	}
	if got := u.eventExitCode(transcript.Event{Result: []byte(`{"exitCode":3}`)}); got != 3 {
		t.Errorf("eventExitCode JSON = %d", got)
	}
	if got := u.eventExitCode(transcript.Event{Text: "Process exited with exit code 4"}); got != 4 {
		t.Errorf("eventExitCode text = %d", got)
	}
	if got := u.eventExitCode(transcript.Event{Raw: []byte(`{"is_error":true}`)}); got != 1 {
		t.Errorf("eventExitCode error = %d", got)
	}
	if got := u.findExitCode([]any{map[string]any{"returnCode": float64(7)}}); got != 7 {
		t.Errorf("findExitCode = %d", got)
	}
	if got, ok := u.findIsError([]byte(`{"nested":[{"is_error":false}]}`)); got || !ok {
		t.Errorf("findIsError = (%t,%t)", got, ok)
	}
	if got, ok := u.callFor(transcript.Event{Who: "Bash", Text: "busy"}, true); got != "busy" || !ok {
		t.Errorf("Claude callFor = (%q,%t)", got, ok)
	}
	if _, ok := u.callFor(transcript.Event{Who: "Read"}, true); ok {
		t.Error("Read accepted as Claude shell")
	}
	if _, ok := u.callFor(transcript.Event{Who: "exec"}, false); !ok {
		t.Error("exec rejected as Codex shell")
	}
	for _, tc := range []struct{ in, want string }{{"", ""}, {"Exit code 1\ntl: no events in range", "tl: no events in range"}, {"other output", ""}} {
		got, ok := u.pureNoEventsOutput(tc.in)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("pureNoEventsOutput(%q) = (%q,%t)", tc.in, got, ok)
		}
	}
	if !u.emptySearchOutput(transcript.Event{Error: true}, "Exit code 1") || u.emptySearchOutput(transcript.Event{}, "text") {
		t.Error("emptySearchOutput classification")
	}
	if !u.sameFiles("a.go", "edit a.go") || u.sameFiles("a.go", "b.go") {
		t.Error("sameFiles classification")
	}
	if got := u.ruleFlags("sub -n 2 'old\\n' file.go"); got == "" {
		t.Error("ruleFlags missed flags")
	}
	if !u.valid(time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)) || u.valid(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("window boundary")
	}
}

func TestRunAnalysisHelpers(t *testing.T) {
	home := t.TempDir()
	u := newRun(time.Now().Add(-time.Hour), time.Now(), 1, t.TempDir(), home)
	if !u.decisionSearch(strings.ToLower(filepath.Join(home, ".claude", "decisions.md"))) || u.decisionSearch(filepath.Join(home, ".claude", "other.md")) {
		t.Error("decisionSearch path")
	}
	if path, old, next, err := u.editRaw(transcript.Event{Raw: []byte(`{"file_path":"a.go","old_string":"x","new_string":"y"}`)}); err != nil || path != "a.go" || old != "x" || next != "y" {
		t.Errorf("editRaw = %q %q %q %v", path, old, next, err)
	}
	if _, _, _, err := u.editRaw(transcript.Event{Raw: []byte("{")}); err == nil {
		t.Error("editRaw accepted invalid JSON")
	}
	u.addRepeat("sub", "new")
	u.addError("sub", "new", 1, "sub: expected 1, found 0", false)
	if u.errorStats["sub\tnew"].repeat != 1 {
		t.Error("addRepeat did not count")
	}
	if !u.commandHasTruncator("cat file | head -n 2") || u.commandHasTruncator("cat file") {
		t.Error("commandHasTruncator")
	}
	if !u.hasTranscriptSearch("rg text ~/.claude/projects/session.jsonl") {
		t.Error("hasTranscriptSearch missed transcript search")
	}
	if got := u.waitOutput(waits.Wait{CallIDs: []string{"a", "b"}}, map[string]transcript.Event{"a": {Text: "first"}, "b": {Text: "second"}}); got != "first\nsecond\n" {
		t.Errorf("waitOutput = %q", got)
	}
	if got := u.sampleReason(example{kind: "habit-anchor"}); got == "" {
		t.Error("sampleReason empty")
	}
	if !u.hasExampleRef([]example{{ref: "a.go:1"}}, "a.go:1") || u.hasExampleRef(nil, "a.go:1") {
		t.Error("hasExampleRef")
	}
	if got := u.utilityHabit("uptime"); len(got) == 0 || got[0] != "busy" {
		t.Errorf("utilityHabit = %v", got)
	}
	if !u.singleUtilityCommand("busy", "busy") || u.singleUtilityCommand("busy", "busy && echo x") {
		t.Error("singleUtilityCommand")
	}
	if got := u.toolFault("busy", "busy: invalid input", 2, "busy", 0); got != "call-error" {
		t.Errorf("toolFault = %q", got)
	}
	if got := u.shellWay(shellcmd.Call{Name: "sed", Raw: "sed -i file"}, ""); got != "sed -i" {
		t.Errorf("shellWay = %q", got)
	}
	searchRun := newRun(time.Now().Add(-time.Hour), time.Now(), 1, t.TempDir(), home)
	searchEvents := []transcript.Event{{Line: 1, Role: transcript.User}, {Line: 2, Role: transcript.Cmd, Who: "Bash", Text: "grep x ~/.claude/projects/demo.jsonl"}}
	searchCall := &call{ev: transcript.Event{Line: 40}, command: "tl --since yesterday", agent: "claude-main", tool: "tl", output: "tl: no events"}
	searchRun.recordNewEmptyCall(filepath.Join(home, ".claude", "projects", "demo.jsonl"), searchEvents, nil, searchCall)
	if searchRun.tlNewRetries != 1 {
		t.Error("unmatched line did not retain zero-slot retry sampling")
	}
}
