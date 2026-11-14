package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestCollidingRolloutLinksStayDistinct(t *testing.T) {
	e, _ := workHome(t)
	start := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	ids := []string{"12345678-0001-7000-8000-000000000001", "12345678-0002-7000-8000-000000000002"}
	names := []string{"first synthetic name", "second synthetic name"}
	paths := make([]string, len(ids))
	for i, id := range ids {
		path := filepath.Join(e.home, ".local", "state", "worker", "synthetic", "codex", "sessions", "2026", "11", "14", "rollout-2026-11-14T08-00-00-"+id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		row := `{"timestamp":"2026-11-14T08:00:00Z","type":"session_meta","payload":{"id":"` + id + `"}}` + "\n" +
			`{"type":"agent-name","agentName":"` + names[i] + `"}` + "\n" +
			`{"timestamp":"2026-11-14T08:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"X synthetic marker ` + names[i] + `"}]}}` + "\n"
		if err := os.WriteFile(path, []byte(row), 0o600); err != nil {
			t.Fatal(err)
		}
		paths[i] = path
	}
	e.now = start.Add(time.Hour)
	code, out, stderr := runTL(t, e, "--all", "--codex", "--grep", "X", "--since", "2026-11-14T00:00:00Z")
	if code != 0 || stderr != "" {
		t.Fatalf("listing code %d, stderr %q, output:\n%s", code, stderr, out)
	}
	for i, name := range names {
		if !strings.Contains(out, "== "+ids[i][:13]+" "+name) {
			t.Errorf("missing correct header for synthetic source %d:\n%s", i, out)
		}
	}
	refs := regexp.MustCompile(`(?m)^\d{2}\.\d{2} \d{2}:\d{2}:\d{2} ([0-9a-f-]+):([0-9]+) `).FindAllStringSubmatch(out, -1)
	if len(refs) != 2 {
		t.Fatalf("got %d event refs, want 2:\n%s", len(refs), out)
	}
	for i, ref := range refs {
		if got := ref[1]; got != ids[i][:13] {
			t.Errorf("listing reference %d is not a longer matching prefix: %q", i, got)
		}
		code, opened, stderr := runTL(t, e, ref[1]+":"+ref[2])
		if code != 0 || stderr != "" || !strings.Contains(opened, names[i]) {
			t.Errorf("ref %d opened wrong source: code %d, stderr %q, output %q", i, code, stderr, opened)
		}
		code, opened, stderr = runTL(t, e, ref[1])
		if code != 0 || stderr != "" || !strings.Contains(opened, names[i]) {
			t.Errorf("id %d opened wrong source: code %d, stderr %q, output %q", i, code, stderr, opened)
		}
	}
	code, _, stderr = runTL(t, e, ids[0][:8]+":3")
	if code != exitUsage || !strings.Contains(stderr, ids[0]) || !strings.Contains(stderr, ids[1]) {
		t.Errorf("ambiguous short ref code %d, stderr %q", code, stderr)
	}
	for i, id := range ids {
		code, opened, stderr := runTL(t, e, id+":3")
		if code != 0 || stderr != "" || !strings.Contains(opened, names[i]) {
			t.Errorf("full ref %d code %d, stderr %q, output %q", i, code, stderr, opened)
		}
	}
}

func TestDistinctClaudeIDsKeepEightCharacterLinks(t *testing.T) {
	e, _ := workHome(t)
	dir := filepath.Join(e.home, ".claude", "projects", "synthetic-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "89abcdef-0000-4000-8000-000000000001.jsonl")
	row := `{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-14T08:00:00Z","message":{"role":"user","content":"X synthetic marker"}}` + "\n"
	if err := os.WriteFile(path, []byte(row), 0o600); err != nil {
		t.Fatal(err)
	}
	e.now = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	code, out, stderr := runTL(t, e, "--all", "--grep", "X", "--since", "2026-11-14T00:00:00Z")
	if code != 0 || stderr != "" || !strings.Contains(out, "89abcdef:1") {
		t.Fatalf("code %d, stderr %q, output:\n%s", code, stderr, out)
	}
}

func TestHelpExplainsLongLinkIDs(t *testing.T) {
	if !strings.Contains(usage, "ID in a link may be longer than 8 characters") {
		t.Fatal("help does not explain that link IDs can exceed 8 characters")
	}
}
