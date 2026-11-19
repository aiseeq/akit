package shellcmd

import "testing"

func TestUnknownWrapperRoundSevenSignals(t *testing.T) {
	inputs := []string{
		"alias command=xargs\ncommand -x kill 9",
		`builtin alias command=xargs; command -x kill 9`,
		`alias x=y; c''ommand -x kill 9`,
		`*; command -x kill 9`,
		`<(echo x); command -x kill 9`,
		`~user; command -x kill 9`,
		`alias exec=xargs; exec -x kill 9`,
		`alias builtin=xargs; builtin -x kill 9`,
		`command() { :; }; command -x kill 9`,
		`command( ) { :; }; command -x kill 9`,
		`exec() { :; }; exec -x kill 9`,
		`builtin() { :; }; builtin -x kill 9`,
		`. ./f; exec -x kill 9`,
		`source ./f; builtin -x kill 9`,
		`d=x; builtin eval "$d"; command -x kill 9`,
		`command . /tmp/f; command -x kill 9`,
		`enable -n command; command -x kill 9`,
		`declare -f f; command -x kill 9`,
		`builtin declare -f f; command -x kill 9`,
		`command export -F f; exec -x kill 9`,
		`typeset -F f; exec -x kill 9`,
		`export -f f; builtin -x kill 9`,
		`readonly -f f; command -x kill 9`,
		`f() { local -f g; command -x kill 9; }; f`,
		`BASH_ENV=/tmp/f bash -c 'command -x kill 9'`,
		`ENV=/tmp/f; command -x kill 9`,
		`env 'BASH_ENV=/tmp/f' bash -c 'command -x kill 9'`,
		`d=x; env "BASH_ENV=$d" bash -c 'command -x kill 9'`,
		`sudo BASH_ENV=/tmp/f bash -c 'command -x kill 9'`,
		`env ENV=/tmp/f bash -c 'command -x kill 9'`,
		`env 'BASH_FUNC_other%%=() { :; }' bash -c 'command -x kill 9'`,
		`echo BASH_''FUNC_other=x; command -x kill 9`,
		`d=x; env "BASH_FUNC_other%%=$d" bash -c 'command -x kill 9'`,
		`d=x; trap "$d" EXIT; command -x kill 9`,
		`d=x; bash -c "$d"; command -x kill 9`,
		"su -c 'cat <<E | command -x bash\nkill 9\nE'",
		`runuser -u x command kill 9`,
		`alias x=y; runuser -u x command -x kill 9`,
		`mksh -c 'command -x kill 9'`,
		`lksh -c 'command -x kill 9'`,
		`pdksh -c 'command -x kill 9'`,
		`command -x kill 9; bash -c 'alias x=y'`,
		`command -x kill 9; nice nice bash -c 'alias x=y'`,
		`command -x kill 9; nice --zzz v bash -c 'alias x=y'`,
		`command -x kill 9; nice --zzz v nice --yyy w bash -c 'alias x=y'`,
		`command -x kill 9; env -S 'alias x=y'`,
		`command -x kill 9; env -S 'alias x=y' echo hi`,
		`find /tmp -exec echo hi \; -exec alias x=y \;; command -x kill 9`,
		"command -x kill 9; cat <<'E'\nalias x=y\nE",
		`command -x kill 9; echo $(alias x=y)`,
	}
	for _, input := range inputs {
		calls, err := Calls(input)
		if err == nil && !containsCallName(calls, "kill") {
			t.Errorf("signal lost in %q: %v", input, calls)
		}
	}
}

func TestUnknownWrapperRoundSevenConservativeSignals(t *testing.T) {
	inputs := []string{
		`ssh host bash -c "'command -x kill 9'"`,
		`echo command=xargs; command -x kill 9`,
		`echo BASH_ENV=/tmp/f; command -x kill 9`,
		`env echo BASH_ENV=/tmp/f; command -x kill 9`,
		`nice echo BASH_ENV=/tmp/f; command -x kill 9`,
		`env echo alias; command -x kill 9`,
		`ssh host echo alias; command -x kill 9`,
		`find /tmp -name alias; command -x kill 9`,
		`'*'; command -x kill 9`,
		`nice nice bash -c 'echo hi'; command -x kill 9`,
		`bash -c 'bash -c "echo hi; ls ."'; command -x kill 9`,
		"shopt -s extglob\ncommand -x kill 9",
	}
	for _, input := range inputs {
		calls, err := Calls(input)
		if err == nil && !containsCallName(calls, "kill") {
			t.Errorf("signal lost in %q: %v, %v", input, calls, err)
		}
	}
}
