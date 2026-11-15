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

// The exact set of program names: after `--` sudo takes no more options or
// assignments, and a lone assignment is no shell option.
func TestSudoAssignmentCallNamesExact(t *testing.T) {
	for _, tc := range []struct {
		src   string
		names []string
	}{
		{`sudo X=1 -- Y=2 kill 1`, []string{"sudo", "Y=2"}},
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
