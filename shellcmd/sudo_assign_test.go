package shellcmd

import (
	"slices"
	"testing"
)

// sudo takes VAR=value words between its options as environment for the command:
// a word with '=' after its first byte is an assignment, options may follow it,
// and after `--` the next word is the command even when it contains '='.
func TestSudoAssignmentsAreNotTheCommand(t *testing.T) {
	for _, tc := range []struct {
		src  string
		name string
		args []string
	}{
		{`sudo X=1 kill -9 -1`, "kill", []string{"-9", "-1"}},
		{`sudo -u root X=1 kill -9 -1`, "kill", []string{"-9", "-1"}},
		{`sudo -E X=1 graft deploy prod`, "graft", []string{"deploy", "prod"}},
		{`sudo X=/tmp/c graft deploy prod`, "graft", []string{"deploy", "prod"}},
		{`sudo X=1 Y=2 kill 1`, "kill", []string{"1"}},
		{`sudo 1A=1 A-B=1 kill 1`, "kill", []string{"1"}},
		{`sudo X=1 -u root kill 1`, "kill", []string{"1"}},
		{`sudo X=1 -u root Y=2 -- kill 1`, "kill", []string{"1"}},
		{`sudo X=s kill 1`, "kill", []string{"1"}},
		{`sudo X=1 env Y=2 kill 1`, "kill", []string{"1"}},
		{`nice sudo X=1 kill 1`, "kill", []string{"1"}},
		{`sudo -- X=1 kill 1`, "X=1", []string{"kill", "1"}},
		{`sudo X=1 -- Y=2 kill 1`, "Y=2", []string{"kill", "1"}},
		{`sudo =1 kill 1`, "=1", []string{"kill", "1"}},
	} {
		t.Run(tc.src, func(t *testing.T) {
			calls, err := Calls(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range calls {
				if c.Name == tc.name && slices.Equal(c.Args, tc.args) && slices.Contains(c.Via, "sudo") {
					return
				}
			}
			t.Fatalf("calls %+v, want %s %v via sudo", calls, tc.name, tc.args)
		})
	}
}

func TestSudoAssignmentWordIsNoCall(t *testing.T) {
	for _, src := range []string{`sudo X=1 kill 1`, `sudo X=/tmp/c kill 1`, `sudo -u root X=1 Y=/a/b kill 1`} {
		t.Run(src, func(t *testing.T) {
			calls, err := Calls(src)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range calls {
				if c.Name == "X=1" || c.Name == "c" || c.Name == "b" || c.Name == "Y=/a/b" {
					t.Fatalf("assignment taken for a call: %+v", calls)
				}
			}
		})
	}
}

// Options after an assignment are still sudo options: -s and -i start a shell
// that reads the script from stdin, and an unknown option is refused.
func TestSudoAssignmentBeforeShellOption(t *testing.T) {
	for _, src := range []string{
		"echo 'zk 1' | sudo X=1 -s",
		"sudo X=1 -i <<'E'\nzk 1\nE",
		"sudo X=1 -s <<< 'zk 1'",
	} {
		t.Run(src, func(t *testing.T) {
			calls, err := Calls(src)
			if err != nil {
				t.Fatal(err)
			}
			if !containsCallName(calls, "zk") {
				t.Fatalf("calls %+v, want zk", calls)
			}
		})
	}
	calls, err := Calls(`sudo X=1 --no-such-option kill 1`)
	if err == nil && containsCallName(calls, "--no-such-option") {
		t.Fatalf("unknown option after assignment taken for the command: %+v", calls)
	}
}

// GNU env ends its options at the first assignment: a later -i or -u is the command.
func TestEnvOptionsEndAtAssignment(t *testing.T) {
	for _, tc := range []struct {
		src, name string
	}{
		{`env -i A=1 kill 1`, "kill"},
		{`env A=1 -u X kill 1`, "-u"},
		{`env A=1 -i kill 1`, "-i"},
	} {
		t.Run(tc.src, func(t *testing.T) {
			calls, err := Calls(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			if !containsCallName(calls, tc.name) {
				t.Fatalf("calls %+v, want %s", calls, tc.name)
			}
		})
	}
}

// The exact set of program names retains the command sudo runs after `--` and
// adds a protective route to the program behind an assignment word.
func TestSudoAssignmentCallNamesExact(t *testing.T) {
	for _, tc := range []struct {
		src   string
		names []string
	}{
		{`sudo X=1 -- Y=2 kill 1`, []string{"sudo", "Y=2", "kill"}},
		{`sudo -- X=1 kill -9 -1`, []string{"sudo", "X=1", "kill"}},
		{`sudo -- -u kill 1`, []string{"sudo", "-u"}},
		{"echo 'zk 1' | sudo X=s", []string{"echo", "sudo"}},
		{"echo 'zk 1' | sudo X=i", []string{"echo", "sudo"}},
		{"echo 'zk 1' | sudo -- -s", []string{"echo", "sudo", "-s"}},
		{`sudo X=1 setsid kill 1`, []string{"sudo", "setsid", "kill"}},
		{`sudo -u root X=1 taskset -c 1 kill 1`, []string{"sudo", "taskset", "kill"}},
		{`env A=1 -i kill 1`, []string{"env", "-i"}},
		{`env - A=1 - kill 1`, []string{"env", "-"}},
	} {
		t.Run(tc.src, func(t *testing.T) {
			calls, err := Calls(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, c := range calls {
				if !slices.Contains(names, c.Name) {
					names = append(names, c.Name)
				}
			}
			slices.Sort(names)
			want := slices.Clone(tc.names)
			slices.Sort(want)
			if !slices.Equal(names, want) {
				t.Fatalf("names %v, want %v (calls %+v)", names, want, calls)
			}
		})
	}
}

// A shell behind sudo assignments reads its script from stdin; a word that starts
// with / is a path to the command for sudo even when it contains =.
func TestSudoAssignmentBeforeStdinShell(t *testing.T) {
	for _, src := range []string{
		"echo 'kill 1' | sudo X=1 bash",
		"echo 'kill 1' | sudo -u root X=1 nice -n 5 bash",
		"echo 'kill 1' | sudo X=1 sudo -s",
	} {
		t.Run(src, func(t *testing.T) {
			calls, err := Calls(src)
			if err != nil {
				t.Fatal(err)
			}
			if !containsCallName(calls, "kill") {
				t.Fatalf("calls %+v, want kill", calls)
			}
		})
	}
}

func TestLegacyNiceAdjustmentKeepsStdinShell(t *testing.T) {
	calls, err := Calls("echo 'kill 1' | nice -5 sudo X=1 bash")
	if err != nil {
		t.Fatal(err)
	}
	if !containsCallName(calls, "kill") {
		t.Fatalf("calls %+v, want kill from stdin shell", calls)
	}
}

func TestChrtVerboseAndFlockExclusiveOptions(t *testing.T) {
	for _, src := range []string{
		`chrt -v -f 10 kill 1`,
		`chrt -v -f 10 sudo X=1 kill 1`,
	} {
		calls, err := Calls(src)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if !containsCallName(calls, "kill") {
			t.Errorf("%s: calls %+v, want kill", src, calls)
		}
	}
	calls, err := Calls(`flock -e /dev/null echo zk`)
	if err != nil {
		t.Fatal(err)
	}
	if !containsCallName(calls, "echo") {
		t.Fatalf("flock -e calls %+v, want echo", calls)
	}
}

func TestXargsOptionalArgumentsKeepWrappedCommand(t *testing.T) {
	for _, src := range []string{
		`xargs -l echo zk`,
		`xargs -l2 echo zk`,
		`xargs -i echo zk`,
		`xargs -i{} echo zk`,
		`xargs --eof echo zk`,
	} {
		calls, err := Calls(src)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if !hasCallVia(calls, "echo", "xargs") {
			t.Errorf("%s: calls %+v, want echo via xargs", src, calls)
		}
	}
}

func TestSudoSlashWordIsCommand(t *testing.T) {
	for _, tc := range []struct{ src, name string }{
		{`sudo -u root /opt/x=1/kill -9 1`, "kill"},
		{`sudo /opt/a=1/bash -c 'kill 1'`, "kill"},
		{"sudo /opt/a=1/bash <<'E'\nkill 1\nE", "kill"},
		{`sudo /usr/local/bin/kill=1 -9 -1`, "kill=1"},
	} {
		t.Run(tc.src, func(t *testing.T) {
			calls, err := Calls(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			if !containsCallName(calls, tc.name) {
				t.Fatalf("calls %+v, want %s", calls, tc.name)
			}
		})
	}
}

func TestEnvEqualsFirstWordIsAssignment(t *testing.T) {
	calls, err := Calls(`env =1 kill 1`)
	if err != nil {
		t.Fatal(err)
	}
	if !containsCallName(calls, "kill") {
		t.Fatalf("calls %+v, want kill", calls)
	}
}

// After `--` the program behind VAR=value words is an additional protective
// route; the actual VAR=value command remains represented as well.
func TestSudoDashDashAssignmentKeepsProgram(t *testing.T) {
	for _, tc := range []struct {
		src  string
		args []string
	}{
		{`sudo -- X=1 kill -9 1`, []string{"-9", "1"}},
		{`sudo -u root -- X=a/b kill 1`, []string{"1"}},
		{`sudo -E -- A=1 B=2 kill 1`, []string{"1"}},
		{`sudo X=1 -- Y=2 kill 1`, []string{"1"}},
	} {
		t.Run(tc.src, func(t *testing.T) {
			calls, err := Calls(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range calls {
				if c.Name == "kill" && slices.Equal(c.Args, tc.args) && slices.Contains(c.Via, "sudo") {
					return
				}
			}
			t.Fatalf("calls %+v, want kill %v via sudo", calls, tc.args)
		})
	}
}

func TestSudoDashDashAssignmentRoutes(t *testing.T) {
	for _, src := range []string{
		`nice sudo -- X=1 kill -9 1`,
		`timeout 5 sudo -- X=1 kill -9 1`,
		`env sudo -- X=1 kill -9 1`,
		`exec sudo -- X=1 kill -9 1`,
		`command sudo -- X=1 kill -9 1`,
		`nohup sudo -- X=1 kill -9 1`,
		`setsid sudo -- X=1 kill -9 1`,
		`bash -c 'timeout 5 sudo -- X=1 kill -9 1'`,
		`sudo -- X=1 bash <<< 'kill -9 1'`,
		`sudo -- X=1 bash 0<<< 'kill -9 1'`,
		`sudo -- X=1 sh -s <<< 'kill -9 1'`,
		`echo 'kill -9 1' | sudo -- X=1 bash`,
		`echo 'kill -9 1' | sudo -u root -- X=1 bash`,
		"sudo -- X=1 bash <<'EOF'\nkill -9 1\nEOF",
	} {
		t.Run(src, func(t *testing.T) {
			calls, err := Calls(src)
			if err != nil {
				return
			}
			for _, c := range calls {
				if c.Name == "kill" && slices.Contains(c.Via, "sudo") {
					return
				}
			}
			t.Fatalf("calls %+v, want kill via sudo or parse error", calls)
		})
	}
}

func TestSudoDashDashVariableNameMatchesWithoutDashDash(t *testing.T) {
	for _, tc := range []struct{ withDash, withoutDash string }{
		{`K=kill; sudo -- X=1 $K -9 1`, `K=kill; sudo X=1 $K -9 1`},
		{`a=(kill); sudo -- X=1 "${a[@]}" -9 1`, `a=(kill); sudo X=1 "${a[@]}" -9 1`},
	} {
		withCalls, withErr := Calls(tc.withDash)
		withoutCalls, withoutErr := Calls(tc.withoutDash)
		if (withErr != nil) != (withoutErr != nil) || !sameNamedCalls(withCalls, withoutCalls, "kill") {
			t.Fatalf("with --: calls=%+v err=%v; without --: calls=%+v err=%v", withCalls, withErr, withoutCalls, withoutErr)
		}
	}
}

func sameNamedCalls(left, right []Call, name string) bool {
	var l, r []Call
	for _, c := range left {
		if c.Name == name {
			l = append(l, c)
		}
	}
	for _, c := range right {
		if c.Name == name {
			r = append(r, c)
		}
	}
	return slices.EqualFunc(l, r, func(a, b Call) bool {
		return slices.Equal(a.Args, b.Args) && slices.Equal(a.Via, b.Via)
	}) && len(l) == len(r)
}
