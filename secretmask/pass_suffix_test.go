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

// "pass:" inside a sentence before a plain word ("the first binding pass: read it
// through") and before a Russian word is prose, not a password; a config key, an
// assignment and an option keep their value hidden.
func TestPassInProseIsNoKey(t *testing.T) {
	for _, line := range []string{
		"- `Plasmoid.configuration.*` is empty on the first binding pass: read it through properties with a fallback, or the sensors bind to `undefined` for good (0 °C).",
		"(id в обратных кавычках: без них маска секретов прячет «…-pass: слово» как пароль)",
		"the second pass: check every link",
	} {
		if got, _ := Mask(line); got != line {
			t.Errorf("Mask(%q) = %q, want unchanged", line, got)
		}
	}
	for _, line := range []string{
		"pass: hunter2xY9", "  pass: hunter2xY9", "db:\n  pass: hunter\n", "DB_PASS: hunter2xY9", "db_pass: hunter words", "first pass: hunter2xY9 here",
		"x pass=hunter rest", "tool --pass hunter more", "pass: hunter",
		"pass: hunter ok", "x pass: hunter", "x pass:hunter ok", `x pass: "hunter" ok`, "x Pass: hunter ok", "x pass:  hunter ok", "сеть pass: hunter ok", "- pass: hunter ok", "user: root, pass: hunter ok", "| root | pass: hunter ok |",
	} {
		if got, _ := Mask(line); strings.Contains(got, "hunter") {
			t.Errorf("Mask(%q) = %q, want the value hidden", line, got)
		}
	}
}

// A Russian value after pass is prose only in a sentence and only as letters:
// a config key, a list item, a table cell and a word with digits stay hidden.
func TestPassRussianValueHiddenOutsideProse(t *testing.T) {
	for _, line := range []string{
		"pass: Москва2024", "  pass: Москва2024", "PASS: Москва2024", "- pass: Москва2024", "--pass: Москва2024", "pass: секрет",
		"| root | pass: Москва2024 |", "user: root, pass: Москва", "db:\n  user: root\n  pass: Москва2024\n  host: x\n",
		"x pass: Москва2024 ok", `x pass: "Москва" ok`, "пароль от сети pass: Москва2024", "user: admin pass: Москва",
	} {
		if got, _ := Mask(line); strings.Contains(got, "Москва") || strings.Contains(got, "секрет") {
			t.Errorf("Mask(%q) = %q, want the value hidden", line, got)
		}
	}
}
