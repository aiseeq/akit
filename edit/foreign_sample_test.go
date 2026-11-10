package edit

import (
	"strings"
	"testing"
)

// The refusal sample of a unified diff takes the first hunk only: an empty
// context line stays a line, and the next file's header ends the hunk.
func TestUnifiedDiffSampleFirstHunk(t *testing.T) {
	for name, tc := range map[string]struct{ spec, want string }{
		"empty context line": {"--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n x\n\n-y\n+Y\n", "f\n<<<<<<< SEARCH\nx\n\ny\n=======\nx\n\nY\n>>>>>>> REPLACE"},
		"two files":          {"--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+A\n--- a/g\n+++ b/g\n@@ -1 +1 @@\n-b\n+B\n", "f\n<<<<<<< SEARCH\na\n=======\nA\n>>>>>>> REPLACE"},
		"git header":         {"diff --git a/f b/f\nindex 1234567..89abcde 100644\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+A\n", "f\n<<<<<<< SEARCH\na\n=======\nA\n>>>>>>> REPLACE"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSpec(strings.NewReader(tc.spec))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v\nwant sample %q", err, tc.want)
			}
		})
	}
}
