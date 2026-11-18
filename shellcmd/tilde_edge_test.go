package shellcmd

import "testing"

// Edge forms exercise tilde marks across quoting, expansion, and shell context.
func TestTildeExpansionEdgeCases(t *testing.T) {
	arg := func(t *testing.T, src, name string, i int) Tilde {
		calls, err := Calls(src)
		if err != nil {
			t.Fatal(err)
		}
		for j := len(calls) - 1; j >= 0; j-- {
			if calls[j].Name == name {
				return calls[j].ArgTilde(i)
			}
		}
		t.Fatalf("no call %s in %+v", name, calls)
		return 0
	}
	last := func(t *testing.T, src string) Tilde {
		calls, err := Calls(src)
		if err != nil {
			t.Fatal(err)
		}
		for j := len(calls) - 1; j >= 0; j-- {
			if calls[j].Name == "printf" {
				return calls[j].ArgTilde(len(calls[j].Args) - 1)
			}
		}
		t.Fatal("no printf")
		return 0
	}
	redir := func(t *testing.T, src string) Tilde {
		calls, err := Calls(src)
		if err != nil || len(calls) == 0 {
			t.Fatal(err)
		}
		r := calls[len(calls)-1].Redirs
		return r[len(r)-1].Tilde
	}
	assign := func(t *testing.T, src string) Tilde {
		as, err := Assigns(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range as {
			if a.Name == "S" {
				return a.Tilde
			}
		}
		t.Fatal("no S")
		return 0
	}
	cases := []struct {
		name string
		got  func(t *testing.T) Tilde
		want Tilde
	}{
		{"out-of-range-after-empty-argv0", func(t *testing.T) Tilde { return arg(t, `P=""; $P printf '%s\n' a ~/x`, "printf", 3) }, 0},
		{"param-default-colon-arg", func(t *testing.T) Tilde { return last(t, `printf '%s\n' ${x:-a:~}`) }, 0},
		{"param-pattern-colon-assign", func(t *testing.T) Tilde { return assign(t, `S=${x/:~/y}`) }, 0},
		{"command-substitution-colon-assign", func(t *testing.T) Tilde { return assign(t, `S=$(echo :~/x)`) }, 0},
		{"double-quoted-colon-assign", func(t *testing.T) Tilde { return assign(t, `S="a:~/x"`) }, 0},
		{"quoted-closing-brace", func(t *testing.T) Tilde { return last(t, `printf '%s\n' {~/x,a"}"`) }, 0},
		{"quoted-brace-in-alternative", func(t *testing.T) Tilde { return last(t, `printf '%s\n' {a"{",~/x}`) }, TildeHome},
		{"commaless-inner-brace", func(t *testing.T) Tilde { return last(t, `printf '%s\n' {{a},~/x}`) }, TildeHome},
		{"quoted-comma-in-alternative", func(t *testing.T) Tilde { return last(t, `printf '%s\n' {a",~/x",b}`) }, 0},
		{"brace-suffix", func(t *testing.T) Tilde { return last(t, `printf '%s\n' {a,~}"/x"`) }, 0},
		{"unclosed-index", func(t *testing.T) Tilde { return last(t, `printf '%s\n' a[0=~/x`) }, 0},
		{"assign-colon-then-quote", func(t *testing.T) Tilde { return assign(t, `S=~:"x"`) }, TildeHome},
		{"escaped-user-char", func(t *testing.T) Tilde { return last(t, `printf '%s\n' ~ro\ot/x`) }, 0},
		{"dollar-user", func(t *testing.T) Tilde { return last(t, `printf '%s\n' ~$/x`) }, 0},
		{"open-brace-user", func(t *testing.T) Tilde { return last(t, `printf '%s\n' ~{x/y`) }, 0},
		{"close-brace-user", func(t *testing.T) Tilde { return last(t, `printf '%s\n' ~x}/y`) }, 0},
		{"heredoc-dash-delimiter", func(t *testing.T) Tilde { return redir(t, "cat <<-~\n\tx\n\t~") }, 0},
		{"parallel-argument", func(t *testing.T) Tilde { return arg(t, `parallel printf '%s\n' ~/x ::: a`, "printf", 1) }, TildeHome},
		{"posix-off-again", func(t *testing.T) Tilde { return last(t, `set -o posix; set +o posix; printf '%s\n' S=~/x`) }, TildeHome},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.got(t); got != c.want {
				t.Fatalf("got %d, want %d", got, c.want)
			}
		})
	}
}

func TestTildeMultilineModeForms(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want Tilde
	}{
		{"extglob-enabled-on-previous-line", "shopt -s extglob\nprintf '%s\\n' ~:@(x)/y", TildeHome},
		{"posix-child-from-heredoc", "env POSIXLY_CORRECT=1 bash <<'E'\nprintf '%s\\n' S=~/x\nE", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastTildeArg(t, tc.src, 1); got != tc.want {
				t.Fatalf("ArgTilde=%d, want %d", got, tc.want)
			}
		})
	}
	for _, src := range []string{"a=(~/x {~,b}/y)", "set +B; a=(~/x {~,b}/y)"} {
		assigns, err := Assigns(src)
		if err != nil {
			t.Fatalf("Assigns(%q): %v", src, err)
		}
		found := false
		for _, assign := range assigns {
			if assign.Name == "a" {
				found = true
				if assign.Tilde != TildeHome {
					t.Errorf("Assign.Tilde for %q=%d, want %d", src, assign.Tilde, TildeHome)
				}
			}
		}
		if !found {
			t.Errorf("array assignment not found for %q", src)
		}
	}
}
