package shellcmd

import (
	"errors"
	"strings"
	"testing"
)

func TestUnknownWrapperRoundSixNestedBuiltinInput(t *testing.T) {
	cases := []struct {
		script string
		signal bool
	}{
		{`d='command() { "${@:2}"; }'; eval "$d"; nice command -x sh <<'E'` + "\nkill 9\nE", true},
		{`nice command -x sh <<'E'` + "\nkill 9\nE", true},
	}
	for _, tc := range cases {
		calls, err := Calls(tc.script)
		if err != nil || containsCallName(calls, "kill") != tc.signal {
			t.Errorf("%q: calls %v, error %v", tc.script, calls, err)
		}
	}
}

func TestRoundSixCommandScriptBudget(t *testing.T) {
	budget := newNestedParseBudget(100)
	budget.uncertainWork = budget.limit
	script := "nice --yyy v echo"
	args := []string{"--zzz", "v", "/tmp/f", "-c", script}
	words := make([]word, len(args)+1)
	words[len(words)-1] = word{script: script, pos: make([]int, len(script))}
	c := cmd{call: Call{Name: "flock", Args: args}, words: words, budget: budget}
	if err := uncertainCommandScripts(c, 0, nil); !errors.Is(err, errUncertainWrapperBudget) {
		t.Fatalf("nested budget error lost: %v", err)
	}
}

func TestRoundSixNestedOptionBoundaries(t *testing.T) {
	prefix := strings.Fields(strings.Repeat("nice ", maxNestedScripts))
	args := append(append([]string(nil), prefix[1:]...), "nice", "--zzz", "v", "echo")
	if !uncertainNestedBeyondLimit(prefix[0], args) {
		t.Error("unknown option at depth limit was not detected")
	}
	args = append(append([]string(nil), prefix[1:]...), "env", "-S", "echo", "nice", "--zzz", "v", "echo")
	if uncertainNestedBeyondLimit(prefix[0], args) {
		t.Error("arguments after env -S were mistaken for nested wrappers")
	}
	if uncertainNestedBeyondLimit("parallel", []string{":::", "::::"}) {
		t.Error("empty parallel command was treated as a wrapper target")
	}
}

func TestRoundSixParsedNestedFunctions(t *testing.T) {
	src := `f() { command -x kill 9; }; command() { :; }; f`
	budget := newNestedParseBudget(len(src))
	file, full, ignored, err := budget.parseShell(src)
	if err != nil {
		t.Fatal(err)
	}
	var calls []Call
	err = parseNestedFileWithBudget(file, full, src, nil, 0, []string{"bash"}, false, false, map[string]string{}, &calls, ignored, nil, budget)
	if err != nil || !containsCallName(calls, "kill") {
		t.Fatalf("nested definition lost: %v, %v", calls, err)
	}
}
