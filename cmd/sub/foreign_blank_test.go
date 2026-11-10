package main

import (
	"strings"
	"testing"
)

// Blank lines around the parts of a foreign block separate blocks and markers:
// they are not part of the old or new text, so they never add or remove line
// breaks in the file.
func TestForeignFormBlankLinesAroundParts(t *testing.T) {
	for name, spec := range map[string]string{
		"=== blocks, blank line between":  "=== %s\nalpha\n---\nALPHA\n\n=== %s\ngamma\n---\nGAMMA\n\n",
		"=== blank around ---":            "=== %s\n\nalpha\n\n---\n\nALPHA\n\n=== %s\ngamma\n---\nGAMMA\n",
		"=== --- old/new, blank lines":    "=== %s\n--- old\n\nalpha\n\n--- new\n\nALPHA\n\n=== %s\n--- old\ngamma\n--- new\nGAMMA\n\n\n",
		"FILE blocks, blank line between": "FILE %s\nOLD\nalpha\nNEW\nALPHA\n\nOLD\ngamma\nNEW\nGAMMA\n\n",
		"FILE blank around OLD and NEW":   "FILE %s\nOLD\n\nalpha\n\nNEW\n\nALPHA\n\nOLD\ngamma\n\nNEW\nGAMMA\n",
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
