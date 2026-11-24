package main

import "testing"

// A subagent file copied under two parent sessions: the files path names the selected copy by its path, the
// database path with the stored 8-character session ids by a short id that fits both copies.
func TestReviewLinkIDsDuplicateAgent(t *testing.T) {
	home := "/h"
	a := "/h/.claude/projects/-p/11111111-1111-4111-8111-111111111111/subagents/agent-a181d1bbcf323f2c9.jsonl"
	b := "/h/.claude/projects/-p/22222222-2222-4222-8222-222222222222/subagents/agent-a181d1bbcf323f2c9.jsonl"
	sources := []source{{Path: a, ID: "a181d1bb"}}
	files := sourceLinkIDs(sources, []string{a, b}, home, nil)
	stored := sourceLinkIDs(sources, []string{a, b}, home, nil)
	t.Logf("files path %q, database path %q", files[a], stored[a])
	if files[a] != stored[a] {
		t.Errorf("link id differs: files %q, database %q", files[a], stored[a])
	}
	// Two claude sessions whose ids share nine characters.
	c := "/h/.claude/projects/-p/abcdef01-2111-4111-8111-111111111111.jsonl"
	d := "/h/.claude/projects/-p/abcdef01-2222-4222-8222-222222222222.jsonl"
	sources = []source{{Path: c, ID: "abcdef01"}}
	files = sourceLinkIDs(sources, []string{c, d}, home, nil)
	stored = sourceLinkIDs(sources, []string{c, d}, home, nil)
	t.Logf("files path %q, database path %q", files[c], stored[c])
	if files[c] != stored[c] {
		t.Errorf("link id differs: files %q, database %q", files[c], stored[c])
	}
}
