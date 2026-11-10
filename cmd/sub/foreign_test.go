package main

import (
	"strings"
	"testing"
)

const foreignBody = "alpha\nbeta\ngamma\n"

// Edit forms agents write instead of SEARCH/REPLACE that read one way only:
// sub applies them with the same exact-count check and shows its own form.
func TestForeignFormsApplied(t *testing.T) {
	for name, spec := range map[string]string{
		"FILE OLD NEW":          "FILE %s\nOLD\nbeta\nNEW\nBETA\n",
		"FILE SEARCH REPLACE":   "FILE %s\nSEARCH\nbeta\nREPLACE\nBETA\n",
		"=== path, ---":         "=== %s\nbeta\n---\nBETA\n",
		"=== path, --- old/new": "=== %s\n--- old\nbeta\n--- new\nBETA\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := tempFile(t, foreignBody)
			code, out, errOut := runSub(t, strings.ReplaceAll(spec, "%s", path))
			if code != 0 || content(t, path) != "alpha\nBETA\ngamma\n" {
				t.Fatalf("code %d, file %q, out %q, err %q", code, content(t, path), out, errOut)
			}
			if !strings.Contains(out+errOut, "<<<<<<< SEARCH") || !strings.Contains(out+errOut, ">>>>>>> REPLACE") {
				t.Errorf("no translation into SEARCH/REPLACE shown: out %q, err %q", out, errOut)
			}
		})
	}
}

// Two blocks of one form for one file, as agents write them.
func TestForeignFormSeveralBlocks(t *testing.T) {
	path := tempFile(t, foreignBody)
	code, out, errOut := runSub(t, "FILE "+path+"\nOLD\nalpha\nNEW\nALPHA\nOLD\ngamma\nNEW\nGAMMA\n")
	if code != 0 || content(t, path) != "ALPHA\nbeta\nGAMMA\n" {
		t.Fatalf("code %d, file %q, out %q, err %q", code, content(t, path), out, errOut)
	}
}

// The count check holds for a translated form: 0 or 2 matches change nothing.
func TestForeignFormCountCheck(t *testing.T) {
	for name, old := range map[string]string{"none": "delta", "two": "a"} {
		t.Run(name, func(t *testing.T) {
			path := tempFile(t, "a\nb\na\n")
			code, _, _ := runSub(t, "FILE "+path+"\nOLD\n"+old+"\nNEW\nX\n")
			if code == 0 || content(t, path) != "a\nb\na\n" {
				t.Errorf("code %d, file %q: want a refusal and the file untouched", code, content(t, path))
			}
		})
	}
}

// Forms with two readings (a unified-diff look, a separator that is also
// content) are refused: the file is untouched and the answer shows the right form.
func TestForeignFormsAmbiguousRefused(t *testing.T) {
	for name, spec := range map[string]string{
		"--- path +++":         "--- %s\nbeta\n+++\nBETA\n",
		"path --- +++":         "%s\n---\nbeta\n+++\nBETA\n",
		"=== path, two ---":    "=== %s\nbeta\n---\nx\n---\nBETA\n",
		"FILE, OLD in content": "FILE %s\nOLD\nOLD\nNEW\nBETA\n",
		"path, old === new":    "%s\nbeta\n===\nBETA\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := tempFile(t, foreignBody)
			code, out, errOut := runSub(t, strings.ReplaceAll(spec, "%s", path))
			if code == 0 || content(t, path) != foreignBody {
				t.Fatalf("code %d, file %q: want a refusal and the file untouched", code, content(t, path))
			}
			for _, want := range []string{"<<<<<<< SEARCH", "=======", ">>>>>>> REPLACE"} {
				if !strings.Contains(out+errOut, want) {
					t.Errorf("refusal has no sample of the right form (%q): %q", want, out+errOut)
				}
			}
		})
	}
}
