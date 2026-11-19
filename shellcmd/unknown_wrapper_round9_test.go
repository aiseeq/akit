package shellcmd

import "testing"

func TestCommandLookupDoesNotRunTarget(t *testing.T) {
	for _, input := range []string{
		`command -v pkill >/dev/null || echo missing`,
		`x=$(command -v pkill)`,
		`[ -x "$(command -v pkill)" ] && echo ok`,
		`command -v kill 9`,
		`command -V kill 9`,
		`command -xv kill 9`,
		`command -vx pkill`,
		`command -zv kill 9`,
		`command -Vx kill 9`,
		`command --zzz -v pkill`,
		`nice command -v pkill`,
		`sudo command -v kill 9`,
		`command -v exec -- kill 9`,
		"command -v exec -- bash <<'E'\nkill 9\nE",
		`echo 'kill 9' | command -v exec -- bash`,
	} {
		t.Run(input, func(t *testing.T) {
			calls, err := Calls(input)
			if err != nil || containsCallName(calls, "kill") || containsCallName(calls, "pkill") || containsCallName(calls, "killall") {
				t.Fatalf("lookup: %v, %v", calls, err)
			}
		})
	}
}

func TestCommandLookupBoundaries(t *testing.T) {
	for _, input := range []string{
		`command --verbose kill 9`,
		`command -- kill -v 9`,
		`command kill -v 9`,
		`command -x kill 9`,
		`command --zzz kill 9`,
		`exec -v kill 9`,
		`builtin -v kill 9`,
		`command -v echo; kill 9`,
		`command -v $(kill 9)`,
	} {
		t.Run(input, func(t *testing.T) {
			calls, err := Calls(input)
			if err != nil || !containsCallName(calls, "kill") {
				t.Fatalf("signal lost: %v, %v", calls, err)
			}
		})
	}
}
