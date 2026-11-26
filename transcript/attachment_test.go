package transcript

import (
	"encoding/json"
	"testing"
)

func parsedAttachmentText(t *testing.T, value string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": "attachment", "timestamp": "2026-11-26T10:00:00Z", "attachment": json.RawMessage(value),
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := parseLine(raw, newClaudeParser(), newCodexParser())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Role != Attach {
		t.Fatalf("events = %#v, want one attach event", events)
	}
	return events[0].Text
}

func TestAttachmentTextArrayContent(t *testing.T) {
	got := parsedAttachmentText(t, `{"type":"hook_additional_context","content":["first","second"]}`)
	if want := "first\nsecond"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestAttachmentTextInvokedSkills(t *testing.T) {
	got := parsedAttachmentText(t, `{"type":"invoked_skills","skills":[{"name":"alpha-skill","path":"/skills/alpha","content":"instructions"},{"name":"beta-skill","path":"/skills/beta","content":"instructions"}]}`)
	if want := "alpha-skill\nbeta-skill"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestAttachmentTextNonStringArraysStayEmpty(t *testing.T) {
	for _, value := range []string{
		`{"type":"hook_additional_context","content":[1,2]}`,
		`{"type":"hook_additional_context","content":[{"text":"value"}]}`,
	} {
		if got := parsedAttachmentText(t, value); got != "" {
			t.Errorf("text for %s = %q, want empty", value, got)
		}
	}
}

func TestAttachmentTextStringField(t *testing.T) {
	got := parsedAttachmentText(t, `{"type":"diagnostics","content":"plain text"}`)
	if want := "plain text"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestAttachmentTextArraySkipsEmptyStrings(t *testing.T) {
	got := parsedAttachmentText(t, `{"type":"hook_additional_context","content":["first","","second"]}`)
	if want := "first\nsecond"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}
