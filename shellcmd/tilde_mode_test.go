package shellcmd

import "testing"

func lastTildeArg(t *testing.T, src string, index int) Tilde {
	t.Helper()
	calls, err := Calls(src)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Name == "printf" {
			return calls[i].ArgTilde(index)
		}
	}
	t.Fatal("printf call not found")
	return 0
}

func TestTildeModeOptionForms(t *testing.T) {
	cases := []struct {
		name string
		src  string
		idx  int
		want Tilde
	}{
		{"set-end-marker-plus-brace", `set -- +B; printf '%s\n' {~,a}/x`, 1, TildeHome},
		{"set-lone-minus-plus-brace", `set - +B; printf '%s\n' {~,a}/x`, 1, TildeHome},
		{"set-positional-plus-brace", `set x +B; printf '%s\n' {~,a}/x`, 1, TildeHome},
		{"set-combined-brace-disable", `set +eB; printf '%s\n' {~,a}/x`, 1, 0},
		{"set-brace-enable-again", `set +B; set -B; printf '%s\n' {~,a}/x`, 1, TildeHome},
		{"set-posix-after-shopt", `shopt -os posix; printf '%s\n' S=~/x`, 1, 0},
		{"set-end-marker-posix", `set -- -o posix; printf '%s\n' S=~/x`, 1, TildeHome},
		{"set-lone-minus-posix", `set - -o posix; printf '%s\n' S=~/x`, 1, TildeHome},
		{"set-positional-posix", `set x -o posix; printf '%s\n' S=~/x`, 1, TildeHome},
		{"set-o-without-operand", `set -o; printf '%s\n' S=~/x`, 1, TildeHome},
		{"set-posix-then-disable", `set -o posix +o posix; printf '%s\n' S=~/x`, 1, TildeHome},
		{"set-eo-without-operand", `set -eo; printf '%s\n' S=~/x`, 1, TildeHome},
		{"set-eo-pipefail-then-disable", `set -o posix; set -eo pipefail +o posix; printf '%s\n' S=~/x`, 1, TildeHome},
		{"set-plus-eo-posix", `set -o posix; set +eo posix; printf '%s\n' S=~/x`, 1, TildeHome},
		{"set-double-dash-o-posix", `set --o posix; printf '%s\n' S=~/x`, 1, TildeHome},
		{"set-short-combined-posix", `set -oe posix; printf '%s\n' S=~/x`, 1, 0},
		{"set-short-combined-posix-after-e", `set -eo posix; printf '%s\n' S=~/x`, 1, 0},
		{"set-combined-attached-posix", `set -eoposix; printf '%s\n' S=~/x`, 1, 0},
		{"set-short-escaped-brace-option", "set +\\B; printf '%s\\n' {~,a}/x", 1, 0},
		{"set-attached-posix-operand", `set -oposix; printf '%s\n' S=~/x`, 1, TildeHome},
		{"set-long-option-stops-option-scan", `set --posix -o posix; printf '%s\n' S=~/x`, 1, TildeHome},
		{"posix-turned-off-again", `set -o posix; set +o posix; printf '%s\n' S=~/x`, 1, TildeHome},
		{"escaped-command-name", `s\et -o posix; printf '%s\n' S=~/x`, 1, 0},
		{"builtin-set", `builtin set -o posix; printf '%s\n' S=~/x`, 1, 0},
		{"command-set", `command set -o posix; printf '%s\n' S=~/x`, 1, 0},
		{"eval-posix-literal", `eval 'set -o posix'; printf '%s\n' S=~/x`, 1, 0},
		{"function-posix-may-change", `f() { set -o posix; }; f; printf '%s\n' S=~/x`, 1, TildeHome | TildeModeUnknown},
		{"function-short-posix-may-change", `f() { set -eo posix; }; f; printf '%s\n' S=~/x`, 1, TildeHome | TildeModeUnknown},
		{"function-prefix-posix-may-change", `f() { POSIXLY_CORRECT=1 :; }; f; printf '%s\n' S=~/x`, 1, TildeHome | TildeModeUnknown},
		{"function-default-mode-known", `f() { printf '%s\n' S=~/x; }; f`, 1, TildeHome},
		{"function-nested-literal-eval-mode-may-change", `f() { eval 'eval "set +B"'; }; f; printf '%s\n' {~,a}/x`, 1, TildeHome | TildeModeUnknown},
		{"function-short-shopt-posix-mode-may-change", `f() { shopt -os posix; }; f; printf '%s\n' S=~/x`, 1, TildeHome | TildeModeUnknown},
		{"if-posix-uncertain", `if true; then set -o posix; fi; printf '%s\n' S=~/x`, 1, TildeHome | TildeModeUnknown},
		{"and-posix-uncertain", `true && set -o posix; printf '%s\n' S=~/x`, 1, TildeHome | TildeModeUnknown},
		{"set-posix-does-not-reach-bash", `set -o posix; bash -c 'printf "%s" S=~/x'`, 1, TildeHome},
		{"posix-sh-child", `sh -c 'printf "%s" S=~/x'`, 1, 0},
		{"posix-after-sh-child", `sh -c 'true'; bash -c 'printf "%s" S=~/x'`, 1, TildeHome},
		{"posix-set-in-pipeline-is-local", `set -o posix | printf '%s' S=~/x`, 1, TildeHome},
		{"posix-in-command-substitution", `set -o posix; printf '%s' "$(printf '%s' S=~/x)"`, 1, 0},
		{"posix-command-substitution-does-not-leak", `$(set -o posix); printf '%s' S=~/x`, 1, TildeHome},
		{"environment-posix-empty", `POSIXLY_CORRECT= bash -c 'printf "%s" S=~/x'`, 1, 0},
		{"environment-posix-through-env", `env POSIXLY_CORRECT=1 bash -c 'printf "%s" S=~/x'`, 1, 0},
		{"environment-posix-current-shell", `POSIXLY_CORRECT=1; printf '%s\n' S=~/x`, 1, 0},
		{"environment-posix-export", `export POSIXLY_CORRECT=1; printf '%s\n' S=~/x`, 1, 0},
		{"child-bash-posix-flag", `bash --posix -c 'printf "%s" S=~/x'`, 1, 0},
		{"child-bash-posix-option", `bash -o posix -c 'printf "%s" S=~/x'`, 1, 0},
		{"child-bash-posix-option-after-c", `bash -c -o posix 'printf "%s" S=~/x'`, 1, 0},
		{"child-bash-brace-option-after-c", `bash -c +B 'printf "%s" {~,a}/x'`, 1, 0},
		{"nested-bash-function-brace-option", `bash +B -c 'f() { printf "%s" {~,a}/x; }; f'`, 1, 0},
		{"unvisited-nested-bash-function-brace-option", `bash +B -c 'f() { printf "%s" {~,a}/x; }'`, 1, 0},
		{"child-posix-option-after-command", `bash -c 'printf "%s" S=~/x' --posix`, 1, TildeHome},
		{"brace-disabled-short-option", `set +o braceexpand; printf '%s\n' {~,a}/x`, 1, 0},
		{"brace-disabled-other-option-first", `set -o pipefail +B; printf '%s\n' {~,a}/x`, 1, 0},
		{"brace-disabled-in-subshell", `set +B; (printf '%s\n' {~,a}/x)`, 1, 0},
		{"brace-disabled-in-if", `set +B; if true; then printf '%s\n' {~,a}/x; fi`, 1, 0},
		{"brace-disabled-function-body", `set +B; f() { printf '%s\n' {~,a}/x; }; f`, 1, 0},
		{"brace-mode-after-function-may-change", `f() { set +B; }; f; printf '%s\n' {~,a}/x`, 1, TildeHome | TildeModeUnknown},
		{"brace-mode-after-if-uncertain", `if true; then set +B; fi; printf '%s\n' {~,a}/x`, 1, TildeHome | TildeModeUnknown},
		{"brace-on-after-disable", `set +B; set -B; printf '%s\n' {~,a}/x`, 1, TildeHome},
		{"escaped-array-index-tilde", `printf '%s\n' a[\~]=~/x`, 1, TildeHome},
		{"escaped-array-index-colon", `printf '%s\n' a[\~]=x:~/y`, 1, TildeHome},
		{"escaped-array-index-user", `printf '%s\n' a[~]=~ro\ot/x`, 1, 0},
		{"escaped-colon-after-tilde", `printf '%s\n' S=~\:x`, 1, 0},
		{"brace-bare-home-last", `printf '%s\n' {a,~}`, 1, TildeHome},
		{"brace-bare-user-last", `printf '%s\n' {a,~root}`, 1, TildeOther},
		{"brace-quoted-tilde", `printf '%s\n' {"~",a}/x`, 1, 0},
		{"brace-tilde-not-first", `printf '%s\n' {a~/x,b}`, 1, 0},
		{"brace-colon-end-prefix", `printf '%s\n' {~:a,b}`, 1, TildeHome},
		{"brace-user-prefix", `printf '%s\n' {~root,b}/x`, 1, TildeOther},
		{"colon-dollar-quoted", `printf '%s\n' ~:$'x'`, 1, 0},
		{"brace-disabled-open-prefix", `set +B; printf '%s\n' ~{x/y`, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastTildeArg(t, tc.src, tc.idx); got != tc.want {
				t.Fatalf("ArgTilde=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestTildeDeclarationAndArrayAssignmentForms(t *testing.T) {
	cases := []struct {
		name, source string
		want         Tilde
	}{
		{"declare-brace-assignment", `declare S={a,b:~/x}`, 0},
		{"assignment-open-brace", `S=~{x/y`, 0},
		{"assignment-tilde-in-parameter-default", `S=${x:-a:~}`, TildeHome},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assigns, err := Assigns(tc.source)
			if err != nil || len(assigns) == 0 {
				t.Fatalf("Assigns: %v", err)
			}
			if assigns[0].Tilde != tc.want {
				t.Fatalf("Assign.Tilde=%d, want %d", assigns[0].Tilde, tc.want)
			}
		})
	}
}
