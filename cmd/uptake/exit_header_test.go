package main

import (
	"testing"
	"time"

	"github.com/aiseeq/akit/transcript"
)

// The exit code comes from the header of a tool output, not from the text a
// command printed: tl showing an old event that starts with "Exit code 128"
// succeeded, while a real kill keeps its code.
func TestEventExitCodeFromHeaderOnly(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	shown := "--- 02.11.2026 17:00:27 CET abcd:368 out exec\nExit code 128\nfatal: not a git repository\n"
	for _, tc := range []struct {
		name string
		e    transcript.Event
		want int
	}{
		{"claude success printing an old failure", transcript.Event{UUID: "u1", Text: shown}, 0},
		{"claude success whose output starts with Exit code", transcript.Event{UUID: "u1", Text: "Exit code 128\nfatal: x"}, 0},
		{"claude error keeps its code", transcript.Event{UUID: "u1", Error: true, Text: "Exit code 137\n" + shown}, 137},
		{"claude error without a code line", transcript.Event{UUID: "u1", Error: true, Text: "tool failed"}, -1},
		{"codex exec output printing an old failure", transcript.Event{CallID: "c1", Text: shown}, 0},
		{"codex output quoting exit status", transcript.Event{CallID: "c1", Text: "build\ngraft: exec: go run ./x: exit status 2\n"}, 0},
		{"codex exec_command header", transcript.Event{CallID: "c1", Text: "Wall time: 0.5 seconds\nProcess exited with code 2\nOriginal token count: 10\nOutput:\nExit code 128\n"}, 2},
		{"codex exec_command header, Exit code form", transcript.Event{CallID: "c1", Text: "Exit code: 3\nWall time: 0.1 seconds\nOutput:\nok\n"}, 3},
		{"bare first line", transcript.Event{Text: "Process exited with exit code 4"}, 4},
	} {
		if got := u.eventExitCode(tc.e); got != tc.want {
			t.Errorf("%s: eventExitCode = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A codex exec script that completed says nothing of its commands' exit codes:
// the code stays unknown, and a utility's own refusal line still counts.
func TestCodexScriptOutputCodeUnknown(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	raw := []byte(`[{"type":"input_text","text":"Script completed\nWall time 0.1 seconds\nOutput:\n"},{"type":"input_text","text":"sub: nothing changed: 1 of 1 edits mismatched\n"}]`)
	e := transcript.Event{CallID: "c1", Raw: raw, Text: "sub: nothing changed: 1 of 1 edits mismatched\n"}
	if got := u.eventExitCode(e); got != -1 {
		t.Errorf("eventExitCode = %d, want -1", got)
	}
	code, err := u.utilityCode(e, "sub", "sub 'a' 'b' f.go", e.Text)
	if err != nil || code != 1 {
		t.Errorf("utilityCode = %d, %v; want 1", code, err)
	}
	if got := u.toolFault("sub", e.Text, code, "sub 'a' 'b' f.go", 0); got != "refusal" {
		t.Errorf("toolFault = %q, want refusal", got)
	}
	plain := transcript.Event{CallID: "c1", Raw: []byte(`[{"type":"input_text","text":"Wall time: 0.1 seconds\nOutput:\nok"}]`), Text: "Wall time: 0.1 seconds\nOutput:\nok"}
	if got := u.eventExitCode(plain); got != 0 {
		t.Errorf("non-script output eventExitCode = %d, want 0", got)
	}
}
