package shellcmd

import "testing"

func TestBuiltinUnknownOptionsAreFlags(t *testing.T) {
	for _, name := range []string{"command", "exec", "builtin"} {
		for _, option := range []string{"-x", "--zzz", "--zzz=value"} {
			input := name + " " + option + " kill 9"
			calls, err := Calls(input)
			if err != nil || !containsCallName(calls, "kill") {
				t.Errorf("%q: %v, %v", input, calls, err)
			}
		}
	}
}

func TestBuiltinOptionsKeepSignal(t *testing.T) {
	for _, input := range []string{
		`command -x kill 9`,
		`command --zzz kill 9`,
		`command --verbose kill 9`,
		`exec --zzz kill 9`,
		`exec -a synthetic kill 9`,
		`builtin --zzz kill 9`,
		`builtin -x kill 9`,
		`runuser -u x command -x kill 9`,
		"command --zzz bash <<'E'\nkill 9\nE",
		"cat <<'E' | command -x bash\nkill 9\nE",
		`echo 'kill 9' | nice command -x bash`,
		`strace --zzz v command --yyy kill 9`,
		`sudo --zzz v exec --yyy kill 9`,
		"nice --zzz v builtin --yyy bash -s <<'E'\nkill 9\nE",
	} {
		t.Run(input, func(t *testing.T) {
			calls, err := Calls(input)
			if err == nil && !containsCallName(calls, "kill") {
				t.Fatalf("signal lost: %v", calls)
			}
		})
	}
}

func TestBuiltinOptionsKeepCommandArguments(t *testing.T) {
	for _, input := range []string{
		`command --zzz echo kill 9`,
		`exec --zzz grep kill /var/log`,
		`builtin --zzz git log --grep kill`,
	} {
		calls, err := Calls(input)
		if err != nil || containsCallName(calls, "kill") {
			t.Errorf("%q: %v, %v", input, calls, err)
		}
	}
}
