package transcript

import (
	"strings"
	"testing"
)

// A subagent transcript is listed among the kinds kept forever only because it has no events of the kept roles:
// its brief and the orchestrator's follow-ups are records without a source, read as notes.
func TestSubagentTranscriptHasNoKeptRoles(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"user","isSidechain":true,"agentId":"a1","promptId":"p1","timestamp":"2026-10-25T10:00:00Z","message":{"role":"user","content":"Read the brief and do it."}}`,
		`{"type":"assistant","isSidechain":true,"agentId":"a1","timestamp":"2026-10-25T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"Done."}]}}`,
		`{"type":"user","isSidechain":true,"agentId":"a1","promptId":"p2","timestamp":"2026-10-25T10:00:02Z","message":{"role":"user","content":[{"type":"text","text":"One more thing."}]}}`,
	}, "\n") + "\n"
	var roles []Role
	if _, err := ReadReader("/p/session/subagents/agent-a1.jsonl", strings.NewReader(lines), nil, func(e Event) { roles = append(roles, e.Role) }); err != nil {
		t.Fatal(err)
	}
	if len(roles) != 3 {
		t.Fatalf("roles %v, want three events", roles)
	}
	for _, role := range roles {
		if EventKeptForever("sub", role) {
			t.Errorf("subagent event of role %s is kept forever; roles %v", role, roles)
		}
	}
	if !EventKeptForever("claude", User) || !EventKeptForever("sub", Ask) || EventKeptForever("codex", User) || EventKeptForever("claude", Agent) {
		t.Error("EventKeptForever: wrong kinds or roles")
	}
}
