package shellcmd

import "testing"

// An or-list feeds nothing to its right side: only | and |& make the next
// command read the left side's output.
func TestOrListShellHasNoScriptInput(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"true || bash <<'EOF'\nkill 101\nEOF", false},
		{"true || bash <<< 'kill 101'", false},
		{"true || bash", false},
		{"true || bash -s", false},
		{"true && bash", false},
		{"true; bash", false},
		{"echo hi | bash", true},
		{"echo hi |& bash", true},
		{"false || echo hi | bash", true},
		{"true || { echo hi | bash; }", true},
	}
	for _, tc := range cases {
		calls, err := Calls(tc.src)
		if err != nil {
			t.Fatalf("%q: %v", tc.src, err)
		}
		found := false
		for _, call := range calls {
			if call.Name != "bash" {
				continue
			}
			found = true
			if call.ScriptInput != tc.want {
				t.Errorf("%q: bash ScriptInput = %v, want %v", tc.src, call.ScriptInput, tc.want)
			}
		}
		if !found {
			t.Errorf("%q: no bash call in %#v", tc.src, calls)
		}
	}
}
