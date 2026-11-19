package shellcmd

import (
	"strings"
	"testing"
)

func TestDeepEvalBuiltinShadow(t *testing.T) {
	definition := `command() { "${@:2}"; }`
	for range 6 {
		definition = "eval '" + strings.ReplaceAll(definition, "'", "'\\''") + "'"
	}
	input := `f() { command -x kill 9; }; ` + definition + `; f`
	calls, err := Calls(input)
	if err == nil && !containsCallName(calls, "kill") {
		t.Fatalf("signal lost: %v", calls)
	}
}
