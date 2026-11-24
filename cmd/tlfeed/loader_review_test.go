package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/akit/transcript"
)

func reviewSubPath(h *harness) string {
	parent := claudePath(h.home)
	return filepath.Join(strings.TrimSuffix(parent, ".jsonl"), "subagents", "agent-a1b2c3d4e5.jsonl")
}

func TestReviewIDZero(t *testing.T) {
	h := newHarness(t)
	path := reviewSubPath(h)
	write(t, path, []byte(`{"partial":`))
	l := h.loader(nil)
	h.pass(l)
	write(t, strings.TrimSuffix(path, ".jsonl")+".meta.json", []byte(`{"description":"late"}`))
	h.pass(l)
	for _, r := range h.fake.latestFiles() {
		if r[0] == "0" {
			t.Errorf("files row with file_id 0")
		}
	}
}

func TestReviewLookupRace(t *testing.T) {
	h := newHarness(t)
	path := reviewSubPath(h)
	write(t, path, sample(t, "claude.jsonl"))
	l, _ := h.started(nil)
	wroteMeta := false
	l.readSource = func(sourcePath string) (transcript.Source, error) {
		src, err := transcript.SourceAt(sourcePath)
		if err == nil && !wroteMeta {
			write(t, strings.TrimSuffix(sourcePath, ".jsonl")+".meta.json", []byte(`{"description":"late"}`))
			wroteMeta = true
		}
		return src, err
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := l.lookup(path, st)
	if err != nil {
		t.Fatal(err)
	}
	h.pass(l)
	h.pass(l)
	if f.src.Description != "late" {
		t.Errorf("description %q after the race, want late", f.src.Description)
	}
}

func TestReviewRestart(t *testing.T) {
	h := newHarness(t)
	path := reviewSubPath(h)
	write(t, path, sample(t, "claude.jsonl"))
	h.loader(nil)
	before := h.fake.latestFiles()
	write(t, strings.TrimSuffix(path, ".jsonl")+".meta.json", []byte(`{"description":"late"}`))
	h.loader(nil)
	after := h.fake.latestFiles()
	if len(after) != 1 || after[0][7] != "late" {
		t.Fatalf("after restart: %v", after)
	}
	for _, c := range []string{"ingested", "lines", "ev_lines", "size", "gen", "tail", "first_ts", "last_ts", "dirty"} {
		if before[0][fileCol[c]] != after[0][fileCol[c]] {
			t.Errorf("%s %s -> %s", c, before[0][fileCol[c]], after[0][fileCol[c]])
		}
	}
	if ver(after[0]) <= ver(before[0]) {
		t.Errorf("ver not above")
	}
}

func TestReviewMetaChanged(t *testing.T) {
	h := newHarness(t)
	path := reviewSubPath(h)
	meta := strings.TrimSuffix(path, ".jsonl") + ".meta.json"
	write(t, path, sample(t, "claude.jsonl"))
	write(t, meta, []byte(`{"description":""}`))
	l := h.loader(nil)
	before := h.fake.latestFiles()
	h.pass(l)
	write(t, meta, []byte(`{"description":"now set"}`))
	h.pass(l)
	after := h.fake.latestFiles()
	if len(after) != 1 || after[0][7] != "now set" {
		t.Fatalf("after change: %v", after)
	}
	for _, c := range []string{"ingested", "lines", "ev_lines", "size", "gen", "tail", "inode", "mtime", "dirty"} {
		if before[0][fileCol[c]] != after[0][fileCol[c]] {
			t.Errorf("%s %s -> %s", c, before[0][fileCol[c]], after[0][fileCol[c]])
		}
	}
	if ver(after[0]) <= ver(before[0]) {
		t.Errorf("ver not above")
	}
}

func TestReviewNoMetadataNoRepeatedRead(t *testing.T) {
	h := newHarness(t)
	path := reviewSubPath(h)
	write(t, path, sample(t, "claude.jsonl"))
	l := h.loader(nil)
	var reads int
	l.readSource = func(string) (transcript.Source, error) {
		reads++
		return transcript.Source{}, nil
	}
	h.pass(l)
	h.pass(l)
	if reads != 0 {
		t.Fatalf("read source %d times while the sidecar is absent and unchanged", reads)
	}
}

func TestReviewNoSubMetadataReadForOtherFiles(t *testing.T) {
	h := newHarness(t)
	l := h.loader(nil)
	var reads int
	l.readSource = func(string) (transcript.Source, error) {
		reads++
		return transcript.Source{}, nil
	}
	f := &file{kind: "session", src: transcript.Source{Path: filepath.Join(h.home, "session.jsonl")}}
	if l.refreshSubMeta(f) {
		t.Fatal("refreshed metadata for a non-subagent")
	}
	if reads != 0 {
		t.Fatalf("read source %d times for a non-subagent", reads)
	}
}

func TestReviewGoneSubagentMetadataIsSkipped(t *testing.T) {
	h := newHarness(t)
	path := reviewSubPath(h)
	write(t, path, sample(t, "claude.jsonl"))
	l := h.loader(nil)
	f := l.files[path]
	l.vanished(path)
	write(t, strings.TrimSuffix(path, ".jsonl")+".meta.json", []byte(`{"description":"late"}`))
	if err := l.refreshSubMetadata(); err != nil {
		t.Fatal(err)
	}
	if f.src.Description != "" {
		t.Fatalf("gone subagent description updated to %q", f.src.Description)
	}
}
