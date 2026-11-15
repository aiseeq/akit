package shellcmd

import (
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestSetsidWrapperRouting(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"plain", []string{"kill", "-9", "-1"}, "kill"},
		{"fork", []string{"-f", "kill", "1"}, "kill"},
		{"wait", []string{"--wait", "echo", "x"}, "echo"},
		{"all flags", []string{"-cfw", "kill", "1"}, "kill"},
		{"long flags", []string{"--ctty", "--fork", "--wait", "kill", "1"}, "kill"},
		{"delimiter", []string{"--", "kill", "1"}, "kill"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := append([]string{"setsid"}, tc.args...)
			calls, err := Calls(strings.Join(command, " "))
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range calls {
				if call.Name == tc.want {
					if !slices.Contains(call.Via, "setsid") {
						t.Fatalf("kill Via = %q, missing setsid", call.Via)
					}
					return
				}
			}
			t.Fatalf("kill call missing: %#v", calls)
		})
	}
	for _, flag := range []string{"-h", "--help", "-V", "--version"} {
		calls, err := Calls("setsid " + flag + " kill 1")
		if err != nil {
			t.Fatal(err)
		}
		for _, call := range calls {
			if call.Name == "kill" {
				t.Errorf("setsid %s should not run its operand: %#v", flag, calls)
			}
		}
	}
	if wrapperOptionsKnown("setsid", []string{"-Z", "kill"}) {
		t.Fatal("unknown setsid option accepted")
	}
	for _, command := range []string{
		`timeout 1 setsid -- kill 1`,
		`setsid timeout 1 echo zk`,
		`sudo setsid -f echo zk`,
		`env A=1 setsid -w echo zk`,
		`setsid --fork env A=1 echo zk`,
	} {
		t.Run(command, func(t *testing.T) {
			calls, err := Calls(command)
			if err != nil {
				t.Fatal(err)
			}
			want := "echo"
			if strings.Contains(command, "kill 1") {
				want = "kill"
			}
			for _, call := range calls {
				if call.Name == want {
					if !slices.Contains(call.Via, "setsid") {
						t.Fatalf("%s Via = %q", want, call.Via)
					}
					return
				}
			}
			t.Fatalf("%s missing: %#v", want, calls)
		})
	}
}

func TestKshFishShellForms(t *testing.T) {
	for _, tc := range []struct {
		name, command, want string
		input, noExec       bool
	}{
		{"ksh -c", `ksh -c 'kill 1'`, "kill", false, false},
		{"mksh -c", `mksh -c 'kill 1'`, "kill", false, false},
		{"fish -c kill", `fish -c 'kill 1'`, "kill", false, false},
		{"fish -c", `fish -c 'zk 1'`, "zk", false, false},
		{"ksh cluster", `ksh -ic 'kill 1'`, "kill", false, false},
		{"mksh cluster", `mksh -ic 'kill 1'`, "kill", false, false},
		{"fish long command", `fish --command='zk 1'`, "zk", false, false},
		{"ksh stdin", "echo 'kill 1' | ksh", "kill", true, false},
		{"mksh stdin", "echo 'kill 1' | mksh", "kill", true, false},
		{"fish stdin", "echo 'zk 1' | fish", "zk", true, false},
		{"fish echo pipe", "echo zk | fish", "zk", true, false},
		{"ksh -s", "ksh -s arg <<'EOF'\nkill 1\nEOF", "kill", false, false},
		{"mksh -s", "mksh -s arg <<'EOF'\nkill 1\nEOF", "kill", false, false},
		{"fish no execute still parsed", `fish -n -c 'zk 1'`, "zk", false, false},
		{"ksh no execute still parsed", `ksh -n -c 'kill 1'`, "kill", false, false},
		{"mksh no execute still parsed", `mksh -n -c 'kill 1'`, "kill", false, false},
		{"fish unsupported -s", "fish -s <<'EOF'\nkill 1\nEOF", "", false, true},
		{"fish command and", `fish -c 'zk 1; and zk 2'`, "", false, false},
		{"fish substitution", `fish -c '(zk 1)'`, "", false, false},
		{"fish begin block", `fish -c 'begin; zk 1; end'`, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, err := Calls(tc.command)
			if tc.name == "fish command and" || tc.name == "fish substitution" || tc.name == "fish begin block" || tc.name == "fish unsupported -s" {
				if err == nil {
					t.Fatalf("fish-only syntax must be rejected; got calls %#v", calls)
				}
				var nested *NestedError
				if !errors.As(err, &nested) {
					t.Fatalf("want nested parse error, got %T: %v", err, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			found, input, killFound := false, false, false
			shellInputMarked := false
			for _, call := range calls {
				if call.Name == tc.want && tc.want != "" {
					found = true
				}
				if call.Name == "kill" {
					killFound = true
				}
				if call.ScriptInput && (call.Name == "ksh" || call.Name == "mksh" || call.Name == "fish") {
					input = true
				}
				if call.ScriptInput && (call.Name == "ksh" || call.Name == "mksh" || call.Name == "fish") {
					shellInputMarked = true
				}
			}
			if tc.want != "" && !found {
				t.Fatalf("%s call missing: %#v", tc.want, calls)
			}
			if tc.input && !input {
				t.Fatalf("ScriptInput = %t, want %t: %#v", input, tc.input, calls)
			}
			_ = killFound
			_ = shellInputMarked
		})
	}
}

func TestKshFishFishArgumentOptions(t *testing.T) {
	for _, tc := range []struct {
		arg                 string
		values              int
		command, noExec, ok bool
	}{
		{"-c", 1, true, false, true},
		{"-czk", 0, true, false, true},
		{"-C", 1, false, false, true},
		{"-Czk", 0, false, false, true},
		{"-d", 1, false, false, true},
		{"-dnone", 0, false, false, true},
		{"-o", 1, false, false, true},
		{"-pFILE", 0, false, false, true},
		{"-f", 1, false, false, true},
		{"--command", 1, true, false, true},
		{"--command=zk", 0, true, false, true},
		{"--no-execute", 0, false, false, true},
		{"--no-execute=yes", 0, false, false, false},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			_, values, command, noExec, ok := fishArgumentOption(tc.arg)
			if values != tc.values || command != tc.command || noExec != tc.noExec || ok != tc.ok {
				t.Fatalf("fishArgumentOption(%q) = (%d,%t,%t,%t), want (%d,%t,%t,%t)", tc.arg, values, command, noExec, ok, tc.values, tc.command, tc.noExec, tc.ok)
			}
		})
	}
	option, values, stdin, command, _ := shellArgumentOption("fish", "-s")
	if !option || values != 0 || stdin || command {
		t.Fatalf("fish -s shellArgumentOption = (%t,%d,%t,%t)", option, values, stdin, command)
	}
	option, values, stdin, command, noExec := shellArgumentOption("fish", "-dnone")
	if !option || values != 0 || stdin || command || noExec {
		t.Fatalf("fish -dnone shellArgumentOption = (%t,%d,%t,%t,%t)", option, values, stdin, command, noExec)
	}
	calls, err := Calls(`fish --command=zk`)
	if err != nil {
		t.Fatal(err)
	}
	if !containsCallName(calls, "zk") {
		t.Fatalf("fish --command did not expose script call: %#v", calls)
	}
	calls, err = Calls(`fish --no-execute -c zk`)
	if err != nil {
		t.Fatal(err)
	}
	if !containsCallName(calls, "zk") {
		t.Fatalf("fish --no-execute lost script call: %#v", calls)
	}
}

func TestKshFishOptionValueCounts(t *testing.T) {
	for _, tc := range []struct {
		arg    string
		values int
	}{
		{"-T", 1},
		{"-T!", 0},
	} {
		_, values, _, _, _ := kornArgumentOption("mksh", tc.arg)
		if values != tc.values {
			t.Errorf("mksh option %q takes %d values, want %d", tc.arg, values, tc.values)
		}
	}
	for _, arg := range []string{"--notify", "--errexit", "-eo"} {
		_, handled, valid := kornValueOption([]string{arg, "errexit"}, 0, "ksh")
		if !handled || valid {
			t.Errorf("kornValueOption(%q) = (%t,%t), want handled invalid", arg, handled, valid)
		}
	}
}

func TestKshFishEmptyCommandAndFishParseErrors(t *testing.T) {
	calls, err := Calls(`ksh -c '> /dev/null; zk'`)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, call := range calls {
		found = found || call.Name == "zk"
	}
	if !found {
		t.Fatalf("ksh command after empty redirection call missing: %#v", calls)
	}

	_, err = Calls(`fish -c 'if'`)
	var nested *NestedError
	if !errors.As(err, &nested) {
		t.Fatalf("fish syntax error must be reported as NestedError, got %v", err)
	}
	if !strings.Contains(err.Error(), "fish script outside the supported subset") {
		t.Fatalf("fish subset error was not reported: %v", err)
	}
}

func containsCallName(calls []Call, name string) bool {
	for _, call := range calls {
		if call.Name == name {
			return true
		}
	}
	return false
}

func TestKshFishAreShells(t *testing.T) {
	for _, name := range []string{"ksh", "mksh", "fish"} {
		if !IsShell(name) {
			t.Errorf("IsShell(%q) = false", name)
		}
	}
}

func TestKshFishBuiltinTableMatchesFish(t *testing.T) {
	path, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish is not installed")
	}
	output, err := exec.Command(path, "-c", "builtin -n").Output()
	if err != nil {
		t.Fatalf("fish builtin -n: %v", err)
	}
	got := strings.Fields(string(output))
	want := make([]string, 0, len(literalWordSet(fishBuiltinNames)))
	for name := range literalWordSet(fishBuiltinNames) {
		want = append(want, name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("fish builtin table differs: got %q want %q", got, want)
	}
}

func TestKshFishFunctionTableCoversFish(t *testing.T) {
	path, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish is not installed")
	}
	output, err := exec.Command(path, "--no-config", "-c", "functions -an").Output()
	if err != nil {
		t.Fatalf("fish functions -an: %v", err)
	}
	known := literalWordSet(fishFunctionNames)
	for name := range strings.FieldsSeq(string(output)) {
		if _, ok := known[name]; !ok {
			t.Errorf("fish function %q is missing from the conservative table", name)
		}
	}
}

func TestKshFishRound3SafetyForms(t *testing.T) {
	for _, source := range []string{
		`fish -c '1A=1 zk'`,
		`fish -c '9a=1'`,
		`fish -c '0_=1'`,
		`fish -c '1A=1 not zk'`,
		`fish <<< '1A=1 zk'`,
		`fish -c "echo 'a\'' ; zk ; echo 'b'"`,
		`fish -c "not zk"`,
		`fish -c "'not' zk"`,
		"fish -c 'echo hi\nnot zk'",
		"fish -c 'echo hi\rnot zk'",
		`fish -c 'echo "a\b"'`,
		`fish -c 'echo "a$b"'`,
		`fish -c 'echo "a(b"'`,
		`fish -c 'echo *'`,
		`fish -c 'echo # comment'`,
	} {
		t.Run(source, func(t *testing.T) {
			_, err := Calls(source)
			var nested *NestedError
			if !errors.As(err, &nested) {
				t.Fatalf("expected fish subset parse error, got %v", err)
			}
			if !strings.Contains(err.Error(), "fish script outside the supported subset") {
				t.Fatalf("unexpected parse error: %v", err)
			}
		})
	}
	for _, source := range []string{
		`ksh -c 'aliasx; xalias'`,
		`mksh -c 'aliasx; xalias'`,
	} {
		t.Run(source, func(t *testing.T) {
			if _, err := Calls(source); err != nil {
				t.Fatalf("ordinary command name should not be rejected: %v", err)
			}
		})
	}
	for _, source := range []string{
		`ksh --unknown -c zk`, `mksh --unknown -c zk`,
		`ksh -Z -c zk`, `mksh -Z -c zk`,
	} {
		t.Run(source, func(t *testing.T) {
			_, err := Calls(source)
			var nested *NestedError
			if !errors.As(err, &nested) {
				t.Fatalf("unknown Korn shell option must be rejected: %v", err)
			}
		})
	}
}

func TestKshFishRound3FishCharacterScanner(t *testing.T) {
	for _, source := range []string{
		`echo 'a\'' ; zk ; echo 'b'`,
		`echo 'a\b'`,
		`echo "a\b"`,
		`echo "a$b"`,
		`echo "a(b"`,
		"echo a\r",
		`echo *`,
		`echo # comment`,
		`echo 'unterminated`,
		`echo "unterminated`,
	} {
		if fishScriptCharactersSupported(source) {
			t.Errorf("fish character scanner accepted %q", source)
		}
	}
	for _, source := range []string{`echo hi`, `echo 'two words'`, `echo "two words"`, "echo hi\nzk"} {
		if !fishScriptCharactersSupported(source) {
			t.Errorf("fish character scanner rejected supported characters in %q", source)
		}
	}
}

func TestKshFishRound3KornAliasWordBoundaries(t *testing.T) {
	for _, source := range []string{"aliasx", "xalias", "alias_extra", "myalias", "x aliasx"} {
		if kornScriptUsesAlias(source) {
			t.Errorf("korn alias scanner matched non-word %q", source)
		}
	}
	for _, source := range []string{"alias", "command alias", "builtin alias", "x; alias y=zk"} {
		if !kornScriptUsesAlias(source) {
			t.Errorf("korn alias scanner missed %q", source)
		}
	}
}

func TestKshFishSupportedAndRejectedFishSubset(t *testing.T) {
	for _, tc := range []struct {
		source    string
		wantCalls []string
		wantError bool
	}{
		{`fish -c 'echo hi'`, []string{"echo"}, false},
		{`fish -c 'ls; pwd'`, []string{"ls", "pwd"}, false},
		{`fish -c 'git status'`, []string{"git"}, false},
		{`fish -c 'grep -r for .'`, []string{"grep"}, false},
		{`fish script.fish`, nil, false},
		{`fish -c 'ls | head'`, nil, true},
		{`fish -c 'set -l x 1'`, nil, true},
		{`fish -c 'A=1 set -l x 1'`, nil, true},
		{`fish -c 'not zk'`, nil, true},
		{`fish -c 'echo (zk)'`, nil, true},
		{`fish -c 'echo \\x6bill'`, nil, true},
		{`fish -c 'echo $x'`, nil, true},
		{`fish -c 'command alias x=zk'`, nil, true},
		{`ksh -c 'command alias x=zk'`, nil, true},
		{`mksh -c 'builtin alias x=zk'`, nil, true},
	} {
		t.Run(tc.source, func(t *testing.T) {
			calls, err := Calls(tc.source)
			var nested *NestedError
			if tc.wantError {
				if !errors.As(err, &nested) {
					t.Fatalf("want NestedError, calls=%#v err=%v", calls, err)
				}
				return
			}
			if err != nil {
				if errors.As(err, &nested) {
					t.Fatalf("unexpected NestedError: %v, script=%q", err, nested.Script)
				}
				t.Fatal(err)
			}
			var got []string
			for _, call := range calls {
				if slices.Contains(tc.wantCalls, call.Name) {
					got = append(got, call.Name)
				}
			}
			if !slices.Equal(got, tc.wantCalls) {
				t.Fatalf("calls=%#v, wanted %q", calls, tc.wantCalls)
			}
		})
	}
}

func TestKshFishInitCommandDoesNotHideScriptInput(t *testing.T) {
	calls, err := Calls("echo zk | fish -C zr")
	if err != nil {
		t.Fatal(err)
	}
	if !containsCallName(calls, "zk") || !containsCallName(calls, "zr") {
		t.Fatalf("fish -C lost stdin or init command: %#v", calls)
	}
}

func TestKshFishNestedShellContextUsesInnermostShell(t *testing.T) {
	var calls []Call
	err := parseNested("and zk", nil, 0, []string{"bash", "-c", "fish", "-c"}, false, false, map[string]string{}, &calls)
	var nested *NestedError
	if !errors.As(err, &nested) {
		t.Fatalf("fish nested under another shell must reject fish syntax: calls=%#v err=%v", calls, err)
	}
	if !strings.Contains(err.Error(), "fish script outside the supported subset") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestKshFishReviewerOrdinaryForms(t *testing.T) {
	for _, tc := range []struct {
		source    string
		wantError bool
		names     []string
	}{
		{`fish -c 'echo hi'`, false, []string{"echo"}},
		{`fish -c 'ls; pwd'`, false, []string{"ls", "pwd"}},
		{`fish script.fish`, false, nil},
		{`fish -c 'ls | head'`, true, nil},
		{`fish -c 'set -x A 1; env'`, true, nil},
		{`fish -c 'echo $HOME'`, true, nil},
		{`fish -c 'ls > /dev/null'`, true, nil},
		{`fish -c 'cd /tmp && ls'`, true, nil},
		{`fish -c 'if test -f x; echo y; end'`, true, nil},
		{`fish -c 'for f in *.txt; echo $f; end'`, true, nil},
		{`fish -c "grep -r 'for' ."`, false, []string{"grep"}},
		{`fish -c 'grep -r for .'`, false, []string{"grep"}},
		{`fish -c 'echo end'`, false, []string{"echo"}},
		{`fish -c 'echo (pwd)'`, true, nil},
		{`fish -c 'ls 2>/dev/null'`, true, nil},
		{`fish -c 'git status'`, false, []string{"git"}},
		{`fish -c "bash -c 'ls | wc -l'"`, false, []string{"bash"}},
		{`fish -c 'string split , a,b'`, false, []string{"string"}},
		{`fish -c 'echo "a b"'`, false, []string{"echo"}},
		{`fish -c "echo it\'s"`, true, nil},
		{`fish -c 'ls -la ~'`, true, nil},
		{`fish -c 'make test'`, false, []string{"make"}},
		{`fish -c 'echo $PATH[1]'`, true, nil},
		{`fish -c 'test -d /tmp; and echo y'`, true, nil},
		{`fish -c 'set -l x 1'`, true, nil},
		{`fish -c 'echo {a,b}'`, true, nil},
		{`fish -C 'set -g x 1' -c 'echo $x'`, true, nil},
		{`fish -l -c 'echo hi'`, false, []string{"echo"}},
		{`fish --no-config -c 'ls'`, false, []string{"ls"}},
		{`ksh -c 'ls | head'`, false, []string{"ls", "head"}},
		{`ksh -c '$CMD'`, true, nil},
		{`ksh -c '"$x" y'`, true, nil},
		{`ksh -c 'alias'`, true, nil},
		{`ksh -c 'a=$(date); echo $a'`, false, []string{"date", "echo"}},
	} {
		t.Run(tc.source, func(t *testing.T) {
			calls, err := Calls(tc.source)
			var nested *NestedError
			if tc.wantError {
				if !errors.As(err, &nested) {
					t.Fatalf("want NestedError, calls=%#v err=%v", calls, err)
				}
				return
			}
			if err != nil {
				if errors.As(err, &nested) {
					t.Fatalf("unexpected nested error %v, script=%q", err, nested.Script)
				}
				t.Fatal(err)
			}
			for _, name := range tc.names {
				if !containsCallName(calls, name) {
					t.Errorf("call %q missing: %#v", name, calls)
				}
			}
		})
	}
}

func TestKshFishKillCallsAndDynamicShellFalsePositives(t *testing.T) {
	for _, source := range []string{`fish -c 'kill 1'`, `fish -c 'kill -9 -1'`} {
		t.Run(source, func(t *testing.T) {
			calls, err := Calls(source)
			if err != nil {
				t.Fatal(err)
			}
			if !containsCallName(calls, "kill") {
				t.Fatalf("fish kill call missing: %#v", calls)
			}
		})
	}
	for _, source := range []string{
		`bash -c 'zk>$HOME/o'`,
		`bash -c '>$HOME/o zk'`,
		`bash -c 'f=/dev/null; zk>$f'`,
		"bash -c 'echo`zk`'",
		`bash -c 'x=1; y="a;b$x" zk'`,
		`setsid bash -c 'zk>$HOME/o'`,
		`dash -c 'zk>$HOME/o'`,
	} {
		t.Run(source, func(t *testing.T) {
			_, err := Calls(source)
			var nested *NestedError
			if errors.As(err, &nested) {
				t.Fatalf("dynamic shell content caused a generic parse error: %v", err)
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestKshFishReviewerRound2LossForms(t *testing.T) {
	forms := []string{
		"fish -c '\\x7a\\x6b'",
		"echo '\\x7a\\x6b' | fish",
		"fish -c '\\x7ak'",
		"echo '\\x7ak' | fish",
		"fish -c 'z\\x6b'",
		"echo 'z\\x6b' | fish",
		"fish -c '\\X7a\\X6b'",
		"echo '\\X7a\\X6b' | fish",
		"fish -c '\\172\\153'",
		"echo '\\172\\153' | fish",
		"fish -c '\\u007a\\u006b'",
		"echo '\\u007a\\u006b' | fish",
		"fish -c '\\U0000007a\\U0000006b'",
		"echo '\\U0000007a\\U0000006b' | fish",
		"fish -c '\\x6eot zk'",
		"echo '\\x6eot zk' | fish",
		"fish -c 'n\\x6ft zk'",
		"echo 'n\\x6ft zk' | fish",
		"fish -c '\\u006eot zk'",
		"echo '\\u006eot zk' | fish",
		"fish -c 'true; \\x61nd zk'",
		"echo 'true; \\x61nd zk' | fish",
		"fish -c 'false; \\x6fr zk'",
		"echo 'false; \\x6fr zk' | fish",
		"fish -c '\\x65val zk'",
		"echo '\\x65val zk' | fish",
		"fish -c 'alias a zk; a'",
		"echo 'alias a zk; a' | fish",
		"fish -c 'alias a=zk; a'",
		"echo 'alias a=zk; a' | fish",
		"fish -c 'alias a '\"'\"'zk'\"'\"'; a'",
		"echo 'alias a '\"'\"'zk'\"'\"'; a' | fish",
		"fish -c 'argparse '\"'\"'x=!zk'\"'\"' -- -x 1'",
		"echo 'argparse '\"'\"'x=!zk'\"'\"' -- -x 1' | fish",
		"echo 'not zk' | fish /dev/stdin",
		"fish /dev/stdin <<< '\\x7a\\x6b'",
		"echo '\\x7a\\x6b' | fish /dev/stdin",
		"fish /proc/self/fd/0 <<< '\\x7a\\x6b'",
		"fish -i <<< '\\x7a\\x6b'",
		"echo '\\x7a\\x6b' | fish -i",
		"exec -a x fish -c '\\x7a\\x6b'",
		"fish -C '\\x7a\\x6b' -c true",
		"fish -c true -C '\\x7a\\x6b'",
		"fish /dev/stdin <<< 'alias a zk; a'",
		"echo 'alias a zk; a' | fish /dev/stdin",
		"fish /proc/self/fd/0 <<< 'alias a zk; a'",
		"fish -i <<< 'alias a zk; a'",
		"echo 'alias a zk; a' | fish -i",
		"exec -a x fish -c 'alias a zk; a'",
		"fish -C 'alias a zk; a' -c true",
		"fish -c true -C 'alias a zk; a'",
		"echo 'trap zk EXIT' | fish /dev/stdin",
		"c=k fish -c 'z$c'",
		"fish -h -c zk </dev/null",
		"fish --he -c zk </dev/null",
		"fish -c zk extra -n </dev/null",
		"fish -c zk extra --no-execute </dev/null",
		"ksh -n -o exec -c zk </dev/null 2>/dev/null",
		"ksh -n --exec -c zk </dev/null 2>/dev/null",
		"ksh -n +o noex -c zk </dev/null 2>/dev/null",
		"ksh -n +o no_exec -c zk </dev/null 2>/dev/null",
		"ksh --notify -c zk </dev/null 2>/dev/null",
		"ksh --nounset -c zk </dev/null 2>/dev/null",
		"ksh --braceexpand -c zk </dev/null 2>/dev/null",
		"ksh --ignoreeof -c zk </dev/null 2>/dev/null",
		"ksh --interactive -c zk </dev/null 2>/dev/null",
		"ksh --noclobber -c zk </dev/null 2>/dev/null",
		"ksh -eo errexit -c zk </dev/null 2>/dev/null",
		"ksh -xo pipefail -c zk </dev/null 2>/dev/null",
		"ksh +eo noexec -c zk </dev/null 2>/dev/null",
		"ksh -o noexec -o exec -c zk </dev/null 2>/dev/null",
		"ksh -c 'command alias a=zk\na'",
		"mksh -eo errexit -c zk </dev/null 2>/dev/null",
		"mksh -xo pipefail -c zk </dev/null 2>/dev/null",
		"mksh +eo noexec -c zk </dev/null 2>/dev/null",
		"mksh -c 'command alias a=zk\na'",
		"mksh -c 'builtin alias a=zk\na'",
		"ksh93 -n -o exec -c zk </dev/null 2>/dev/null",
		"ksh93 -n --exec -c zk </dev/null 2>/dev/null",
		"ksh93 -n +o noex -c zk </dev/null 2>/dev/null",
		"ksh93 -n +o no_exec -c zk </dev/null 2>/dev/null",
		"ksh93 --notify -c zk </dev/null 2>/dev/null",
		"ksh93 --nounset -c zk </dev/null 2>/dev/null",
		"ksh93 --braceexpand -c zk </dev/null 2>/dev/null",
		"ksh93 --ignoreeof -c zk </dev/null 2>/dev/null",
		"ksh93 --interactive -c zk </dev/null 2>/dev/null",
		"ksh93 --noclobber -c zk </dev/null 2>/dev/null",
		"ksh93 -eo errexit -c zk </dev/null 2>/dev/null",
		"ksh93 -xo pipefail -c zk </dev/null 2>/dev/null",
		"ksh93 +eo noexec -c zk </dev/null 2>/dev/null",
		"ksh93 -o noexec -o exec -c zk </dev/null 2>/dev/null",
		"ksh93 -c 'command alias a=zk\na'",
		"lksh -eo errexit -c zk </dev/null 2>/dev/null",
		"lksh -xo pipefail -c zk </dev/null 2>/dev/null",
		"lksh +eo noexec -c zk </dev/null 2>/dev/null",
		"lksh -c 'command alias a=zk\na'",
		"lksh -c 'builtin alias a=zk\na'",
	}
	for _, form := range forms {
		t.Run(form, func(t *testing.T) {
			calls, err := Calls(form)
			var nested *NestedError
			if errors.As(err, &nested) {
				return
			}
			if err != nil {
				t.Fatalf("non-nested error: %v", err)
			}
			if !containsCallName(calls, "zk") {
				t.Fatalf("silent loss: calls=%#v", calls)
			}
		})
	}
}

func TestKshFishAttachedLongCommandArgument(t *testing.T) {
	if operand, command, _ := shellArgs("fish", []string{"--command=kill 1"}); operand != 0 || !command {
		t.Fatalf("fish --command= shell args = (%d, %t), want (0, true)", operand, command)
	}
	source := `fish --command='zk 1'`
	calls, err := Calls(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if call.Name == "zk" {
			if call.Offset != strings.Index(source, "zk") {
				t.Fatalf("attached script call offset = %d, want %d", call.Offset, strings.Index(source, "zk"))
			}
			return
		}
	}
	t.Fatal("attached --command script was not parsed")
}

func TestSetsidReviewForms(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{`setsid kill -9 -1`, "kill"},
		{`setsid --fork sh -c 'kill 1'`, "kill"},
		{`setsid --for zk`, "zk"},
		{`setsid --f zk`, "zk"},
		{`setsid --wai zk`, "zk"},
		{`setsid --w zk`, "zk"},
		{`setsid --ct zk`, "zk"},
		{`setsid $(echo zk)`, ""},
		{`X=zk; setsid $X`, ""},
	} {
		t.Run(tc.source, func(t *testing.T) {
			calls, err := Calls(tc.source)
			if tc.want == "" {
				var nested *NestedError
				if !errors.As(err, &nested) {
					t.Fatalf("want nested parse error; calls=%#v err=%v", calls, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range calls {
				if call.Name == tc.want {
					if !slices.Contains(call.Via, "setsid") {
						t.Fatalf("%s Via = %q", tc.want, call.Via)
					}
					return
				}
			}
			t.Fatalf("%s missing: %#v", tc.want, calls)
		})
	}
}

func TestKshFishReviewInvocationForms(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		want         []string
		wantError    bool
	}{
		{"fish not", `fish -c 'not zk'`, nil, true},
		{"fish begin", `fish -c 'begin; zk; end'`, nil, true},
		{"fish and", `fish -c 'and zk'`, nil, true},
		{"fish or", `fish -c 'or zk'`, nil, true},
		{"fish substitution", `fish -c 'echo (zk)'`, nil, true},
		{"fish command substitution", `fish -c 'echo $(zk)'`, nil, true},
		{"fish exec", `fish -c 'exec zk'`, nil, true},
		{"fish redirect", `fish -c 'zk >?/dev/null'`, nil, true},
		{"fish single quote escape", `fish -c "echo 'a\\''; zk"`, nil, true},
		{"fish source via pipe", `fish -c 'echo zk | source'`, nil, true},
		{"fish dot via pipe", `fish -c 'echo zk | .'`, nil, true},
		{"fish eval and", `fish -c 'eval "true; and zr"'`, nil, true},
		{"fish time not", `fish -c 'time not zk'`, nil, true},
		{"fish dynamic invocation", `fish -c zk$(echo)`, nil, true},
		{"fish brace expansion", `fish -c 'zk{,}'`, nil, true},
		{"fish set variable command", `fish -c 'set c zk; $c'`, nil, true},
		{"fish line continuation and", "fish -c 'zk; \\\nand zr'", nil, true},
		{"fish -C", `fish -C zk -c true`, []string{"zk"}, false},
		{"fish -CX", `fish -Czk -c true`, []string{"zk"}, false},
		{"fish multiple init", `fish -C zk -C zr -c true`, []string{"zk", "zr"}, false},
		{"fish init abbreviation", `fish --init-comm zk`, []string{"zk"}, false},
		{"fish init command alias", `fish --init-cmd zk`, []string{"zk"}, false},
		{"fish -iC", `fish -iC zk`, []string{"zk"}, false},
		{"fish -NC", `fish -NC zk`, []string{"zk"}, false},
		{"fish -PC", `fish -PCzk`, []string{"zk"}, false},
		{"fish multiple c", `fish -c zk -c zr`, []string{"zk", "zr"}, false},
		{"fish multiple long c", `fish --command=zk --command=zr`, []string{"zk", "zr"}, false},
		{"fish short attached", `fish -czk`, []string{"zk"}, false},
		{"fish short quoted attached", `fish -c'zk 1'`, []string{"zk"}, false},
		{"fish option cluster attached", `fish -Pczk`, []string{"zk"}, false},
		{"fish command abbreviation", `fish --comm zk`, []string{"zk"}, false},
		{"fish command abbreviation equals", `fish --co=zk`, []string{"zk"}, false},
		{"fish short command abbreviation", `fish --c zk`, []string{"zk"}, false},
		{"fish profile startup value", `fish --profile-startup /dev/null -c zk`, []string{"zk"}, false},
		{"fish value cluster p", `fish -ip /dev/null -c zk`, []string{"zk"}, false},
		{"fish value cluster d", `fish -id none -c zk`, []string{"zk"}, false},
		{"fish -d attached n", `fish -dnone -c zk`, []string{"zk"}, false},
		{"fish -n", `fish -n -c zk`, []string{"zk"}, false},
		{"fish -in executes", `fish -in -c zk`, []string{"zk"}, false},
		{"fish -ni executes", `fish -ni -c zk`, []string{"zk"}, false},
		{"fish -i -n executes", `fish -i -n -c zk`, []string{"zk"}, false},
		{"fish -n -i executes", `fish -n -i -c zk`, []string{"zk"}, false},
		{"fish help with command", `fish -h -c zk`, []string{"zk"}, false},
		{"fish unknown option", `fish --not-a-fish-option -c zk`, nil, true},
		{"fish unknown option after command", `fish -c zk extra --not-a-fish-option`, nil, true},
		{"fish stdin unsupported", "fish <<< 'not zk'", nil, true},
		{"fish heredoc unsupported", "fish <<'EOF'\nzk; and zr\nEOF", nil, true},
		{"fish pipe unsupported", "echo 'not zk' | fish", nil, true},
		{"ksh noexec reset", `ksh -n +n -c zk`, []string{"zk"}, false},
		{"mksh noexec reset", `mksh -n +n -c zk`, []string{"zk"}, false},
		{"ksh optional o ambiguous", `ksh -o -c zk`, nil, true},
		{"ksh attached o ambiguous", `ksh -oerrexit -c zk`, nil, true},
		{"mksh optional o ambiguous", `mksh -o -c zk`, nil, true},
		{"mksh attached o ambiguous", `mksh -oerrexit -c zk`, nil, true},
		{"mksh T value", `mksh -T - -c zk`, []string{"zk"}, false},
		{"lksh T is not supported", `lksh -T - -c zk`, nil, true},
		{"ksh dynamic command", `ksh -c zk$(echo)`, nil, true},
		{"ksh unknown long option", `ksh --not-a-ksh-option -c zk`, nil, true},
		{"mksh unknown long option", `mksh --not-a-mksh-option -c zk`, nil, true},
		{"mksh dynamic command", `mksh -c zk$(echo)`, nil, true},
		{"ksh dynamic stdin command", `echo 'zk$(echo)' | ksh`, nil, true},
		{"mksh dynamic stdin command", `echo 'zk$(echo)' | mksh`, nil, true},
		{"ksh alias", "ksh -c 'alias a=zk\na'", nil, true},
		{"mksh alias", "mksh -c 'alias a=zk\na'", nil, true},
		{"ksh93 name", `ksh93 -c zk`, []string{"zk"}, false},
		{"lksh name", `lksh -c zk`, []string{"zk"}, false},
		{"rksh name", `rksh -c zk`, []string{"zk"}, false},
		{"rksh93 name", `rksh93 -c zk`, []string{"zk"}, false},
		{"pdksh name", `pdksh -c zk`, []string{"zk"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, err := Calls(tc.source)
			if tc.wantError {
				var nested *NestedError
				if !errors.As(err, &nested) {
					t.Fatalf("want nested parse error; calls=%#v err=%v", calls, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, call := range calls {
				if call.Name == "zk" || call.Name == "zr" {
					got = append(got, call.Name)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("calls=%#v, want names %q", calls, tc.want)
			}
		})
	}
}

func TestKshFishPipeInputIsParsedOnlyFromLiteralEcho(t *testing.T) {
	for _, source := range []string{
		"cat f | fish",
		"bash f | fish",
		"X=zk; echo $X | fish",
		"echo $X | fish",
		"echo $(printf zk) | fish",
		"echo z$(true)k | fish",
		"fish -c \"'A'=1 zk\"",
		"echo zk >f | fish",
	} {
		t.Run(source, func(t *testing.T) {
			if calls, err := Calls(source); err == nil {
				t.Fatalf("expected parse error, calls %v", calls)
			}
		})
	}
	for _, source := range []string{"echo zr; echo zk | fish", "fish -c 'A=1 zk'", "fish -c 'A_1=x B=2 zk'"} {
		t.Run(source, func(t *testing.T) {
			calls, err := Calls(source)
			if err != nil {
				t.Fatal(err)
			}
			if !containsCallName(calls, "zk") {
				t.Fatalf("calls %v, want zk", calls)
			}
		})
	}
}
