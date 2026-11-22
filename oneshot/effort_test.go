package oneshot

import (
	"context"
	"strings"
	"testing"
)

// An empty Effort reaches both CLIs as high; an effort below high is refused before any CLI runs.
func TestEffortDefaultsToHighAndRefusesLower(t *testing.T) {
	claude := fakeCLI(t, "claude", `{"result":"ответ","is_error":false,"usage":{}}`)
	if _, err := Ask(context.Background(), Request{Model: "opus", Input: "вопрос", Approved: approved(t)}); err != nil {
		t.Fatal(err)
	}
	if args := read(t, claude+"/args"); !strings.Contains(args, "--effort\nhigh\n") {
		t.Errorf("claude args lack --effort high:\n%s", args)
	}
	codex := fakeCLI(t, "codex", `{"type":"item.completed","item":{"type":"agent_message","text":"ответ"}}
{"type":"turn.completed","usage":{}}`)
	if _, err := Ask(context.Background(), Request{Model: "sol", Input: "вопрос", Approved: approved(t)}); err != nil {
		t.Fatal(err)
	}
	if args := read(t, codex+"/args"); !strings.Contains(args, "model_reasoning_effort=\"high\"\n") {
		t.Errorf("codex args lack model_reasoning_effort high:\n%s", args)
	}
	for _, e := range []string{"low", "medium", "minimal", "none", "Medium"} {
		_, err := Ask(context.Background(), Request{Model: "sol", Effort: e, Input: "вопрос", Approved: approved(t)})
		if err == nil || !strings.Contains(err.Error(), "below high") {
			t.Errorf("effort %q: err = %v, want below high", e, err)
		}
	}
	if _, err := Ask(context.Background(), Request{Model: "sol", Effort: "auto", Input: "вопрос", Approved: approved(t)}); err == nil {
		t.Error("effort auto: no error, want not one of high, xhigh, max, ultra")
	}
	if _, err := Ask(context.Background(), Request{Model: "sol", Effort: " XHigh ", Input: "вопрос", Approved: approved(t)}); err != nil {
		t.Fatal(err)
	}
	if args := read(t, codex+"/args"); !strings.Contains(args, "model_reasoning_effort=\"xhigh\"\n") {
		t.Errorf("codex args lack normalized xhigh:\n%s", args)
	}
}
