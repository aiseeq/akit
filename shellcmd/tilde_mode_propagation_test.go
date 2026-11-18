package shellcmd

import (
	"strings"
	"testing"
)

func TestTildeModePropagationForms(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want Tilde
	}{
		{"plain-assignment-brace-suffix", `S=~/x{a,b}; printf '%s' "$S"`, TildeHome},
		{"colon-assignment-brace-suffix", `S=a:~/x{a,b}; printf '%s' "$S"`, TildeHome},
		{"eval-in-conditional-unknown-brace-mode", `if [ -z "$X" ]; then set +B; fi; eval 'printf "%s" {~,a}/x'`, TildeHome | TildeModeUnknown},
		{"eval-in-conditional-unknown-posix-mode", `if [ -n "$X" ]; then set -o posix; fi; eval 'printf "%s" S=~/x'`, TildeHome | TildeModeUnknown},
		{"dynamic-eval-mode-unknown", `eval "$X"; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
		{"else-branch-mode-unknown", `if [ -n "$X" ]; then :; else set +B; fi; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
		{"literal-case-eval-unknown-after-case", `case x in x) set +B;; esac; eval 'printf "%s" {~,a}/x'`, TildeHome | TildeModeUnknown},
		{"literal-case-following-mode-unknown", `case x in x) set +B;; esac; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
		{"exit-trap-word-after-change-unknown", `trap 'printf "%s" {~,a}/x' EXIT; set +B`, TildeHome | TildeModeUnknown},
		{"debug-trap-mode-unknown", `trap 'set +B' DEBUG; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
		{"loop-same-mode-stays-known", `for i in 1 2; do printf '%s' {~,a}/x; set -B; done`, TildeHome},
		{"loop-later-pass-unknown", `for i in 1 2; do printf '%s' {~,a}/x; set +B; done`, TildeHome | TildeModeUnknown},
		{"loop-word-after-mode-change", `for i in 1 2; do set +B; printf '%s' {~,a}/x; done`, 0},
		{"loop-after-possibly-empty-mode-change", `for i in $X; do set +B; done; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
		{"redefined-function-brace-mode", `f() { :; }; f; f() { set +B; }; f; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
		{"redefined-function-posix-mode", `f() { :; }; f; f() { set -o posix; }; f; printf '%s' S=~/x`, TildeHome | TildeModeUnknown},
		{"recursive-function-mode-unknown", `f() { if [ -z "$D" ]; then D=1; g; fi; set +B; }; g() { f; }; (f); g; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
		{"variable-function-name-mode-unknown", `f() { set +B; }; F=f; $F; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
		{"process-substitution-in-loop-redirection", `set +B; while read -r l; do :; done < <(printf '%s' {~,a}/x >&2)`, 0},
		{"process-substitution-in-block-redirection", `set +B; { :; } < <(printf '%s' {~,a}/x >&2)`, 0},
		{"pipeline-right-side-mode-is-isolated", `true | set +B; printf '%s' {~,a}/x`, TildeHome},
		{"function-subshell-mode-is-isolated", `f() { (eval "$X"); }; f; printf '%s' S=~/x`, TildeHome},
		{"function-background-mode-is-isolated", `f() { eval "$X" & }; f; printf '%s' S=~/x`, TildeHome},
		{"function-pipeline-mode-is-isolated", `f() { eval "$X" | :; }; f; printf '%s' S=~/x`, TildeHome},
		{"shopt-query-brace-mode", `set +B; shopt -qo braceexpand; printf '%s' {~,a}/x`, 0},
		{"conditional-same-brace-mode-known", `if [ -n "$X" ]; then set -B; fi; printf '%s' {~,a}/x`, TildeHome},
		{"conditional-same-posix-mode-known", `set -o posix; if [ -n "$X" ]; then set -o posix; fi; printf '%s' S=~/x`, 0},
		{"shopt-query-posix-mode", `shopt -o posix >/dev/null; printf '%s' S=~/x`, TildeHome},
		{"shopt-query-posix-short-mode", `shopt -po posix >/dev/null; printf '%s' S=~/x`, TildeHome},
		{"colon-posix-assignment-argument", `: POSIXLY_CORRECT=1; printf '%s' S=~/x`, TildeHome},
		{"export-posix-variable", `export POSIXLY_CORRECT; printf '%s' S=~/x`, TildeHome | TildeModeUnknown},
		{"declare-posix-variable", `declare POSIXLY_CORRECT; printf '%s' S=~/x`, TildeHome | TildeModeUnknown},
		{"readonly-posix-variable", `readonly POSIXLY_CORRECT; printf '%s' S=~/x`, TildeHome | TildeModeUnknown},
		{"prefix-posix-before-set-disable", `POSIXLY_CORRECT=1 set +o posix; printf '%s' S=~/x`, 0},
		{"prefix-posix-before-shift", `POSIXLY_CORRECT=1 shift; printf '%s' S=~/x`, 0},
		{"prefix-posix-before-unset-other", `POSIXLY_CORRECT=1 unset X; printf '%s' S=~/x`, 0},
		{"prefix-posix-before-declare", `POSIXLY_CORRECT=1 declare X=1; printf '%s' S=~/x`, TildeHome | TildeModeUnknown},
		{"prefix-posix-through-command-unknown", `POSIXLY_CORRECT=1 command :; printf '%s' S=~/x`, TildeHome | TildeModeUnknown},
		{"prefix-posix-eval", `POSIXLY_CORRECT=1 eval 'printf "%s" S=~/x'`, 0},
		{"unset-functions-does-not-disable-posix", `set -o posix; unset -f POSIXLY_CORRECT; printf '%s' S=~/x`, 0},
		{"flock-posix-mode-unknown", `flock /tmp/lk9 -c 'printf "%s" S=~/x'`, TildeHome | TildeModeUnknown},
		{"flock-brace-mode-unknown", `set +B; flock /tmp/lk9 -c 'printf "%s" {~,a}/x'`, TildeHome | TildeModeUnknown},
		{"piped-sh-posix-mode", `echo 'printf "%s" S=~/x' | sh`, 0},
		{"bash-here-string-posix-mode", `bash -o posix <<<'printf "%s" S=~/x'`, 0},
		{"env-split-string-no-tilde", `env -S 'printf %s\n ~/x'`, 0},
		{"command-p-set-mode", `command -p set +B; printf '%s' {~,a}/x`, 0},
		{"brace-expansion-tilde-alternative", `printf '%s' {echo,~/x}`, TildeHome},
		{"exec-redirection-mode-known", `exec 2>&1; printf '%s' {~,a}/x`, TildeHome},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.name, "assignment-brace-suffix") {
				assigns, err := Assigns(tc.src)
				if err != nil || len(assigns) == 0 {
					t.Fatalf("Assigns: %v", err)
				}
				if assigns[0].Tilde != tc.want {
					t.Fatalf("Assign.Tilde=%d, want %d", assigns[0].Tilde, tc.want)
				}
				return
			}
			calls, err := Calls(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			got, found := lastCallTilde(calls, "printf")
			if !found {
				t.Fatalf("printf not found: %+v", calls)
			}
			if got != tc.want {
				t.Fatalf("ArgTilde=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestTildeLocalDeclarationBraceSuffix(t *testing.T) {
	assigns, err := Assigns(`f() { local S=~/x{a,b}; }; f`)
	if err != nil {
		t.Fatal(err)
	}
	for _, assign := range assigns {
		if assign.Name == "S" {
			if assign.Tilde != 0 {
				t.Fatalf("local Assign.Tilde=%d, want 0", assign.Tilde)
			}
			return
		}
	}
	t.Fatalf("local assignment not found: %+v", assigns)
}

func TestTildeUnknownModeDeclarationAssignment(t *testing.T) {
	forms := []struct {
		name string
		src  string
	}{
		{"brace-disabled-branch", `if [ -n "$X" ]; then set +B; fi; declare S=~/x{a,b}`},
		{"mixed-brace-branches", `if [ -n "$X" ]; then set +B; else set -B; fi; declare S=~/x{a,b}`},
	}
	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			assigns, err := Assigns(form.src)
			if err != nil {
				t.Fatal(err)
			}
			for _, assign := range assigns {
				if assign.Name == "S" {
					want := TildeHome | TildeModeUnknown
					if assign.Tilde != want {
						t.Fatalf("conditional declare Assign.Tilde=%d, want %d", assign.Tilde, want)
					}
					return
				}
			}
			t.Fatalf("conditional declare assignment not found: %+v", assigns)
		})
	}
}

func lastCallTilde(calls []Call, name string) (Tilde, bool) {
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Name == name && len(calls[i].Args) > 0 {
			return calls[i].ArgTilde(len(calls[i].Args) - 1), true
		}
	}
	return 0, false
}

func TestTildeModeDepthLimitMarksUnknown(t *testing.T) {
	blocks := strings.Repeat("{ ", 70) + "set +B; printf '%s' {~,a}/x" + strings.Repeat("; }", 70)
	if got := lastTildeArg(t, blocks, 1); got != TildeHome|TildeModeUnknown {
		t.Fatalf("deep block ArgTilde=%d, want %d", got, TildeHome|TildeModeUnknown)
	}
	subshells := strings.Repeat("( ", 70) + "set +B; printf '%s' {~,a}/x" + strings.Repeat(" )", 70)
	if got := lastTildeArg(t, subshells, 1); got != TildeHome|TildeModeUnknown {
		t.Fatalf("deep subshell ArgTilde=%d, want %d", got, TildeHome|TildeModeUnknown)
	}
}
