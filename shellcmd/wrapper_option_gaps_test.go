package shellcmd

import (
	"slices"
	"strings"
	"testing"
)

func TestGetoptWrapperOptionGapsKeepWrappedProgram(t *testing.T) {
	for _, tc := range []struct{ wrapper, src string }{
		{"timeout", `timeout -f 5 echo zk`},
		{"timeout", `timeout -p 5 echo zk`},
		{"nice", `nice --5 echo zk`},
		{"nice", `nice -+5 echo zk`},
		{"taskset", `taskset -c 0 echo zk`},
		{"taskset", `taskset --cpu-list 0 echo zk`},
		{"xargs", `xargs -eEND echo zk`},
		{"xargs", `xargs -e echo zk`},
		{"xargs", `xargs -i echo zk`},
		{"xargs", `xargs -i{} echo zk`},
		{"xargs", `xargs --replace echo zk`},
		{"xargs", `xargs -l echo zk`},
		{"xargs", `xargs -l2 echo zk`},
		{"xargs", `xargs --eof=END echo zk`},
		{"xargs", `xargs --eof echo zk`},
		{"xargs", `xargs --max-lines echo zk`},
		{"xargs", `xargs --max-lines=2 echo zk`},
		{"systemd-run", `systemd-run --background red echo zk`},
		{"systemd-run", `systemd-run --expand-environment no echo zk`},
		{"systemd-run", `systemd-run -r echo zk`},
		{"systemd-run", `systemd-run -C capsule echo zk`},
		{"systemd-run", `systemd-run --capsule=capsule echo zk`},
		{"systemd-run", `systemd-run --host host echo zk`},
		{"systemd-run", `systemd-run --tty echo zk`},
		{"systemd-run", `systemd-run --slice-inherit echo zk`},
		{"unshare", `unshare -c echo zk`},
		{"unshare", `unshare --map-current-user echo zk`},
		{"unshare", `unshare --map-subids echo zk`},
		{"strace", `strace -Y echo zk`},
		{"strace", `strace --decode-fds echo zk`},
		{"strace", `strace --decode-fds=path echo zk`},
		{"strace", `strace --attach 123456 echo zk`},
		{"strace", `strace --debug echo zk`},
		{"strace", `strace --failed-only echo zk`},
		{"strace", `strace --successful-only echo zk`},
		{"strace", `strace --kvm vcpu echo zk`},
		{"strace", `strace --namespace echo zk`},
		{"strace", `strace --output-append-mode echo zk`},
		{"strace", `strace --raw none echo zk`},
		{"env", `env -S 'echo zk'`},
		{"env", `env --split-string='echo zk'`},
		{"env", `env --s='echo zk'`},
		{"sudo", `sudo -N echo zk`},
	} {
		t.Run(tc.src, func(t *testing.T) {
			calls, err := Calls(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range calls {
				if call.Name == "echo" && slices.Contains(call.Via, tc.wrapper) {
					return
				}
			}
			t.Fatalf("calls %+v, want echo via %s", calls, tc.wrapper)
		})
	}
}

func TestWrapperOptionReviewRoundTwoForms(t *testing.T) {
	for _, tc := range []struct {
		src  string
		args []string
	}{
		{`taskset 2 kill -9 1 -cz`, []string{"-9", "1", "-cz"}},
		{`nice taskset 1 kill 1 -c0`, []string{"1", "-c0"}},
	} {
		t.Run(tc.src, func(t *testing.T) {
			calls, err := Calls(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range calls {
				if call.Name == "kill" && slices.Equal(call.Args, tc.args) && slices.Contains(call.Via, "taskset") {
					return
				}
			}
			t.Fatalf("calls %+v, want kill with args %q via taskset", calls, tc.args)
		})
	}
	for _, src := range []string{
		"taskset 1 pkill foo <<E\nkill 1\nE",
		"taskset -c 0 pkill -f x <<< 'kill 1'",
	} {
		t.Run(src, func(t *testing.T) {
			calls, err := Calls(src)
			if err != nil {
				t.Fatal(err)
			}
			if containsCallName(calls, "kill") || !hasCallVia(calls, "pkill", "taskset") {
				t.Fatalf("calls %+v, want [taskset pkill] and no kill", calls)
			}
		})
	}
	for _, src := range []string{
		`env -S kill ''`,
		`env -iS kill '' -ac`,
		`env -S pkill '' sh`,
		`sudo --split-string= -E <<'E'
kill 1
E`,
		`env xargs env -S pkill '' sh 'bash -c kill' chrt -s`,
		`command env -x --timeout -cS kill doas unshare ; 'kill 1'`,
	} {
		t.Run(src, func(t *testing.T) {
			calls, err := Calls(src)
			if err != nil {
				t.Fatal(err)
			}
			if !containsCallName(calls, "kill") && !containsCallName(calls, "pkill") {
				t.Fatalf("calls %+v, want kill from real target or stdin", calls)
			}
		})
	}
	calls, err := Calls(`echo x | taskset -c0 kill -execdir -o pkill script find -p --command=`)
	if err != nil {
		t.Fatal(err)
	}
	if !hasCallVia(calls, "kill", "taskset") {
		t.Fatalf("calls %+v, want legacy taskset fallback as on killguard base", calls)
	}
}

func TestWrapperNoCommandHelpModes(t *testing.T) {
	for _, src := range []string{`strace -h kill 1`, `strace -fh kill 1`, `chrt -m 0 kill 1`, `chrt --max 0 kill 1`, `chrt -vm 0 kill`, `strace -fV kill 1`, `setsid -wV kill 1`} {
		t.Run(src, func(t *testing.T) {
			calls, err := Calls(src)
			if err != nil {
				t.Fatal(err)
			}
			if containsCallName(calls, "kill") {
				t.Fatalf("calls %+v, help/query mode does not run a child", calls)
			}
		})
	}
}

func TestTasksetUnknownOptionsKeepMainBehavior(t *testing.T) {
	for _, src := range []string{
		`taskset -h 1 kill 1`,
		`taskset -V 1 kill 1`,
		`taskset --help 1 kill 1`,
		`taskset --version 1 kill 1`,
	} {
		t.Run(src, func(t *testing.T) {
			calls, err := Calls(src)
			if err != nil {
				t.Fatal(err)
			}
			if len(calls) != 1 || calls[0].Name != "taskset" || containsCallName(calls, "kill") {
				t.Fatalf("calls %+v, want only taskset as on main", calls)
			}
		})
	}
	calls, err := Calls(`echo 'kill 1' | taskset -h 1 bash`)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].Name != "echo" || calls[1].Name != "taskset" || calls[1].ScriptInput {
		t.Fatalf("calls %+v, want [echo taskset] without script input", calls)
	}
}

func TestTasksetPkillDoesNotReadScriptInput(t *testing.T) {
	for _, src := range []string{
		`taskset 1 pkill foo <&3`,
		`taskset 1 pkill foo < /dev/fd/3`,
		`taskset 1 pkill foo 0<&3`,
		`taskset 1 nice pkill foo <&3`,
	} {
		t.Run(src, func(t *testing.T) {
			calls, err := Calls(src)
			if err != nil {
				t.Fatal(err)
			}
			foundTaskset, foundPkill := false, false
			for _, call := range calls {
				if call.Name == "taskset" {
					foundTaskset = true
					if call.ScriptInput {
						t.Errorf("taskset call marked as script input: %+v", call)
					}
				}
				if call.Name == "pkill" {
					foundPkill = true
					if call.ScriptInput {
						t.Errorf("pkill call marked as script input: %+v", call)
					}
				}
			}
			if !foundTaskset || !foundPkill {
				t.Fatalf("calls %+v, want taskset and pkill calls", calls)
			}
		})
	}
}

func TestTasksetLegacyAttachedOptionsStayCompatible(t *testing.T) {
	calls, err := Calls("taskset -cS pkill -iS --cpu-list=0 -e <<E\nkill 1\nE")
	if err != nil {
		t.Fatal(err)
	}
	if containsCallName(calls, "kill") || !hasCallVia(calls, "pkill", "taskset") {
		t.Fatalf("calls %+v, want pkill via taskset without reading stdin", calls)
	}
	calls, err = Calls(`echo x | taskset -c0 kill -execdir -o pkill script find -p --command=`)
	if err != nil {
		t.Fatal(err)
	}
	if !hasCallVia(calls, "kill", "taskset") {
		t.Fatalf("calls %+v, want kill via legacy taskset fallback", calls)
	}
}

func TestGroupedWrapperExitAndValueOptions(t *testing.T) {
	for _, src := range []string{`strace -fV kill 1`, `setsid -wV kill 1`} {
		t.Run(src, func(t *testing.T) {
			calls, err := Calls(src)
			if err != nil {
				t.Fatal(err)
			}
			if containsCallName(calls, "kill") {
				t.Fatalf("calls %+v, grouped version must not run child", calls)
			}
		})
	}
	for _, tc := range []struct{ src, wrapper string }{
		{`strace -oh kill 1`, "strace"},
		{`strace -Ph kill 1`, "strace"},
		// Real flock rejects "h" as an exit code and runs nothing; reporting kill keeps the safe side.
		{`flock -Eh f kill 1`, "flock"},
	} {
		t.Run(tc.src, func(t *testing.T) {
			calls, err := Calls(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			if !hasCallVia(calls, "kill", tc.wrapper) {
				t.Fatalf("calls %+v, want kill after grouped value option", calls)
			}
		})
	}
}

func TestXargsOptionalEThenEnvSplitFindsKill(t *testing.T) {
	calls, err := Calls(`xargs -ezz env -S 'kill 1'`)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if call.Name == "kill" && slices.Contains(call.Via, "env") && slices.Contains(call.Via, "xargs") {
			return
		}
	}
	t.Fatalf("calls %+v, want kill through xargs env split", calls)
}

func TestReviewRoundThreeMutationForms(t *testing.T) {
	for _, src := range []string{
		`env --split-string ls kill`,
	} {
		t.Run(src, func(t *testing.T) {
			calls, err := Calls(src)
			if err != nil {
				t.Fatal(err)
			}
			if containsCallName(calls, "kill") || !containsCallName(calls, "ls") {
				t.Fatalf("calls %+v, want env ls, no kill", calls)
			}
		})
	}
	calls, err := Calls(`strace --colo never kill 1`)
	if err != nil {
		t.Fatal(err)
	}
	if !hasCallVia(calls, "kill", "strace") {
		t.Fatalf("calls %+v, want kill after abbreviated --color value", calls)
	}

	for _, src := range []string{
		`systemd-run -R kill 1`,
		`xargs -ea kill 1`,
		`xargs -iE kill 1`,
		`env -S '-i kill 1'`,
		`echo 'kill 1' | env --split-string bash`,
		`echo 'kill 1' | env -S 'X=1 bash'`,
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

func TestStraceColorConsumesSeparateArgument(t *testing.T) {
	calls, err := Calls(`strace --color never kill 1`)
	if err != nil {
		t.Fatal(err)
	}
	if !hasCallVia(calls, "kill", "strace") {
		t.Fatalf("calls %+v, want kill after the --color value", calls)
	}
}

func TestEnvSplitStringDoesNotTreatLaterKillAsProgram(t *testing.T) {
	for _, src := range []string{
		`env -S 'grep -r' kill src`,
		`env -S 'ls -l' kill`,
		`env -S ls kill`,
		`env -S ls -- kill`,
	} {
		t.Run(src, func(t *testing.T) {
			calls, err := Calls(src)
			if err != nil {
				t.Fatal(err)
			}
			if containsCallName(calls, "kill") {
				t.Fatalf("calls %+v, kill is an env split-string argument", calls)
			}
		})
	}
}

func TestReviewOptionalWrapperFormsKeepKillAtProgramPosition(t *testing.T) {
	for _, src := range []string{
		`xargs -ezz kill 1`,
		`xargs -e kill 1`,
		`xargs --eof=zz kill 1`,
		`xargs --eof kill 1`,
		`xargs -l1 kill 1`,
		`xargs -l kill 1`,
		`xargs --max-lines kill 1`,
		`xargs -izz kill 1`,
		`xargs -i kill 1`,
		`xargs --replace=zz kill 1`,
		`xargs --replace kill 1`,
		`xargs -rezz kill 1`,
		`xargs -rl1 kill 1`,
		`xargs -rizz kill 1`,
		`xargs -r -e kill 1`,
		`xargs -0e kill 1`,
		`xargs -el kill 1`,
		`xargs -e -r kill 1`,
		`xargs -l -r kill 1`,
		`xargs -i -r kill 1`,
		`strace --summary-columns time kill 1`,
		`strace --summary-columns=time kill 1`,
		`strace -U time kill 1`,
		`strace --summary-wall-clock kill 1`,
	} {
		t.Run(src, func(t *testing.T) {
			calls, err := Calls(src)
			if err != nil {
				t.Fatalf("Calls(%q): %v", src, err)
			}
			wrapper := strings.Fields(src)[0]
			if !hasCallVia(calls, "kill", wrapper) {
				t.Fatalf("calls %+v, want kill in the wrapped program position", calls)
			}
		})
	}
}

func TestStraceRemovedNonHelpOption(t *testing.T) {
	if valueOptions["strace"]["--qualify"] || flagOptions["strace"]["--qualify"] {
		t.Fatal("strace --qualify is not in this machine's --help")
	}
}

func TestRunuserRemovedNonHelpOption(t *testing.T) {
	if valueOptions["runuser"]["--groups"] || flagOptions["runuser"]["--groups"] {
		t.Fatal("runuser --groups is not in this machine's --help")
	}
}
