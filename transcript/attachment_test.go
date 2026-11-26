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

func TestAttachmentTextArrayWithNullStaysEmpty(t *testing.T) {
	for _, value := range []string{
		`{"type":"hook_additional_context","content":["first",null,"second"]}`,
		`{"type":"invoked_skills","skills":[{"name":"alpha-skill"},null]}`,
	} {
		if got := parsedAttachmentText(t, value); got != "" {
			t.Errorf("text for %s = %q, want empty", value, got)
		}
	}
}

func TestAttachmentTextNestedForms(t *testing.T) {
	tests := []struct {
		name, value, want string
	}{
		{
			name:  "instructions file contents",
			value: `{"type":"instructions","files":[{"name":"one.md","content":"first instruction"},{"name":"two.md","content":"second instruction"}]}`,
			want:  "first instruction\nsecond instruction",
		},
		{
			name:  "MCP instruction blocks",
			value: `{"type":"mcp_instructions_delta","addedBlocks":["first block","second block"]}`,
			want:  "first block\nsecond block",
		},
		{
			name:  "blocking hook error",
			value: `{"type":"hook_blocking_error","blockingError":{"blockingError":"hook failed with a useful explanation"}}`,
			want:  "hook failed with a useful explanation",
		},
		{
			name:  "nested file contents",
			value: `{"type":"file","content":{"file":{"content":"file body"}},"filename":"example.txt"}`,
			want:  "file body",
		},
		{
			name:  "file fallback",
			value: `{"type":"file","filename":"example.txt"}`,
			want:  "example.txt",
		},
		{
			name:  "nested memory contents",
			value: `{"type":"nested_memory","content":{"content":"memory body"},"path":"memory.md"}`,
			want:  "memory body",
		},
		{
			name:  "nested memory fallback",
			value: `{"type":"nested_memory","path":"memory.md"}`,
			want:  "memory.md",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parsedAttachmentText(t, tt.value); got != tt.want {
				t.Fatalf("text = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAttachmentTextSkillsSkipsUnnamedObjects(t *testing.T) {
	got := parsedAttachmentText(t, `{"type":"invoked_skills","skills":[{"name":"alpha-skill"},{"name":""},{"name":"beta-skill"}]}`)
	if want := "alpha-skill\nbeta-skill"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestAttachmentTextOtherTypeKeepsFallbackPriority(t *testing.T) {
	got := parsedAttachmentText(t, `{"type":"other","filename":"file.txt","skills":[{"name":"skill-name"}]}`)
	if want := "file.txt"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestAttachmentTextUnknownNestedArrayUsesFallback(t *testing.T) {
	got := parsedAttachmentText(t, `{"type":"instructions","files":[{"content":"visible"},{"body":"unrecognized"}],"filename":"fallback.txt"}`)
	if want := "fallback.txt"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestAttachmentTextNestedArraySkipsEmptyKnownValues(t *testing.T) {
	got := parsedAttachmentText(t, `{"type":"instructions","files":[{"content":"visible"},{"content":""},{"content":[]}]}`)
	if want := "visible"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}
