package main

import (
	"strings"
	"testing"
)

// A line of spaces or tabs at the edge of a part is blank too, as in the
// SEARCH/REPLACE form: it never lands in the file.
func TestForeignFormWhitespaceLinesAroundParts(t *testing.T) {
	for name, spec := range map[string]string{
		"=== whitespace between blocks":   "=== %s\nalpha\n---\nALPHA\n \t\n=== %s\ngamma\n---\nGAMMA\n",
		"=== whitespace before ---":       "=== %s\nalpha\n  \n---\n\t\nALPHA\n=== %s\ngamma\n---\nGAMMA\n",
		"FILE whitespace between blocks":  "FILE %s\nOLD\nalpha\nNEW\nALPHA\n\t\nOLD\ngamma\nNEW\nGAMMA\n",
		"FILE whitespace around OLD, NEW": "FILE %s\nOLD\n \nalpha\n\t\nNEW\n  \nALPHA\nOLD\ngamma\nNEW\nGAMMA\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := tempFile(t, foreignBody)
			code, out, errOut := runSub(t, strings.ReplaceAll(spec, "%s", path))
			if code != 0 || content(t, path) != "ALPHA\nbeta\nGAMMA\n" {
				t.Fatalf("code %d, file %q, out %q, err %q", code, content(t, path), out, errOut)
			}
		})
	}
}

// The sample in a refusal reads the first block of the input: its old text in
// SEARCH and its new text in REPLACE, with no control lines of the foreign form.
func TestForeignRefusalSampleReadsFirstBlock(t *testing.T) {
	for name, tc := range map[string]struct{ spec, old, repl string }{
		"FILE with OLD inside the text": {"FILE %s\nOLD\nbeta\nNEW\nBETA\nOLD\nOLD\nNEW\nx\n", "beta", "BETA"},
		"unified diff":                  {"--- a/%s\n+++ b/%s\n@@ -1,3 +1,3 @@\n alpha\n-beta\n+BETA\n gamma\n", "alpha\nbeta\ngamma", "alpha\nBETA\ngamma"},
	} {
		t.Run(name, func(t *testing.T) {
			path := tempFile(t, foreignBody)
			code, out, errOut := runSub(t, strings.ReplaceAll(tc.spec, "%s", path))
			if code == 0 || content(t, path) != foreignBody {
				t.Fatalf("code %d, file %q: want a refusal and the file untouched", code, content(t, path))
			}
			msg := out + errOut
			want := "<<<<<<< SEARCH\n" + tc.old + "\n=======\n" + tc.repl + "\n>>>>>>> REPLACE"
			if !strings.Contains(msg, want) {
				t.Errorf("sample lacks the first block\nwant %q\nin %q", want, msg)
			}
			for _, control := range []string{"\nNEW\n", "\n+++", "\n@@", "\nOLD\n"} {
				if i := strings.Index(msg, "<<<<<<< SEARCH"); i >= 0 && strings.Contains(msg[i:], control) {
					t.Errorf("sample holds control line %q: %q", control, msg[i:])
				}
			}
		})
	}
}
