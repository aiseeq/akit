package shellcmd

import (
	"strings"
	"testing"
)

func TestUnknownWrapperRoundFiveSignals(t *testing.T) {
	cases := []string{
		`su -s ksh root -c 'command -x kill 9'`,
		`su -s /bin/ksh root -c 'command -x kill 9'`,
		"su -s /bin/ksh root <<'E'\ncommand -x kill 9\nE",
		`runuser -s /bin/ksh -u x -c 'command -x kill 9'`,
		`runuser -s /bin/ksh x -c 'command -x kill 9'`,
		"SHELL=/bin/ksh sudo -s <<'E'\ncommand -x kill 9\nE",
		`SHELL=/bin/ksh script -c 'command -x kill 9'`,
		`su root -c 'command -x kill 9'`,
		`su -c 'command -x kill 9'`,
		"sudo -s <<'E'\ncommand -x kill 9\nE",
		`ssh host 'command -x kill 9'`,
		`ssh host command -x kill 9`,
		`tmux new 'command -x kill 9'`,
		`script -c 'command -x kill 9'`,
		`f() { command -x kill 9; }; command() { "${@:2}"; }; f`,
		`for i in 1 2; do [ $i = 2 ] && command -x kill 9; command() { "${@:2}"; }; done`,
		`trap 'command -x kill 9' EXIT; command() { "${@:2}"; }`,
		`eval 'command() { "${@:2}"; }'; command -x kill 9`,
		`command() { "${@:2}"; }; eval 'command -x kill 9'`,
		"command() { \"${@:2}\"; }; command -x sh <<'E'\nkill 9\nE",
		`export -f command; bash -c 'command -x kill 9'`,
		`env 'BASH_FUNC_command%%=() { echo; }' bash -c 'command -x kill 9'`,
		`ksh -c "echo 'kill 9' | command -x bash"`,
		"ksh -c \"command -x bash <<'E'\nkill 9\nE\"",
		`command() { "${@:2}"; }; eval "echo 'kill 9' | command -x bash"`,
		strings.Repeat("nice ", 33) + "nice --zzz v kill 9",
		strings.Repeat("nice ", 32) + "nice --zzz v kill 9",
		"su --zzz root < <(echo '" + strings.Repeat("nice --zzz v ", 33) + "kill 9')",
		"flock --zzz v /tmp/f -c '" + strings.Repeat("nice --zzz v ", 33) + "kill 9'",
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			calls, err := Calls(input)
			if err == nil && !containsCallName(calls, "kill") {
				t.Fatalf("signal lost: %v", calls)
			}
		})
	}
}

func TestUnknownWrapperRoundFiveBenign(t *testing.T) {
	for _, input := range []string{`su --zzz root < <(x=1)`} {
		calls, err := Calls(input)
		if err != nil || containsCallName(calls, "kill") {
			t.Errorf("%s: %v, %v", input, calls, err)
		}
	}
}
