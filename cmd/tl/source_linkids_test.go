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

func TestSourceLinkIDAvoidsLiveNamePrefix(t *testing.T) {
	path := "/home/project/01a20453-0000-4000-8000-000000000001.jsonl"
	got := sourceLinkIDs([]source{{Path: path, ID: path}}, []string{path}, "/home", map[string]bool{"01a20453": true})[path]
	if got == "01a20453" {
		t.Fatalf("link id %q collides with a live name", got)
	}
}
