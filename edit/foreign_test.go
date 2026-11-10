package edit

import (
	"strings"
	"testing"
)

func TestParseForeignSpecForms(t *testing.T) {
	tests := []struct {
		name string
		spec string
		old  string
		new  string
	}{
		{"FILE OLD NEW", "FILE ./x.go\nOLD\nold\nNEW\nnew\n", "old", "new"},
		{"FILE SEARCH REPLACE", "FILE x.go\nSEARCH\nold\nREPLACE\nnew\n", "old", "new"},
		{"plain separator", "=== x.go\nold\n---\nnew\n", "old", "new"},
		{"named separators", "=== x.go\n--- old\nold\n--- new\nnew\n", "old", "new"},
		{"named separators with edge blanks", "=== x.go\n\n--- old\n\nold\n\n--- new\n\nnew\n\n", "old", "new"},
		{"empty edges and CRLF", "=== x.go\r\n\r\nold\r\n\r\n---\r\n\r\nnew\r\n\r\n", "old", "new"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			edits, err := ParseSpec(strings.NewReader(tt.spec))
			if err != nil {
				t.Fatal(err)
			}
			if len(edits) != 1 || edits[0].Old != tt.old || edits[0].New != tt.new || edits[0].Want != 1 {
				t.Fatalf("edits = %#v", edits)
			}
		})
	}
}

func TestParseForeignKeepsEmptyLinesInsideParts(t *testing.T) {
	edits, err := ParseSpec(strings.NewReader("=== x.go\nfirst\n\nlast\n---\nnew first\n\nnew last\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 1 || edits[0].Old != "first\n\nlast" || edits[0].New != "new first\n\nnew last" {
		t.Fatalf("edits = %#v", edits)
	}
}

func TestParseForeignSeveralBlocksAndFixedPath(t *testing.T) {
	spec := "FILE ./x.go\nOLD\na\nNEW\nA\nOLD\nb\nNEW\nB\n"
	edits, err := ParseSpecFor(strings.NewReader(spec), "x.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 2 || edits[0].Files[0] != "./x.go" || edits[1].Old != "b" {
		t.Fatalf("edits = %#v", edits)
	}
	if _, err := ParseSpecFor(strings.NewReader("FILE other.go\nOLD\na\nNEW\nA\n"), "x.go"); err == nil {
		t.Fatal("expected fixed path mismatch")
	}
}

func TestAmbiguousForeignSpecHasCanonicalExample(t *testing.T) {
	for _, spec := range []string{
		"=== x.go\na\n---\nb\n---\nc\n",
		"FILE x.go\nOLD\nOLD\nNEW\nb\n",
		"x.go\na\n===\nb\n",
		"--- x.go\na\n+++\nb\n",
	} {
		if _, err := ParseSpec(strings.NewReader(spec)); err == nil || !strings.Contains(err.Error(), markSearch) || !strings.Contains(err.Error(), markDivider) || !strings.Contains(err.Error(), markReplace) {
			t.Errorf("ParseSpec(%q) error = %v", spec, err)
		}
	}
}

func TestUnrecognizedSpecKeepsNoBlocksErrorAndExample(t *testing.T) {
	_, err := ParseSpec(strings.NewReader("x.go\nold text\nnew text\n"))
	if err == nil || !strings.Contains(err.Error(), "spec has no SEARCH/REPLACE blocks") || !strings.Contains(err.Error(), markSearch) {
		t.Fatalf("error = %v", err)
	}
}
