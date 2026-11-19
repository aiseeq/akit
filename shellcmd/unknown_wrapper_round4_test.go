package shellcmd

import (
	"errors"
	"strings"
	"testing"
)

func TestUnknownWrapperRoundFourSignals(t *testing.T) {
	cases := []string{
		`ksh -c 'command -x kill 9'`,
		`ksh93 -c 'command -x kill 9'`,
		`rksh -c 'command -x kill 9'`,
		`echo 'command -x kill 9' | ksh`,
		"ksh <<'E'\ncommand -x kill 9\nE",
		`ksh -c 'command -px kill 9'`,
		`ksh -c 'command -x pkill -9 init'`,
		`command() { "${@:2}"; }; command -x kill 9`,
		`exec() { "${@:2}"; }; exec -x kill 9`,
		`builtin() { "${@:2}"; }; builtin -x kill 9`,
		`nice --zzz v nice --yyy w kill -9 -1`,
		`strace --zzz v timeout --yyy w 5 kill -9 -1`,
		`flock --zzz v /var/tmp/f -c 'nice --yyy w kill -9 -1'`,
		`su --zzz root -c 'nice --yyy w kill -9 -1'`,
		`runuser --zzz root -c 'nice --yyy w kill -9 -1'`,
		`cat f | xargs --zzz v kill`,
		`true x | xargs --zzz v kill`,
		strings.Repeat("nice --zzz v ", 33) + "kill -9 -1",
		strings.Repeat("nice --zzz v ", 60) + "kill -9 -1",
		"time --zzz " + strings.Repeat("nice --zzz v ", 60) + "kill -9 -1",
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			calls, err := Calls(input)
			if err == nil && !containsCallName(calls, "kill") && !containsCallName(calls, "pkill") {
				t.Fatalf("lost signal: %v", calls)
			}
		})
	}
}

func TestUnknownWrapperRoundFourBenign(t *testing.T) {
	for _, input := range []string{
		`xargs --show-limits grep kill < f`,
		`su --zzz root < <(echo 'kill -9 -1'; echo b)`,
		`nice --zzz v kill --table 5`,
		`nice --zzz v kill --timeout 100 -1`,
	} {
		t.Run(input, func(t *testing.T) {
			calls, err := Calls(input)
			if err != nil || containsCallName(calls, "kill") {
				t.Fatalf("ordinary command: %v, %v", calls, err)
			}
		})
	}
}

func TestUnknownWrapperTimeKnownFlags(t *testing.T) {
	for _, option := range []string{"-a", "-v"} {
		calls, err := Calls("/usr/bin/time " + option + " kill -9 -1")
		if err != nil || !containsCallName(calls, "kill") {
			t.Errorf("time %s: %v, %v", option, calls, err)
		}
	}
}

func TestUnknownWrapperRoundFourBudget(t *testing.T) {
	budget := newNestedParseBudget(100)
	budget.uncertainWork = budget.limit - 1
	args := []string{"--zzz=value", "echo"}
	c := cmd{call: Call{Name: "nice", Args: args}, words: make([]word, len(args)+1), budget: budget}
	if err := emitUncertainWrapper(c, 0, nil); err != nil {
		t.Fatalf("exact budget boundary: %v", err)
	}
	budget.uncertainWork = budget.limit - 1
	args = []string{"--zzz=value", "nice", "--yyy=value", "echo"}
	c = cmd{call: Call{Name: "nice", Args: args}, words: make([]word, len(args)+1), budget: budget}
	if err := emitUncertainWrapper(c, 0, nil); !errors.Is(err, errUncertainWrapperBudget) {
		t.Fatalf("nested budget error lost: %v", err)
	}
	budget = newNestedParseBudget(100)
	budget.uncertainWork = budget.limit
	src := "time --zzz nice --yyy v echo"
	file, _, err := parseShell(src)
	if err != nil {
		t.Fatal(err)
	}
	if err = checkUncertainTime(file, src, 0, nil, nil, budget); !errors.Is(err, errUncertainWrapperBudget) {
		t.Fatalf("time budget error lost: %v", err)
	}
	budget = newNestedParseBudget(100)
	budget.uncertainWork = budget.limit
	c = cmd{call: Call{Redirs: []Redir{{Op: "<", Target: `<(echo 'nice --zzz v echo')`}}}, budget: budget}
	if _, err = processSubstitutionSignal(c, 0, nil); !errors.Is(err, errUncertainWrapperBudget) {
		t.Fatalf("process substitution budget error lost: %v", err)
	}
}

func TestUnknownWrapperRoundFourShellOffset(t *testing.T) {
	name, _, offset, ok := shellInputNextTarget("nice", []string{"--zzz", "v", "bash"}, 4)
	if !ok || name != "bash" || offset != 7 {
		t.Fatalf("shell target: %s offset %d, found %t", name, offset, ok)
	}
}
