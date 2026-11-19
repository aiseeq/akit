package shellcmd

import "testing"

func TestUnknownWrapperRoundSixSignals(t *testing.T) {
	cases := []string{
		`flock /tmp/f -c 'command -x kill 9'`,
		`SHELL=/bin/ksh flock /tmp/f -c 'command -x kill 9'`,
		`rksh93 -c 'command -x kill 9'`,
		`ssh host sh -c "'command -x kill 9'"`,
		"runuser x <<'E'\ncommand -x kill 9\nE",
		`f() { command -x kill 9; }; d='command() { "${@:2}"; }'; eval "$d"; f`,
		`f() { command -x kill 9; }; trap 'command() { "${@:2}"; }' DEBUG; f`,
		`f() { command -x bash <<<'kill 9'; }; d='command() { "${@:2}"; }'; eval "$d"; f`,
		`. /dev/stdin <<<'command() { "${@:2}"; }'; command -x kill 9`,
		`source <(echo 'command() { "${@:2}"; }'); command -x kill 9`,
		`d='command() { "${@:2}"; }'; bash -c "$d; command -x kill 9"`,
		`eval "$(echo 'command() { "${@:2}"; }')"; command -x kill 9`,
		`e=env; $e 'BASH_FUNC_command%%=() { "${@:2}"; }' bash -c 'command -x kill 9'`,
		`shopt -s expand_aliases; alias command=xargs; command -x kill 9`,
		`command() { :; }; command -x kill 9`,
		`e=source; $e /tmp/f; command -x kill 9`,
		`\export -f command; bash -c 'command -x kill 9'`,
		`sudo command -x kill 9`,
		"bash <<'E'\nf() { command -x kill 9; }; command() { \"${@:2}\"; }; f\nE",
	}
	for _, input := range cases {
		calls, err := Calls(input)
		if err == nil && !containsCallName(calls, "kill") {
			t.Errorf("signal lost in %q: %v", input, calls)
		}
	}
}

func TestUnknownWrapperRoundSixConservativeSignals(t *testing.T) {
	for _, input := range []string{
		"cat <<'E' | command -x bash\nkill 9\nE",
		"cat <<'E' | command -x sh\nkill 9\nE",
		`echo 'kill 9' | nice command -x bash`,
		`runuser -u x command -x kill 9`,
		`\command() { "${@:2}"; }; command -x kill 9`,
		`ksh -c 'bash -c "command -x kill 9"'`,
		`ksh -c 'sh -c "command -x kill 9"'`,
		`export -n command; command -x kill 9`,
		`export command; command -x kill 9`,
		`export "xf" command; command -x kill 9`,
		`echo 'command() { :; }'; command -x kill 9`,
	} {
		t.Run(input, func(t *testing.T) {
			calls, err := Calls(input)
			if err == nil && !containsCallName(calls, "kill") {
				t.Fatalf("signal lost: %v, %v", calls, err)
			}
		})
	}
}
