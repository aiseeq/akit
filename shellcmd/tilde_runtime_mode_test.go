package shellcmd

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestTildeRuntimeModeForms: a mode change whose effect on a word depends on the
// run (a loop pass, a trap, a function from eval) marks the word unknown; without
// such a change the word keeps its exact mark.
func TestTildeRuntimeModeForms(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want Tilde
	}{
		{"while-condition-after-body-change", `set +B; while printf '%s' {~,a}/x; false; do set -B; done`, TildeHome | TildeModeUnknown},
		{"until-condition-after-body-change", `set +B; until printf '%s' {~,a}/x; true; do set -B; done`, TildeHome | TildeModeUnknown},
		{"until-condition-before-body-change", `until printf '%s' {~,a}/x; [ -n "$D" ]; do D=1; set +B; done`, TildeHome | TildeModeUnknown},
		{"while-condition-without-change", `while printf '%s' {~,a}/x; false; do :; done`, TildeHome},
		{"while-condition-in-changed-mode", `set +B; while printf '%s' {~,a}/x; false; do :; done`, 0},
		{"exit-trap-before-change", `trap 'printf "%s" {~,a}/x' EXIT; set +B; exit; set -B`, TildeHome | TildeModeUnknown},
		{"exit-trap-in-subshell", `(trap 'printf "%s" {~,a}/x' EXIT; set +B)`, TildeHome | TildeModeUnknown},
		{"exit-trap-in-substitution", `x=$(trap 'printf "%s" {~,a}/x' EXIT; set +B); echo "$x"`, TildeHome | TildeModeUnknown},
		{"exit-trap-before-eval-function-change", `trap 'printf "%s" {~,a}/x' EXIT; eval 'f() { set +B; }'; f`, TildeHome | TildeModeUnknown},
		{"eval-sets-exit-trap-before-change", `eval 'trap "printf %s {~,a}/x" EXIT'; set +B`, TildeHome | TildeModeUnknown},
		{"exit-trap-without-change", `trap 'printf "%s" {~,a}/x' EXIT; printf '%s' a`, TildeHome},
		{"posix-trap-without-change", `trap 'printf "%s" S=~/x' EXIT; printf '%s' a`, TildeHome},
		{"eval-function-changes-mode", `eval 'f() { set +B; }'; f; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
		{"eval-function-changes-posix", `eval 'f() { set -o posix; }'; f; printf '%s' S=~/x`, TildeHome | TildeModeUnknown},
		{"eval-function-keeps-mode", `eval 'f() { :; }'; f; printf '%s' {~,a}/x`, TildeHome},
		{"eval-function-before-computed-name", `eval 'f() { set +B; }'; F=f; $F; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
		{"eval-text-word-beside-function", `eval 'f() { :; }; printf "%s" {~,a}/x'`, TildeHome},
		{"function-sets-dynamic-debug-trap", `f() { trap "$X" DEBUG; }; f; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
		{"eval-function-calls-changing-function", `g() { set +B; }; eval 'f() { g; }'; f; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
		{"recursive-functions-without-change", `f() { g; }; g() { f; }; f; printf '%s' {~,a}/x`, TildeHome},
		{"recursive-functions-with-change", `f() { g; }; g() { f; set +B; }; f; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
		{"eval-function-after-effects-asked", `g() { :; }; g; eval 'f() { set +B; }'; f; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
		{"function-sets-debug-trap", `f() { trap 'set +B' DEBUG; }; f; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
		{"function-sets-debug-trap-calling-function", `g() { set +B; }; f() { trap g DEBUG; }; f; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
		{"function-sets-debug-trap-after-double-dash", `f() { trap -- 'set +B' DEBUG; }; f; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
		{"function-sets-exit-trap-without-change", `f() { trap ':' EXIT; }; f; printf '%s' {~,a}/x`, TildeHome},
		{"exit-trap-before-posix-assignment", `trap 'printf "%s" S=~/x' EXIT; POSIXLY_CORRECT=1`, TildeHome | TildeModeUnknown},
		{"exit-trap-before-second-posix-assignment", `trap 'printf "%s" S=~/x' EXIT; x=1 POSIXLY_CORRECT=1`, TildeHome | TildeModeUnknown},
		{"exit-trap-before-posix-declaration", `trap 'printf "%s" S=~/x' EXIT; declare POSIXLY_CORRECT=1`, TildeHome | TildeModeUnknown},
		{"exit-trap-before-posix-assignment-in-if", `trap 'printf "%s" S=~/x' EXIT; if true; then POSIXLY_CORRECT=1; fi`, TildeHome | TildeModeUnknown},
		{"exit-trap-function-before-posix-assignment", `f() { printf '%s' S=~/x; }; trap f EXIT; POSIXLY_CORRECT=1`, TildeHome | TildeModeUnknown},
		{"exit-trap-before-function-posix-assignment", `f() { POSIXLY_CORRECT=1; }; trap 'printf "%s" S=~/x' EXIT; f`, TildeHome | TildeModeUnknown},
		{"exit-trap-before-command-posix-prefix", `trap 'printf "%s" S=~/x' EXIT; POSIXLY_CORRECT=1 ls`, TildeHome},
		{"debug-trap-posix-assignment", `trap 'POSIXLY_CORRECT=1' DEBUG; printf '%s' S=~/x`, TildeHome | TildeModeUnknown},
		{"function-posix-assignment", `f() { POSIXLY_CORRECT=1; }; f; printf '%s' S=~/x`, TildeHome | TildeModeUnknown},
		{"loop-posix-assignment", `while true; do printf '%s' S=~/x; POSIXLY_CORRECT=1; done`, TildeHome | TildeModeUnknown},
		{"exit-trap-before-nested-eval-change", `trap 'printf "%s" {~,a}/x' EXIT; eval eval 'set +B'`, TildeHome | TildeModeUnknown},
		{"exit-trap-before-eval-change-past-depth", `trap 'printf "%s" {~,a}/x' EXIT; ` + strings.Repeat("eval ", 20) + `:`, TildeHome | TildeModeUnknown},
		{"exit-trap-before-deep-eval-without-change", `trap 'printf "%s" {~,a}/x' EXIT; ` + strings.Repeat("eval ", 5) + `:`, TildeHome},
		// The trap text inside eval counts toward the depth: seven eval levels
		// in it reach past the limit.
		{"exit-trap-before-trap-text-past-depth", `trap 'printf "%s" {~,a}/x' EXIT; eval "trap '` + strings.Repeat("eval ", 7) + `:' USR1"`, TildeHome | TildeModeUnknown},
		{"exit-trap-before-trap-text-within-depth", `trap 'printf "%s" {~,a}/x' EXIT; eval "trap '` + strings.Repeat("eval ", 5) + `:' USR1"`, TildeHome},
		{"nested-eval-change", strings.Repeat("eval ", 5) + `set +B; printf '%s' {~,a}/x`, 0},
		{"nested-eval-change-past-depth", strings.Repeat("eval ", 20) + `set +B; printf '%s' {~,a}/x`, TildeHome | TildeModeUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls, err := Calls(c.src)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := tildeWordMark(calls)
			if !ok {
				t.Fatalf("no printf of a tilde word in %+v", calls)
			}
			if got != c.want {
				t.Fatalf("Tilde=%d, want %d", got, c.want)
			}
		})
	}
}

// tildeWordMark returns the mark of the last printf argument holding a tilde.
func tildeWordMark(calls []Call) (Tilde, bool) {
	for i := len(calls) - 1; i >= 0; i-- {
		args := calls[i].Args
		if calls[i].Name == "printf" && len(args) > 0 && strings.Contains(args[len(args)-1], "~") {
			return calls[i].ArgTilde(len(args) - 1), true
		}
	}
	return 0, false
}

// TestTildeRuntimeModeFunctionBodies: the words inside a function body get an
// unknown mode when the body runs in more than one mode, through a trap or a
// second call, including the functions it calls.
func TestTildeRuntimeModeFunctionBodies(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want Tilde
	}{
		{"exit-trap-function", `f() { printf '%s' {~,a}/x; }; trap f EXIT; set +B; exit; set -B`, TildeHome | TildeModeUnknown},
		{"exit-trap-function-callee", `g() { printf '%s' {~,a}/x; }; f() { g; }; trap f EXIT; set +B`, TildeHome | TildeModeUnknown},
		{"second-call-callee", `g() { printf '%s' {~,a}/x; }; f() { g; }; f; set +B; f`, TildeHome | TildeModeUnknown},
		{"repeated-call-same-mode", `g() { printf '%s' {~,a}/x; }; f() { g; }; f; f`, TildeHome},
		{"exit-trap-function-without-change", `f() { printf '%s' {~,a}/x; }; trap f EXIT`, TildeHome},
		{"eval-function-sets-exit-trap", `eval 'f() { trap "printf %s {~,a}/x" EXIT; }'; f; set +B`, TildeHome | TildeModeUnknown},
		{"eval-function-called-after-change", `eval 'f() { printf "%s" {~,a}/x; }'; set +B; f`, TildeHome | TildeModeUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls, err := Calls(c.src)
			if err != nil {
				t.Fatal(err)
			}
			var got Tilde
			found := false
			for _, call := range calls {
				if call.Name == "printf" && len(call.Args) == 2 && call.Args[1] == "{~,a}/x" {
					got, found = call.ArgTilde(1), true
				}
			}
			if !found {
				t.Fatalf("no printf of the brace word in %+v", calls)
			}
			if got != c.want {
				t.Fatalf("Tilde=%d, want %d", got, c.want)
			}
		})
	}
}

// TestTildeBeforeBraceGroup: braces after a finished tilde prefix leave the
// prefix as it is; a pattern inside the prefix keeps the tilde.
func TestTildeBeforeBraceGroup(t *testing.T) {
	cases := []struct {
		word string
		want Tilde
	}{
		{"~/{a,b}", TildeHome},
		{"~/x{1,2}", TildeHome},
		{"~/{a,b}/c", TildeHome},
		{"~/.bashrc{,.bak}", TildeHome},
		{"~/a{b,c}{d,e}", TildeHome},
		{"~root/{a,b}", TildeOther},
		{"~+/{a,b}", TildeOther},
		{"~:{a,b}", TildeHome},
		{"a{~,b}", 0},
		{"\\~/{a,b}", 0},
		{"~@(x)", 0},
		{"~@(x|y)", 0},
		{"{b,{{~,a}}}", 0},
		{"~{x}{a,b}/y", 0},
		{"{b,{~,a}{x}}/y", 0},
		{"{\\{,~}/x", TildeHome},
		// ~N and a raw control byte in a user name are syntactic: TildeOther.
		{"{~,a}{1..2}/x", TildeOther},
		{"~a\x01{b,c}/{x}", TildeOther},
		{"{{~,a}", 0},
		{"{b,{~,a}}/x", TildeHome},
		{"{b,{{{~,a}}}}", 0},
		{"S=~/x{a}", TildeHome},
		{"S=~/x{a", TildeHome},
		{"S=~/x{a,b}", 0},
	}
	for _, c := range cases {
		t.Run(c.word, func(t *testing.T) {
			if got := lastTildeArg(t, "printf '%s' "+c.word, 1); got != c.want {
				t.Fatalf("Tilde=%d, want %d", got, c.want)
			}
		})
	}
}

// TestTildeModeWalkIsLinear: doubling the input at most about doubles the time.
func TestTildeModeWalkIsLinear(t *testing.T) {
	callback := func(n int) string {
		var calls, defs strings.Builder
		for i := range n {
			fmt.Fprintf(&calls, "g%d; ", i)
			fmt.Fprintf(&defs, "g%d() { f; }; ", i)
		}
		return "f() { " + calls.String() + "}; " + defs.String() + "f; printf '%s' ~/x"
	}
	cases := []struct {
		name  string
		build func(int) string
		n     int
	}{
		{"braces-nested-in-root-comma", func(n int) string {
			return "printf '%s' {b," + strings.Repeat("{", n) + "~,a" + strings.Repeat("}", n) + "}"
		}, 2000},
		{"calls-of-mode-changing-function", func(n int) string {
			return "f() { set +B; " + strings.Repeat("x0=1; ", n/10) + "}; " + strings.Repeat("f; ", n) + "printf '%s' {~,a}/x"
		}, 1000},
		{"functions-calling-back", callback, 500},
		{"definitions-between-calls", func(n int) string {
			var b strings.Builder
			for i := range n {
				fmt.Fprintf(&b, "f%d() { :; }; f%d; ", i, i)
			}
			return b.String() + "printf '%s' {~,a}/x"
		}, 500},
		{"unclosed-nested-braces", func(n int) string {
			return "printf '%s' ~" + strings.Repeat("{", n) + "a,b"
		}, 2000},
		{"nested-eval", func(n int) string {
			return strings.Repeat("eval ", n) + "set +B; printf '%s' {~,a}/x"
		}, 500},
		{"functions-defined-by-eval", func(n int) string {
			var b strings.Builder
			for i := range n {
				fmt.Fprintf(&b, "eval 'f%d() { set +B; }'; f%d; ", i, i)
			}
			return b.String() + "printf '%s' {~,a}/x"
		}, 500},
		{"computed-command-names", func(n int) string {
			var b strings.Builder
			for i := range n {
				fmt.Fprintf(&b, "f%d() { set +B; }; ", i)
			}
			return b.String() + strings.Repeat("$F; ", n) + "printf '%s' {~,a}/x"
		}, 500},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			measure := func(n int) time.Duration {
				src := c.build(n)
				best := time.Duration(1<<63 - 1)
				for range 3 {
					started := time.Now()
					// Deep eval runs past the expansion budget: Calls returns an
					// error, and the time to it still has to stay linear.
					_, _ = Calls(src)
					best = min(best, time.Since(started))
				}
				return best
			}
			small, large := measure(c.n), measure(4*c.n)
			if large > 8*small+10*time.Millisecond {
				t.Fatalf("four times the input took %s against %s", large, small)
			}
		})
	}
}

// TestTildeModeCommandWithUnreadableWord: a set, shopt or unset word the parser
// cannot read may change a mode; positional arguments of set may not.
func TestTildeModeCommandWithUnreadableWord(t *testing.T) {
	cases := []struct {
		src     string
		unknown bool
	}{
		{`X=+B; set $X; printf '%s' {~,a}/x`, true},
		{`set -o $X; printf '%s' {~,a}/x`, true},
		{`f() { set $X; }; f; printf '%s' {~,a}/x`, true},
		{`X=-u; shopt $X -o braceexpand; printf '%s' {~,a}/x`, true},
		{`set -o posix; unset $X; printf '%s' S=~/x`, true},
		{`set -- $X; printf '%s' {~,a}/x`, false},
		{`set -e "$X"; printf '%s' {~,a}/x`, true},
		{`set x $X; printf '%s' {~,a}/x`, false},
		{`unset $X; printf '%s' {~,a}/x`, false},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			calls, err := Calls(c.src)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := tildeWordMark(calls)
			if !ok {
				t.Fatalf("no printf of a tilde word in %+v", calls)
			}
			if unknown := got&TildeModeUnknown != 0; unknown != c.unknown {
				t.Fatalf("Tilde=%d, want unknown mode %v", got, c.unknown)
			}
		})
	}
}
