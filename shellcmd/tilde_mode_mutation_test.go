package shellcmd

import (
	"fmt"
	"testing"
)

func TestTildeModeMutationForms(t *testing.T) {
	forms := []struct {
		src  string
		want Tilde
	}{
		{"(set +B); printf '%s\\n' {~,a}/x", Tilde(1)},
		{"f() { set +B; }; printf '%s\\n' {~,a}/x", Tilde(1)},
		{"sh -c 'set +B'; printf '%s\\n' {~,a}/x", Tilde(1)},
		{"bash -c 'set -o posix'; printf '%s\\n' S=~/x", Tilde(1)},
		{"f() { set -o posix; }; f; printf '%s\\n' ~/x", Tilde(1)},
		{"f() { set +B; }; f; printf '%s\\n' ~/x", Tilde(1)},
		{"echo $(set +B); printf '%s\\n' {~,a}/x", Tilde(1)},
		{"set +B | cat; printf '%s\\n' {~,a}/x", Tilde(1)},
		{"x=$(set -o posix); printf '%s\\n' S=~/x", Tilde(1)},
		{"f() { set +B; }; (f); printf '%s\\n' {~,a}/x", Tilde(1)},
		{"f() { set +B; }; f | cat; printf '%s\\n' {~,a}/x", Tilde(1)},
		{"[ -n \"$X\" ] && set +B; printf '%s\\n' {~,a}/x", Tilde(5)},
		{"while false; do set +B; done; printf '%s\\n' {~,a}/x", Tilde(5)},
		{"case $X in a) set +B;; esac; printf '%s\\n' {~,a}/x", TildeHome | TildeModeUnknown},
		{"{ set +B; }; printf '%s\\n' {~,a}/x", Tilde(0)},
		{"echo \"$(set +B; printf '%s\\n' {~,a}/x)\"", Tilde(0)},
		{"set +B; f() { set -B; }; f; printf '%s\\n' {~,a}/x", TildeHome | TildeModeUnknown},
		{"if set -o posix; then :; fi; printf '%s\\n' S=~/x", TildeHome | TildeModeUnknown},
		{"set -o posix || true; printf '%s\\n' S=~/x", TildeHome | TildeModeUnknown},
		{"true || set -o posix; printf '%s\\n' S=~/x", Tilde(5)},
		{"POSIXLY_CORRECT=; printf '%s\\n' S=~/x", Tilde(0)},
		{"shopt -uo braceexpand; printf '%s\\n' {~,a}/x", Tilde(0)},
		{"shopt -s posix; printf '%s\\n' S=~/x", Tilde(1)},
		{"! set +B; printf '%s\\n' {~,a}/x", Tilde(0)},
		{"coproc set +B; printf '%s\\n' {~,a}/x", Tilde(1)},
		{"f() { printf '%s\\n' S=~/x; }; f; set -o posix; f", Tilde(5)},
		{"set -o posix; f() { printf '%s\\n' S=~/x; }; f; set +o posix; f", Tilde(5)},
		{"set +B; bash -c \"printf '%s\\n' {~,a}/x\"", Tilde(1)},
		{"set +B; cat < {~,a}", Tilde(0)},
		{"if [ -n \"$X\" ]; then set +B; fi; cat < {~,}", Tilde(5)},
		{"set +B; printf '%s\\n' ~/x", Tilde(1)},
		{"set -o posix; X=~/x; echo $X", Tilde(1)},
		{"set -o posix; export S=~/x; echo $S", Tilde(1)},
		{"f() { set -o posix; }; f; cat <<< a:~/x", Tilde(1)},
		{"f() { set +B; }; f; X={a,~}; echo $X", Tilde(0)},
		{"set +B; printf '%s\\n' {~,a}/x; set -B; printf '%s\\n' {~,a}/y", Tilde(1)},
		{"until true; do set +B; done; printf '%s\\n' {~,a}/x", Tilde(5)},
		{"for i in; do set +B; done; printf '%s\\n' {~,a}/x", Tilde(5)},
		{"select i in; do set +B; done; printf '%s\\n' {~,a}/x", Tilde(5)},
		{"f() { local -; set +B; }; f; printf '%s\\n' {~,a}/x", TildeHome | TildeModeUnknown},
		{"set -o posix; printf '%s\\n' S=~/x >/dev/null; printf '%s\\n' a:~/x S=~/y", Tilde(0)},
		{"set -o posix; printf '%s\\n' \"$(printf '%s\\n' S=~/x)\"", Tilde(0)},
		{"set -o posix; x=$(set +o posix; printf '%s\\n' S=~/x); echo $x", Tilde(1)},
		{"set +o posix; f() { set -o posix; }; [ -z \"$X\" ] || f; printf '%s\\n' S=~/x", Tilde(5)},
		{"set -o posix 2>/dev/null; printf '%s\\n' S=~/x", Tilde(0)},
		{"set -B +B; printf '%s\\n' {~,a}/x", Tilde(0)},
		{"set -o braceexpand +o braceexpand; printf '%s\\n' {~,a}/x", Tilde(0)},
		{"set -uo braceexpand; printf '%s\\n' {~,a}/x", Tilde(1)},
		{"set +uo braceexpand; printf '%s\\n' {~,a}/x", Tilde(0)},
		{"set -o posix; printf '%s\\n' S=~/x{a,b}", Tilde(0)},
		{"set +B; printf '%s\\n' S=~/x{a,b}", Tilde(1)},
		{"set +B; set -o posix; printf '%s\\n' S=~/x{a,b}", Tilde(0)},
		{"declare S={a,b:~/x}; echo \"$S\"", Tilde(0)},
		{"set +B; declare S=~/x{a,b}; echo \"$S\"", Tilde(1)},
		{"export S=a:{b,~/x}; echo \"$S\"", Tilde(0)},
		{"set +B; export S=a:{b,~/x}; echo \"$S\"", Tilde(0)},
		{"S=x:{~,a}; echo \"$S\"", Tilde(0)},
		{"S=x:{a,~}/y; echo \"$S\"", Tilde(0)},
		{"S={~/x}; echo \"$S\"", Tilde(0)},
		{"printf '%s\\n' S={a,b:~/x}", Tilde(0)},
		{"printf '%s\\n' S=x:{a,~}/y", Tilde(0)},
		{"printf '%s\\n' x{a,b}=~/y", Tilde(0)},
		{"f() { set +B; }; f; declare S=~/x{a,b}; echo \"$S\"", TildeHome | TildeModeUnknown},
		{"S+={~,a}:~/x; echo \"$S\"", Tilde(1)},
		{"cat < ~{,}", Tilde(0)},
		{"cat < {,~}", Tilde(1)},
		{"f() ( set +B; ); f; printf '%s\\n' {~,a}/x", TildeHome},
		{"f() { set +B; }; f & wait; printf '%s\\n' {~,a}/x", TildeHome},
		{"f() { set +B; }; x=$(f); printf '%s\\n' {~,a}/x", Tilde(1)},
		{"f() { set +B; }; command f; printf '%s\\n' {~,a}/x", Tilde(1)},
		{"POSIXLY_CORRECT=1 set -o posix; printf '%s\\n' S=~/x", Tilde(0)},
		{"POSIXLY_CORRECT=1 eval true; printf '%s\\n' S=~/x", Tilde(0)},
		{"POSIXLY_CORRECT=1 :; printf '%s\\n' S=~/x", Tilde(0)},
		{"unset -v POSIXLY_CORRECT; set -o posix; printf '%s\\n' S=~/x", Tilde(0)},
		{"set -o posix; set +o posix; f() { set -o posix; }; printf '%s\\n' S=~/x", Tilde(1)},
		{"set +B; printf '%s\\n' \"$(set -B; printf '%s\\n' {~,a}/x)\"", Tilde(1)},
		{"set +B; (set -B; printf '%s\\n' {~,a}/x)", Tilde(1)},
		{"set +B; { set -B; }; printf '%s\\n' {~,a}/x", Tilde(1)},
		{"set +B; while read -r l; do printf '%s\\n' {~,a}/x; done <<< a", Tilde(0)},
		{"set +B; printf '%s\\n' {~,a}/x | cat", Tilde(0)},
		{"set +B; cat < {~,a}x", Tilde(0)},
		{"true && { set +B; printf '%s\\n' {~,a}/x; }", Tilde(0)},
		{"if true; then set +B; printf '%s\\n' {~,a}/x; fi", Tilde(0)},
		{"while true; do set +B; printf '%s\\n' {~,a}/x; break; done", Tilde(0)},
		{"printf '%s\\n' ~:x/}", Tilde(1)},
		{"printf '%s\\n' a:~:x}", Tilde(0)},
		{"cat <<< a:~:x}", Tilde(1)},
		{"f() { set --posix -o posix; }; f; printf '%s\\n' S=~/x", Tilde(1)},
		{"set -o posix; shopt -uo posix; printf '%s\\n' S=~/x", Tilde(1)},
		{"command export POSIXLY_CORRECT=1; printf '%s\\n' S=~/x", Tilde(0)},
		{"sh -c 'case x in x) printf \"%s\\n\" S=~/x;; esac'", Tilde(0)},
		{"bash +B -c 'case x in x) printf \"%s\\n\" {~,a}/x;; esac'", Tilde(0)},
		{"if [ -n \"$X\" ]; then set +B; fi; cat < {~,a}", Tilde(0)},
		{"cat < {,{~,a}}", Tilde(0)},
		{"printf '%s\\n' {~,a}\\\n/x", Tilde(1)},
		{"declare S=a}:{b,:~/x}; echo \"$S\"", Tilde(0)},
		{"printf '%s\\n' a[x]]=~/y", Tilde(0)},
		{"{ { { { { { { set +B; }; }; }; }; }; }; }; printf '%s\\n' {~,a}/x", Tilde(0)},
		{"set +B; f() { printf '%s\\n' {~,a}/x; }; f; set -B; f", Tilde(5)},
		{"set +B; f() { set -B; }; f; cat < {~,a}", Tilde(0)},
		{"if [ -n \"$X\" ]; then set +B; fi; cat < ~/{a,b}", TildeModeUnknown},
		{"set +B; cat < ~/{a,b}", Tilde(1)},
		{"f() { set - +B; }; f; printf '%s\\n' {~,a}/x", Tilde(1)},
		{"f() { set x +B; }; f; printf '%s\\n' {~,a}/x", Tilde(1)},
		{"set +B && printf '%s\\n' {~,a}/x", TildeHome | TildeModeUnknown},
		{"eval 'set +B; if'; printf '%s\\n' {~,a}/x", Tilde(5)},
	}
	for i, form := range forms {
		t.Run(fmt.Sprintf("form-%03d", i+1), func(t *testing.T) {
			if got := tildeMutationMarks(t, form.src); got != form.want {
				t.Fatalf("got %d, want %d for %q", got, form.want, form.src)
			}
		})
	}
}

func tildeMutationMarks(t *testing.T, src string) Tilde {
	t.Helper()
	var marks Tilde
	calls, _ := Calls(src)
	for _, call := range calls {
		for i := range call.Args {
			marks |= call.ArgTilde(i)
		}
		for _, redir := range call.Redirs {
			marks |= redir.Tilde
		}
	}
	assigns, _ := Assigns(src)
	for _, assign := range assigns {
		marks |= assign.Tilde
	}
	return marks
}
