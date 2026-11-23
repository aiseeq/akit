package main

import (
	"strings"
	"testing"
)

func TestSourceLinkIDUsesPathWhenIDIsPrefix(t *testing.T) {
	paths := []string{"/home/project/session/subagents/agent-abc.jsonl", "/home/project/session/subagents/agent-abcd.jsonl"}
	sources := []source{{Path: paths[0], ID: "abc"}}
	got := sourceLinkIDs(sources, paths, "/home", nil)[paths[0]]
	if strings.HasPrefix(got, "abc") {
		t.Fatalf("ambiguous short id %q; want path link", got)
	}
}
