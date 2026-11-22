package shellcmd

import (
	"slices"
	"testing"
)

// zsh precommand modifiers noglob and nocorrect run the next word as the command.
func TestZshPrecommandModifiersAreWrappers(t *testing.T) {
	for _, tc := range []struct {
		src, name, via string
	}{
		{`noglob kill -9 1`, "kill", "noglob"},
		{`nocorrect rm -rf /x`, "rm", "nocorrect"},
		{`noglob cd /tmp`, "cd", "noglob"},
		{`zsh -c 'noglob kill -9 1'`, "kill", "noglob"},
		{`nocorrect noglob rm -rf /x`, "rm", "noglob"},
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
