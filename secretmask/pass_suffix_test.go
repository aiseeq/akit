package secretmask

import (
	"strings"
	"testing"
)

// "pass" closing a longer word (bypass, compass) is no password key; the key
// itself, options like --pass and --db-pass and the names db_pass and db-pass
// still hide their value. A hyphenated step name ending in -pass cannot be told
// from db-pass by its form, so it stays a key: a false mask costs less than a
// shown password.
func TestPassAsWordSuffixIsNoKey(t *testing.T) {
	for _, line := range []string{"bypass: grep -q z", "compass: grep -q w"} {
		if got, _ := Mask(line); got != line {
			t.Errorf("Mask(%q) = %q, want unchanged", line, got)
		}
	}
	for _, line := range []string{"password: hunter2xyz", "pass: hunter2xyz", `"pass": "hunter2xyz"`, "x pass=hunter2xyz", "db_pass: hunter2xyz", "DB_PASS=hunter2xyz", "tool --pass hunter2xyz", "tool --db-pass hunter2xyz", "db-pass: hunter2xyz"} {
		if got, _ := Mask(line); strings.Contains(got, "hunter2xyz") {
			t.Errorf("Mask(%q) = %q, want the value hidden", line, got)
		}
	}
}
