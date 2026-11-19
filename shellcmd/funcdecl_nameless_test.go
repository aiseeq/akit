package shellcmd

import "testing"

// A function declaration can reach the walkers without Name: the parser
// accepts "() word" as an anonymous function, and zsh forms carry no name or
// several Names. Every entry point must answer without panicking.
func TestNamelessFunctionDeclarationsDoNotPanic(t *testing.T) {
	// zsh marks forms the bash parser rejects with an error instead of a tree.
	forms := []struct {
		src string
		zsh bool
	}{
		{"() answers ", false},
		{"() without corresponding ", false},
		{"echo a; () answers; echo b", false},
		{"f() { :; }; () answers", false},
		{"function { :; }", true},
		{"() { :; }", true},
		{"function f g { :; }", true},
		{"function f g { set +B; }; f; echo {a,b}", true},
	}
	check := func(t *testing.T, what string, zsh bool, err error) {
		t.Helper()
		if (err != nil) != zsh {
			t.Errorf("%s error = %v, want error %v", what, err, zsh)
		}
	}
	for _, form := range forms {
		t.Run(form.src, func(t *testing.T) {
			calls, err := Calls(form.src)
			check(t, "Calls", form.zsh, err)
			_, err = Find(form.src, "echo")
			check(t, "Find", form.zsh, err)
			_, err = Assigns(form.src)
			check(t, "Assigns", form.zsh, err)
			_, err = Scopes(form.src)
			check(t, "Scopes", form.zsh, err)
			for _, c := range calls {
				for i := range c.Args {
					_ = c.ArgTilde(i)
				}
				_, err := c.ShadowedByFunction(form.src)
				check(t, "ShadowedByFunction", false, err)
			}
		})
	}
}

// The first nameless form is the one a consumer hit: it parsed to one call
// before tilde tracking and must keep doing so.
func TestNamelessFunctionDeclarationKeepsCall(t *testing.T) {
	calls, err := Calls("() answers ")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Name != "answers" {
		t.Fatalf("calls = %+v, want one call answers", calls)
	}
}
