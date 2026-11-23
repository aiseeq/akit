package shellcmd

import (
	"slices"
	"testing"
)

// zsh precommand modifiers noglob, nocorrect and - run the next word as the command.
func TestZshPrecommandModifiersAreWrappers(t *testing.T) {
	for _, tc := range []struct {
		src, name, via string
	}{
		{`noglob kill -9 1`, "kill", "noglob"},
		{`nocorrect rm -rf /x`, "rm", "nocorrect"},
		{`noglob cd /tmp`, "cd", "noglob"},
		{`zsh -c 'noglob kill -9 1'`, "kill", "noglob"},
		{`nocorrect noglob rm -rf /x`, "rm", "noglob"},
		// zsh precommand modifier - (argv[0] with a leading dash) also runs the next word.
		{`noglob - kill -9 1`, "kill", "noglob"},
		{`nocorrect - rm -rf /x`, "rm", "nocorrect"},
		{`noglob - - kill 1`, "kill", "noglob"},
		{`noglob exec - kill 1`, "kill", "exec"},
		{`zsh -c 'noglob - kill -9 1'`, "kill", "noglob"},
		{`- kill 1`, "kill", "-"},
		{`time - kill 1`, "kill", "-"},
		{`zsh -c '- rm -rf /x'`, "rm", "-"},
		{`- - kill 1`, "kill", "-"},
	} {
		calls, err := Calls(tc.src)
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		i := slices.IndexFunc(calls, func(c Call) bool { return c.Name == tc.name })
		if i < 0 {
			t.Fatalf("%s: no %s call in %+v", tc.src, tc.name, calls)
		}
		if !slices.Contains(calls[i].Via, tc.via) {
			t.Errorf("%s: %s Via = %v, want %s in it", tc.src, tc.name, calls[i].Via, tc.via)
		}
	}
}

func TestZshDashModifierDoesNotApplyInsideExternalWrappers(t *testing.T) {
	for _, src := range []string{`env -`, `env - A=1 - kill 1`} {
		calls, err := Calls(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		var names []string
		for _, call := range calls {
			if !slices.Contains(names, call.Name) {
				names = append(names, call.Name)
			}
		}
		slices.Sort(names)
		if !slices.Equal(names, []string{"-", "env"}) {
			t.Errorf("%s: call names = %v, want [- env] (calls %+v)", src, names, calls)
		}
	}
}
