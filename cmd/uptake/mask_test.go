package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/secretmask"
	"github.com/aiseeq/akit/shellcmd"
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
	evs := []transcript.Event{
		{Line: 2, Role: transcript.Cmd, Text: strings.Repeat("c", candidateContextRunes-len(head)) + " " + key},
		{Line: 3},
	}
	u.collectCandidate("synthetic", evs, 1, "sleep 5; "+long, "claude")
	if !strings.Contains(u.pairCandidates[0].context, "скрыто") {
		t.Fatalf("the context fixture did not reach the cut: %q", u.pairCandidates[0].context)
	}
	if len(u.pairCandidates) == 0 {
		t.Fatal("the fixture made no candidate")
	}
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

// The whole file is what a later reader masks and checks: a quote left open by a
// cut must not join the next row into one value, a NUL makes the file unreadable
// for such a check, and a real line break written out must not turn the next line
// into the value of a label.
func TestTableFileReadsCleanAsAWhole(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 3, t.TempDir(), t.TempDir())
	cut := `curl -s -u "$JIRA_EMAI`
	rows := []string{
		"id:1\t" + u.clean(cut),
		"id:2\t" + u.clean("plain \"other\" text"),
		"id:3\t" + u.clean("binary\x00output"),
		"id:4\t" + u.clean("1318:  target:\n1319:  docs-set-password:\n1320:  next"),
		"id:5\t" + escapePairField("binary\x00output"),
		"id:6\t" + finishCell(escapePairField(truncateRunes(cut+`more"`, len(cut)))),
		"id:7\t" + u.clean(`curl -s -u "$JIRA_EMAI\`),
		"id:8\t" + u.clean("plain \"last\" text"),
	}
	file := strings.Join(rows, "\n") + "\n"
	if strings.Contains(file, "\x00") {
		t.Errorf("a NUL stayed in the table: %q", file)
	}
	if masked, found := secretmask.Mask(file); len(found) > 0 {
		t.Errorf("the file as a whole still has %v:\n%s", found, masked)
	}
}

// Long output keeps only its ends: the cut must not leave a piece of a secret the
// mask no longer recognizes. Wide runes bring the cut inside the cell's limit.
func TestClippedOutputMasksSecretAtTheCut(t *testing.T) {
	key := "sk" + "-proj-" + strings.Repeat("x9Y8", 6)
	out := strings.Repeat("─", 1361) + key + strings.Repeat("─", 3000)
	u := newRun(time.Time{}, time.Time{}, 3, t.TempDir(), t.TempDir())
	u.pairCalls = []pairCall{{tool: "prior", way: "new", id: "synthetic:1", code: -1, outcome: "unknown", command: "prior x", out: clipOutput(out, pairOutputKeep)}}
	if err := u.writePairUnknown(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(u.outputDir, "pair-unknown.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), key[:12]) {
		t.Errorf("a piece of the secret stayed beside the cut:\n%s", data)
	}
}

func TestTKRefusalKindMasksSecret(t *testing.T) {
	key := "sk" + "-proj-" + strings.Repeat("x9Y8", 6)
	u := newRun(time.Time{}, time.Time{}, 3, t.TempDir(), t.TempDir())
	u.collectTKRefusal("synthetic", "claude", transcript.Event{}, shellcmd.Call{Name: "tk", Args: []string{"note", "x"}}, "tk: "+key, 1)
	if len(u.tkRefusals) != 1 {
		t.Fatalf("the fixture made %d refusals", len(u.tkRefusals))
	}
	if kind := u.tkRefusals[0].kind; strings.Contains(kind, "proj") {
		t.Errorf("the refusal kind keeps the secret: %q", kind)
	}
}
