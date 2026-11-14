package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/akit/transcript"
)

func TestScanDiagnosticOrder(t *testing.T) {
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "a.jsonl"), filepath.Join(dir, "b.jsonl")}
	sources := []source{{Path: paths[0], ID: "first"}, {Path: paths[1], ID: "second"}}
	opts := options{roles: map[transcript.Role]bool{transcript.User: true}}
	for _, slowFirst := range []bool{true, false} {
		for i, path := range paths {
			raw := "not json\n"
			if (i == 0) == slowFirst {
				raw = strings.Repeat(" ", 8<<20) + raw
			}
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
		}
		got, err := scan(sources, opts, dir)
		if err != nil || got.bad != 2 || got.firstBad == nil || !strings.HasPrefix(got.firstBad.Error(), "first line 1:") {
			t.Fatalf("slowFirst=%v: result=%+v err=%v", slowFirst, got, err)
		}
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	got, err := scan(sources, opts, dir)
	if err != nil || len(got.errs) != 2 || !strings.Contains(got.errs[0].Error(), paths[0]) {
		t.Fatalf("errors=%v err=%v", got.errs, err)
	}
}
