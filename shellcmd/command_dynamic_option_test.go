package shellcmd

import (
	"strings"
	"testing"
)

func TestCommandDynamicOptionKeepsFollowingCommand(t *testing.T) {
	killAll := "kill -9 -" + "1"
	forms := []struct {
		kind string
		src  string
	}{
		{"kill", `command -$v ` + killAll},
		{"kill", `command -${v} ` + killAll},
		{"kill", `command -${verb:-p} ` + killAll},
		{"kill", `command -$opt ` + killAll},
		{"kill", `command $opt ` + killAll},
		{"kill", `command "$@" ` + killAll},
		{"kill", `command -$(echo p) ` + killAll},
		{"kill", `command -p$v ` + killAll},
		{"kill", `o=-p; command $o ` + killAll},
		{"kill", `sudo command -${v} ` + killAll},
		{"kill", `nice command -${verb:-p} ` + killAll},
		{"kill", `command "-$v" ` + killAll},
		{"kill", `command -"${v}" ` + killAll},
		{"kill", `command -${OPTV} ` + killAll},
		{"kill", `command -$'\x70' ` + killAll},
		{"kill", `command -${x/v/p} ` + killAll},
		{"kill", `command -$(echo v) ` + killAll},
		{"kill", "command -`echo p` " + killAll},
		{"kill", `command -"$v" -p ` + killAll},
		{"kill", `command -p -$v ` + killAll},
		{"kill", `builtin command -$v ` + killAll},
		{"kill", `f() { command -$1 ` + killAll + `; }; f p`},
		{"pkill", `command -$v pkill -f x`},
		{"kill", "command -${v} bash <<E\n" + killAll + "\nE"},
		{"any", `o=-v; command $o ` + killAll},
		{"any", `command -v$x ` + killAll},
		{"any", `command -pv$x ` + killAll},
		{"any", "command -`echo p`v " + killAll},
		{"none", `command -'v' ` + killAll},
		{"none", `command -v ` + killAll},
		{"none", `command -V ` + killAll},
		{"none", `command -pv ` + killAll},
		{"none", `command -p -v ` + killAll},
		{"none", `command -xv ` + killAll},
		{"none", `command "-v" ` + killAll},
		{"none", `command -v'' ` + killAll},
		{"none", `command -"V" ` + killAll},
		{"none", `command $'-v' ` + killAll},
		{"none", `command $'\x2dv' ` + killAll},
		{"none", `command -p'v' ` + killAll},
		{"none", `nice command -'v' ` + killAll},
		{"none", `nice command "-v" ` + killAll},
		{"any", `C="nice -n 5"; $C command -'v' ` + killAll},
		{"any", `v=; command -v$v ` + killAll},
		{"any", `command -v$'' ` + killAll},
		{"any", `v=p; command -$v-p ` + killAll},
		{"kill", `command -$v -- ` + killAll},
		{"kill", `command -p -$v -- ` + killAll},
		{"kill", `command ${v:--p} ` + killAll},
		{"kill", `exec -a "$v" command -$v ` + killAll},
		{"kill", `env -u "$x" command -$v ` + killAll},
		{"kill", `timeout $t command -$v ` + killAll},
		{"kill", `( command -$v ` + killAll + ` )`},
		{"kill", `x=$(command -$v ` + killAll + `)`},
		{"none", `command -- -x ` + killAll},
	}
	for _, form := range forms {
		t.Run(form.src, func(t *testing.T) {
			calls, err := Calls(form.src)
			found := false
			for _, call := range calls {
				found = found || call.Name == "kill" || call.Name == "pkill"
			}
			switch form.kind {
			case "any":
				return
			case "none":
				if found || err != nil {
					t.Fatalf("literal lookup should not find kill: calls=%v err=%v", callNames(calls), err)
				}
			default:
				if !found && err == nil {
					t.Fatalf("dynamic option hid following command: calls=%v", callNames(calls))
				}
			}
		})
	}
}

func callNames(calls []Call) []string {
	names := make([]string, len(calls))
	for i, call := range calls {
		names[i] = call.Name
	}
	return names
}

func TestCommandLookupOptionScanStopsAtTarget(t *testing.T) {
	calls, err := Calls(`command echo -v; command grep -V x`)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(callNames(calls), ","); got != "command,echo,command,grep" {
		t.Fatalf("calls=%s", got)
	}
}
