package shellcmd

import "testing"

func TestTildeExpansionModeAndBraceEdges(t *testing.T) {
	arg := func(src, name string) func(*testing.T) Tilde {
		return func(t *testing.T) Tilde {
			calls, err := Calls(src)
			if err != nil {
				t.Fatal(err)
			}
			for i := len(calls) - 1; i >= 0; i-- {
				if calls[i].Name == name && len(calls[i].Args) != 0 {
					return calls[i].ArgTilde(len(calls[i].Args) - 1)
				}
			}
			t.Fatalf("no %s call in %q", name, src)
			return 0
		}
	}
	assign := func(src, name string) func(*testing.T) Tilde {
		return func(t *testing.T) Tilde {
			assigns, err := Assigns(src)
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range assigns {
				if a.Name == name {
					return a.Tilde
				}
			}
			t.Fatalf("no %s assignment in %q", name, src)
			return 0
		}
	}
	redir := func(src string) func(*testing.T) Tilde {
		return func(t *testing.T) Tilde {
			calls, err := Calls(src)
			if err != nil || len(calls) == 0 {
				t.Fatalf("Calls: %v", err)
			}
			rs := calls[len(calls)-1].Redirs
			if len(rs) == 0 {
				t.Fatal("no redirect")
			}
			return rs[len(rs)-1].Tilde
		}
	}
	cases := []struct {
		name string
		get  func(*testing.T) Tilde
		want Tilde
	}{
		{"colon-then-close-brace", arg(`printf '%s\n' ~:x}`, "printf"), TildeHome},
		{"colon-then-open-brace", arg(`printf '%s\n' ~:x{`, "printf"), TildeHome},
		{"brace-in-user-prefix", arg(`printf '%s\n' ~x}/y`, "printf"), 0},
		{"ordinary-assignment-colon-in-brace", assign(`S={a,b:~/x}`, "S"), TildeHome},
		{"declaration-colon-in-brace-with-brace-disabled", assign(`set +B; declare S={a,b:~/x}`, "S"), TildeHome},
		{"argument-assignment-brace-suffix", arg(`printf '%s\n' S=~/x{a,b}`, "printf"), 0},
		{"declaration-brace-suffix", assign(`declare S=~/x{a,b}`, "S"), 0},
		{"declaration-brace-suffix-disabled", assign(`set +B; declare S=~/x{a,b}`, "S"), TildeHome},
		{"ambiguous-redirect-with-tail", redir(`cat < {~,}x`), 0},
		{"empty-redirect-alternative", redir(`cat < {~,}`), TildeHome},
		{"quoted-empty-redirect-alternative", redir(`cat < {~,""}`), 0},
		{"nested-redirect-alternative", redir(`cat < {~,{,}x}`), 0},
		{"array-index-expression", arg(`printf '%s\n' a[[x]]=~/y`, "printf"), TildeHome},
		{"literal-eval-current-brace-mode", arg(`set +B; eval "printf '%s\\n' {~,a}/x"`, "printf"), 0},
		{"literal-eval-current-posix-mode", arg(`set -o posix; eval 'printf "%s\\n" S=~/x'`, "printf"), 0},
		{"substitution-in-declaration", arg(`set +B; export X=$(printf '%s\\n' {~,a}/x)`, "printf"), 0},
		{"substitution-in-test", arg(`set +B; [[ -n $(printf '%s\\n' {~,a}/x) ]]`, "printf"), 0},
		{"branch-mode-local-to-arm", arg(`set +B; if true; then printf '%s\\n' {~,a}/x; fi`, "printf"), 0},
		{"function-mode-effect-unknown", arg(`f() { set +B; }; f; printf '%s\\n' {~,a}/x`, "printf"), TildeHome | TildeModeUnknown},
		{"background-mode-isolated", arg(`set +B & wait; printf '%s\\n' {~,a}/x`, "printf"), TildeHome},
		{"time-inner-mode-persistent", arg(`time set +B; printf '%s\\n' {~,a}/x`, "printf"), 0},
		{"coprocess-mode-isolated", arg(`set +B; coproc printf '%s\\n' {~,a}/x`, "printf"), 0},
		{"case-mode-from-before", arg(`set +B; case x in x) printf '%s\\n' {~,a}/x;; esac`, "printf"), 0},
		{"case-branch-mode-known-inside", arg(`case x in x) set +B; printf '%s\\n' {~,a}/x;; esac`, "printf"), 0},
		{"and-left-mode-persistent", arg(`set +B && printf '%s\\n' {~,a}/x`, "printf"), TildeHome | TildeModeUnknown},
		{"background-mode-does-not-leak", arg(`set +B & wait; printf '%s\\n' {~,a}/x`, "printf"), TildeHome},
		{"nested-function-mode-unknown", arg(`f() { set +B; }; g() { f; }; g; printf '%s\\n' {~,a}/x`, "printf"), TildeHome | TildeModeUnknown},
		{"loop-condition-after-body-option-change-unknown", arg(`set +B; i=0; while printf '%s\\n' {~,a}/x; [ $i -lt 1 ]; do set -B; i=1; done`, "printf"), TildeHome | TildeModeUnknown},
		{"loop-until-condition-after-body-option-change-unknown", arg(`set +B; until printf '%s\\n' {~,a}/x; [ -n "$D" ]; do D=1; set -B; done`, "printf"), TildeHome | TildeModeUnknown},
		{"deferred-trap-function-mode-unknown", arg(`f() { printf '%s\\n' {~,a}/x; }; trap 'f;:' EXIT; set +B`, "printf"), TildeHome | TildeModeUnknown},
		{"dynamic-function-body-uses-call-site-mode", arg(`f() { printf '%s\\n' {~,a}/x; }; F=f; set +B; $F`, "printf"), 0},
		{"posix-function-mode-unknown", arg(`f() { set -o posix; }; g() { f; }; g; printf '%s\\n' S=~/x`, "printf"), TildeHome | TildeModeUnknown},
		{"unset-posix-variable", arg(`unset POSIXLY_CORRECT; printf '%s\\n' S=~/x`, "printf"), TildeHome},
		{"eval-mode-from-literal", arg(`eval 'set +B'; printf '%s\\n' {~,a}/x`, "printf"), 0},
		{"literal-eval-in-function", arg(`f() { eval 'set +B'; printf '%s\\n' {~,a}/x; }; f`, "printf"), 0},
		{"posix-environment-only-for-child", arg(`POSIXLY_CORRECT=1 bash -c 'printf "%s\\n" S=~/x'`, "printf"), 0},
		{"shopt-invalid-brace-option", arg(`shopt -u braceexpand; printf '%s\\n' {~,a}/x`, "printf"), TildeHome},
		{"zero-iteration-loop-mode-unknown", arg(`while false; do set +B; done; printf '%s\\n' {~,a}/x`, "printf"), TildeHome | TildeModeUnknown},
		{"pipeline-mode-does-not-leak", arg(`set +B | cat; printf '%s\\n' {~,a}/x`, "printf"), TildeHome},
		{"pipeline-mode-does-not-affect-peer", arg(`set +B | printf '%s\\n' {~,a}/x`, "printf"), TildeHome},
		{"substitution-in-process-redirection", arg(`set +B; cat <(printf '%s\\n' {~,a}/x)`, "printf"), 0},
		{"substitution-in-array-assignment", arg(`set +B; a=($(printf '%s\\n' {~,a}/x))`, "printf"), 0},
		{"substitution-in-for-list", arg(`set +B; for x in $(printf '%s\\n' {~,a}/x); do :; done`, "printf"), 0},
		{"nested-function-export-mode", arg(`f() { export POSIXLY_CORRECT=1; }; f; printf '%s\\n' S=~/x`, "printf"), TildeHome | TildeModeUnknown},
		{"nested-direct-block-mode", arg(`{ { { { { { { set +B; }; }; }; }; }; }; }; printf '%s\\n' {~,a}/x`, "printf"), 0},
		{"case-mode-after-branch-unknown", arg(`case x in x) set +B;; esac; printf '%s\\n' {~,a}/x`, "printf"), TildeHome | TildeModeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.get(t); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}
