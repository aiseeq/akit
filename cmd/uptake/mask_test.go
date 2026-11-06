package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcript"
)

// The tables quote commands and their output from transcripts, so a secret typed
// into a command would otherwise stay in them. The value sits across the point
// where a cell is cut, the place a mask run after the cut would miss.
func TestTablesMaskSecrets(t *testing.T) {
	key := "sk" + "-proj-" + strings.Repeat("x9Y8", 6)
	head := key[:12]
	long := strings.Repeat("a", 290) + " " + key
	u := newRun(time.Time{}, time.Time{}, 3, t.TempDir(), t.TempDir())
	u.subCases = []example{{kind: "sub-applied", ref: "synthetic:1", agent: "claude", command: long, output: key, next: key, reason: key}}
	u.pairCalls = []pairCall{{tool: "prior", way: "new", id: "synthetic:2", code: -1, outcome: "unknown", command: long, out: key}}
	u.collectCandidate("synthetic", []transcript.Event{{Line: 3}}, 0, "sleep 5; "+long, "claude")
	if len(u.pairCandidates) == 0 {
		t.Fatal("the fixture made no candidate")
	}
	u.pairCandidates[0].context = key
	if err := u.writeExamples(); err != nil {
		t.Fatal(err)
	}
	if err := u.writePairUnknown(); err != nil {
		t.Fatal(err)
	}
	if err := u.writePairTables(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"examples.tsv", "pair-unknown.tsv", "candidates.tsv"} {
		data, err := os.ReadFile(filepath.Join(u.outputDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "скрыто") {
			t.Errorf("%s has no mask, the fixture did not reach it:\n%s", name, data)
		}
		if strings.Contains(string(data), head) {
			t.Errorf("%s keeps the secret:\n%s", name, data)
		}
	}
}
