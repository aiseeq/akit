package shellcmd

import (
	"strings"
	"testing"
	"time"
)

func TestTildeMarks(t *testing.T) {
	tests := []struct {
		name, source string
		want         Tilde
	}{
		{"arg-home", `printf '%s' ~/x`, TildeHome},
		{"arg-bare-home", `printf '%s' ~`, TildeHome},
		{"arg-double-quoted", `printf '%s' "~/x"`, 0},
		{"arg-single-quoted", `printf '%s' '~/x'`, 0},
		{"arg-escaped", `printf '%s' \~/x`, 0},
		{"arg-tilde-quoted", `printf '%s' ~"/x"`, 0},
		{"arg-prefix-quoted", `printf '%s' "~"/x`, 0},
		{"arg-slash-escaped", `printf '%s' ~\\/x`, 0},
		{"arg-suffix-quoted", `printf '%s' ~/"x"`, TildeHome},
		{"arg-user", `printf '%s' ~root/x`, TildeOther},
		{"arg-plus", `printf '%s' ~+/x`, TildeOther},
		{"arg-plus-stack-index", `printf '%s' ~+1/x`, TildeOther},
		{"arg-minus", `printf '%s' ~-/x`, TildeOther},
		{"arg-minus-stack-index", `printf '%s' ~-2/x`, TildeOther},
		{"arg-user-index", `printf '%s' ~1/x`, TildeOther},
		{"arg-middle", `printf '%s' a~/x`, 0},
		{"arg-short-option-attached", `printf '%s' -T~/x`, 0},
		{"arg-at-prefix", `printf '%s' -d @~/x`, 0},
		{"arg-quoted-at-prefix", `printf '%s' -d @"~/x"`, 0},
		{"arg-option", `printf '%s' --opt=~/x`, 0},
		{"arg-url", `printf '%s' --data-binary=@~/x`, 0},
		{"arg-assignment", `printf '%s' S=~/x`, TildeHome},
		{"arg-assignment-colon", `printf '%s' S=a:~/x`, TildeHome},
		{"arg-tilde-colon-terminator", `printf '%s' ~:x`, TildeHome},
		{"arg-assignment-colons", `printf '%s' a=~:~/x`, TildeHome},
		{"arg-assignment-append", `printf '%s' a+=~/x`, TildeHome},
		{"arg-assignment-index", `printf '%s' a[0]=~/x`, TildeHome},
		{"arg-assignment-empty-index", `printf '%s' a[]=~/x`, TildeHome},
		{"arg-assignment-invalid-index", `printf '%s' a[x=1]=~/x`, 0},
		{"arg-assignment-index-append", `printf '%s' a[0]+=~/x`, TildeHome},
		{"arg-quoted-user-prefix", `printf '%s' ~ro"o"t/x`, 0},
		{"arg-quoted-user-slash", `printf '%s' ~root"/x"`, 0},
		{"arg-escaped-user-slash", `printf '%s' ~root\/x`, 0},
		{"arg-param-default", `printf '%s' ${x:-~}`, TildeHome},
		{"arg-process-substitution-before", `printf '%s' > >(cat) ~/x`, TildeHome},
		{"arg-command-substitution-before", `printf '%s' "$(echo a b)" ~/x`, TildeHome},
		{"arg-backquote-prefix", "printf '%s' ~`echo`", 0},
		{"arg-brace-prefix", `printf '%s' ~{x}/y`, 0},
		{"arg-single-quote-prefix", `printf '%s' ~'u'`, 0},
		{"arg-underscore-assignment", `printf '%s' _S=~/x`, TildeHome},
		{"arg-digit-first-assignment", `printf '%s' 1S=~/x`, 0},
		{"arg-digit-assignment", `printf '%s' S1=~/x`, TildeHome},
		{"arg-dash-assignment", `printf '%s' a-b=~/x`, 0},
		{"arg-heredoc-body", "printf '%s' a <<EOF ~/x\nbody\nEOF", TildeHome},
		{"arg-posix-assignment", `set -o posix; printf '%s' S=~/x`, 0},
		{"arg-posix-disabled-again", `set -o posix; set +o posix; printf '%s' S=~/x`, TildeHome},
		{"arg-colon-data", `printf '%s' x:~/y`, 0},
		{"arg-dynamic-user", `printf '%s' ~$U/x`, 0},
		{"redir-home", `cat < ~/x`, TildeHome},
		{"redir-quoted", `cat < "~/x"`, 0},
		{"redir-escaped", `cat < \~/x`, 0},
		{"redir-user", `cat < ~root/x`, TildeOther},
		{"redir-plus", `cat < ~+/x`, TildeOther},
		{"redir-output", `printf x > ~/x`, TildeHome},
		{"redir-dup-has-no-target-tilde", `printf x 2> ~/x >&2`, 0},
		{"redir-here-string-quoted", `cat <<< "~/x"`, 0},
		{"redir-here-string-colon", `cat <<< a:~`, TildeHome},
		{"redir-here-string", `cat <<< ~/x`, TildeHome},
		{"redir-ordinary-colon", `cat < a:~/x`, 0},
		{"redir-here-doc-delimiter", "cat <<~\nx\n~", 0},
		{"assign-home", `S=~/x`, TildeHome},
		{"assign-quoted", `S="~/x"`, 0},
		{"assign-colon", `S=a:~root/x:~/y`, TildeHome | TildeOther},
		{"assign-colon-terminator", `S=~:x`, TildeHome},
		{"assign-empty-quoted-segment", `S=a:""~/x`, 0},
		{"assign-escaped-colon", `S=a\:~/x`, 0},
		{"assign-single-quote-in-double", `S=a:"'":~/x`, TildeHome},
		{"assign-backslash-in-single", "S='\\':~/x", TildeHome},
		{"assign-declare-colons", `declare S=~:~/x`, TildeHome},
		{"assign-export-colon", `export S=~:x`, TildeHome},
		{"assign-line-continuation", "S=\\\n~/x", TildeHome},
		{"assign-quoted-tilde", `S=a:"~"/x`, 0},
		{"assign-equals", `S=a=~/x`, 0},
		{"append", `S+=~/x`, TildeHome},
		{"export", `export S=~/x`, TildeHome},
		{"declare", `declare S=~/x`, TildeHome},
		{"readonly", `readonly S=~/x`, TildeHome},
		{"local", `f() { local S=~/x; }`, TildeHome},
		{"nested-script", `bash -c 'printf "%s" ~/x'`, TildeHome},
		{"nested-quoted-script", `bash -c 'printf "%s" "~/x"'`, 0},
		{"arg-brace-start", `printf '%s' {~,a}/x`, TildeHome},
		{"arg-brace-user", `printf '%s' ~{root,}/x`, TildeHome | TildeOther},
		{"arg-brace-nested", `printf '%s' {{~,a},b}/x`, TildeHome},
		{"arg-brace-malformed-prefix", `printf '%s' ~{x/y`, 0},
		{"arg-brace-quoted-comma", `printf '%s' {a\,~,b}`, 0},
		{"arg-colon-then-quoted", `printf '%s' ~:"x"`, 0},
		{"arg-colon-then-escaped", `printf '%s' ~:\x`, 0},
		{"arg-colon-then-parameter", `printf '%s' ~:$HOME`, TildeHome},
		{"arg-user-colon-then-quoted", `printf '%s' ~root:"x"`, 0},
		{"arg-empty-command-word", `P=""; $P printf '%s' a ~/x`, TildeHome},
		{"arg-split-command-word", `P="printf %s\n"; $P a ~/x`, TildeHome},
		{"arg-posix-echo-is-text", `echo "set -o posix"; printf '%s' S=~/x`, TildeHome},
		{"arg-posix-subshell-does-not-leak", `(set -o posix); printf '%s' S=~/x`, TildeHome},
		{"arg-posix-short-options", `set -eo posix; printf '%s' S=~/x`, 0},
		{"arg-posix-multiple-o", `set -o errexit -o posix; printf '%s' S=~/x`, 0},
		{"arg-posix-double-space", `set -o  posix; printf '%s' S=~/x`, 0},
		{"arg-posix-comment-is-text", "# set -o posix\nprintf '%s' S=~/x", TildeHome},
		{"arg-posix-function-not-run", `f() { set -o posix; }; printf '%s' S=~/x`, TildeHome},
		{"arg-nested-sh-posix", `sh -c 'printf "%s" S=~/x'`, 0},
		{"arg-nested-bash-posix", `bash --posix -c 'printf "%s" S=~/x'`, 0},
		{"arg-nested-bash-o-posix", `bash -o posix -c 'printf "%s" S=~/x'`, 0},
		{"arg-nested-posix-env", `POSIXLY_CORRECT=1 bash -c 'printf "%s" S=~/x'`, 0},
		{"arg-brace-disabled", `set +B; printf '%s' {~,a}/x`, 0},
		{"redir-colon-then-quoted", `cat < ~:"x"`, 0},
		{"redir-braces-no-expansion", `cat < {~,a}`, 0},
		{"redir-here-string-braces-no-expansion", `cat <<< {~,a}`, 0},
		{"redir-here-doc-dash", "cat <<-~\n\tx\n\t~", 0},
		{"arg-param-colon", `printf '%s' ${x:-a:~}`, 0},
		{"arg-param-prefix-tilde", `printf '%s' ${x:-~/y}`, TildeHome},
		{"arg-param-default-assignment", `printf '%s' S=${x:-~}`, TildeHome},
		{"arg-param-colon-assignment", `S=${x:-a:~}`, TildeHome},
		{"arg-param-empty-colon-assignment", `S=${x:-:~/y}`, TildeHome},
		{"arg-assignment-no-brace-expansion", `S={~,a}/x`, 0},
		{"arg-assignment-brace-after-colon", `S=a:{~,b}`, 0},
		{"arg-assignment-prefix-quoted", `S=~:"x"`, TildeHome},
		{"assign-posix-colon", `set -o posix; S=a:~/x`, TildeHome},
		{"arg-heredoc-tilde-last", "printf '%s' a <<EOF ~/x\nbody\nEOF", TildeHome},
		{"arg-here-doc-body", "cat <<EOF ~/x\nbody\nEOF", TildeHome},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			switch tt.name {
			case "assign-home", "assign-quoted", "assign-colon", "assign-colon-terminator", "assign-empty-quoted-segment", "assign-escaped-colon", "assign-single-quote-in-double", "assign-backslash-in-single", "assign-declare-colons", "assign-export-colon", "assign-line-continuation", "assign-quoted-tilde", "assign-equals", "append", "export", "declare", "readonly", "local", "arg-param-colon-assignment", "arg-param-empty-colon-assignment", "arg-assignment-no-brace-expansion", "arg-assignment-brace-after-colon", "arg-assignment-prefix-quoted", "assign-posix-colon":
				assigns, err := Assigns(tt.source)
				if err != nil {
					t.Fatal(err)
				}
				for _, a := range assigns {
					if a.Name == "S" {
						if a.Tilde != tt.want {
							t.Fatalf("Tilde=%d, want %d", a.Tilde, tt.want)
						}
						return
					}
				}
				t.Fatal("assignment S not found")
			default:
				calls, err := Calls(tt.source)
				if err != nil {
					t.Fatal(err)
				}
				for i := len(calls) - 1; i >= 0; i-- {
					if tt.name == "nested-script" || tt.name == "nested-quoted-script" {
						if calls[i].Name == "printf" && len(calls[i].Args) > 0 {
							got := calls[i].ArgTilde(len(calls[i].Args) - 1)
							if got != tt.want {
								t.Fatalf("ArgTilde=%d, want %d", got, tt.want)
							}
							return
						}
					} else if strings.HasPrefix(tt.name, "redir-") {
						if len(calls[i].Redirs) > 0 {
							got := calls[i].Redirs[len(calls[i].Redirs)-1].Tilde
							if got != tt.want {
								t.Fatalf("Redir.Tilde=%d, want %d", got, tt.want)
							}
							return
						}
					} else if (calls[i].Name == "printf" || tt.name == "arg-here-doc-body" && calls[i].Name == "cat") && len(calls[i].Args) > 0 {
						got := calls[i].ArgTilde(len(calls[i].Args) - 1)
						if got != tt.want {
							t.Fatalf("ArgTilde=%d, want %d", got, tt.want)
						}
						return
					}
				}
				t.Fatal("matching call not found")
			}
		})
	}
}

func TestArgTildeUsesExactWord(t *testing.T) {
	cases := []struct {
		source string
		name   string
		index  int
		want   Tilde
	}{
		{`cat a b > >(tee ~/y c) ~/nd9/x`, "cat", 1, 0},
		{`cat > >(x=1) ~/nd9/x`, "cat", 0, TildeHome},
		{`cat a b > >(tee ~/y c) ~/nd9/x`, "cat", 0, 0},
		{`find /var/tmp -maxdepth 0 -exec printf '%s\n' ~/x \;`, "printf", 1, TildeHome},
		{`env ~/bin/t ~/x`, "t", -1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name+tc.source, func(t *testing.T) {
			calls, err := Calls(tc.source)
			if err != nil {
				t.Fatal(err)
			}
			for i := len(calls) - 1; i >= 0; i-- {
				if calls[i].Name == tc.name {
					if got := calls[i].ArgTilde(tc.index); got != tc.want {
						t.Fatalf("ArgTilde(%d)=%d, want %d; call=%+v", tc.index, got, tc.want, calls[i])
					}
					return
				}
			}
			t.Fatalf("call %q not found: %+v", tc.name, calls)
		})
	}
}

func TestArgTildeBeyondPackedRange(t *testing.T) {
	args := append([]string{`printf '%s'`}, make([]string, 31)...)
	for i := 1; i < len(args); i++ {
		args[i] = "x"
	}
	args = append(args, "~/x")
	calls, err := Calls(strings.Join(args, " "))
	if err != nil || len(calls) != 1 {
		t.Fatalf("Calls: %v, %d calls", err, len(calls))
	}
	if got := calls[0].ArgTilde(32); got != TildeHome {
		t.Fatalf("ArgTilde(32)=%d, want %d; args=%d raw=%q", got, TildeHome, len(calls[0].Args), calls[0].Raw)
	}
	if calls[0].ArgTilde(-1) != 0 || calls[0].ArgTilde(len(calls[0].Args)) != 0 {
		t.Fatal("out-of-range ArgTilde must return zero")
	}
}

func TestTildeBraceScanHasBoundedWork(t *testing.T) {
	measure := func(groups int) time.Duration {
		source := `printf '%s' ` + strings.Repeat("{a,b}", groups) + `~/x`
		best := time.Duration(1<<63 - 1)
		for range 3 {
			started := time.Now()
			calls, err := Calls(source)
			elapsed := time.Since(started)
			if err != nil || len(calls) != 1 {
				t.Fatalf("Calls: %v, count %d", err, len(calls))
			}
			if got := calls[0].ArgTilde(0); got != 0 {
				t.Fatalf("ArgTilde=%d, want 0", got)
			}
			if elapsed < best {
				best = elapsed
			}
		}
		return best
	}
	small, large := measure(1000), measure(2000)
	if large > 4*small+5*time.Millisecond {
		t.Fatalf("doubling brace groups grew too much: %s to %s", small, large)
	}
}

func TestTildeExpansionAdditionalForms(t *testing.T) {
	tests := []struct {
		name, source, kind string
		want               Tilde
	}{
		{"continuation-home-arg", "printf '%s' ~\\\n/x", "arg", TildeHome},
		{"continuation-assignment", "S=~\\\n/x", "assign", TildeHome},
		{"continuation-assignment-arg", "printf '%s' S=a:~\\\n/x", "arg", TildeHome},
		{"continuation-user-prefix", "printf '%s' ~ro\\\not/x", "arg", TildeOther},
		{"colon-quote-inside-prefix", `printf '%s' ~:x"y"`, "arg", 0},
		{"colon-escape-inside-prefix", `printf '%s' ~:x\y`, "arg", 0},
		{"colon-double-separator-quote", `printf '%s' ~::"x"`, "arg", 0},
		{"colon-second-segment-quote", `printf '%s' ~:x:"y"`, "arg", 0},
		{"colon-escaped-colon", `printf '%s' ~:a\:b`, "arg", 0},
		{"colon-single-quote-prefix", `printf '%s' ~:x'y'/z`, "arg", 0},
		{"colon-parameter-expansion", `printf '%s' ~:$(echo a)`, "arg", TildeHome},
		{"colon-command-substitution", "printf '%s' ~:`echo a`", "arg", TildeHome},
		{"colon-quote-in-brace-alt", `printf '%s' {~:x\"y,a}`, "arg", 0},
		{"colon-escape-in-brace-alt", `printf '%s' {~:\x,a}`, "arg", 0},
		{"parameter-replacement", `printf '%s' ${x/a/~}`, "arg", TildeHome},
		{"parameter-replacement-prefix", `printf '%s' ${x/#a/~/y}`, "arg", TildeHome},
		{"quoted-parameter-replacement", `printf '%s' "${x/a/~}"`, "arg", TildeHome},
		{"brace-disabled-long-option", `set +o braceexpand; printf '%s' {~,a}/x`, "arg", 0},
		{"brace-disabled-combined", `set +eB; printf '%s' {~,a}/x`, "arg", 0},
		{"brace-disabled-after-other", `set -o pipefail +B; printf '%s' {~,a}/x`, "arg", 0},
		{"brace-disabled-nested-bash", `bash +B -c 'printf "%s" {~,a}/x'`, "arg", 0},
		{"brace-disabled-subshell", `set +B; (printf '%s' {~,a}/x)`, "arg", 0},
		{"brace-disabled-if", `set +B; if true; then printf '%s' {~,a}/x; fi`, "arg", 0},
		{"brace-disabled-function", `set +B; f() { printf '%s' {~,a}/x; }; f`, "arg", 0},
		{"declare-assignment-literal-braces", `declare S={a,b:~/x}`, "assign", 0},
		{"brace-redir-home-empty-last", `cat < {~,}`, "redir", TildeHome},
		{"brace-redir-path-empty-last", `cat < {~/x,}`, "redir", TildeHome},
		{"brace-redir-path-empty-first", `cat < {,~/x}`, "redir", TildeHome},
		{"brace-redir-disabled", `set +B; cat < {~,}`, "redir", 0},
		{"redir-colon-quote", `cat < ~:x"y"`, "redir", 0},
		{"posix-set-in-called-function", `f() { set -o posix; }; f; printf '%s' S=~/x`, "arg", TildeHome | TildeModeUnknown},
		{"brace-set-in-called-function", `f() { set +B; }; f; printf '%s' {~,a}/x`, "arg", TildeHome | TildeModeUnknown},
		{"posix-if-state-unknown", `if true; then set -o posix; fi; printf '%s' S=~/x`, "arg", TildeHome | TildeModeUnknown},
		{"brace-if-state-unknown", `if true; then set +B; fi; printf '%s' {~,a}/x`, "arg", TildeHome | TildeModeUnknown},
		{"posix-and-state-unknown", `true && set -o posix; printf '%s' S=~/x`, "arg", TildeHome | TildeModeUnknown},
		{"posix-brace-group", `{ set -o posix; }; printf '%s' S=~/x`, "arg", 0},
		{"posix-subshell-does-not-leak", `set -o posix; (printf '%s' S=~/x); printf '%s' S=~/x`, "arg", 0},
		{"posix-pipeline", `set -o posix; printf '%s' S=~/x | cat`, "arg", 0},
		{"posix-if-body", `set -o posix; if true; then printf '%s' S=~/x; fi`, "arg", 0},
		{"posix-for-body", `set -o posix; for i in 1; do printf '%s' S=~/x; done`, "arg", 0},
		{"posix-and-body", `set -o posix; true && printf '%s' S=~/x`, "arg", 0},
		{"posix-function-body-known", `set -o posix; f() { printf '%s' S=~/x; }; f`, "arg", 0},
		{"posix-does-not-inherit-bash", `set -o posix; bash -c 'printf "%s" S=~/x'`, "arg", TildeHome},
		{"posix-shopt", `shopt -os posix; printf '%s' S=~/x`, "arg", 0},
		{"posix-combined-option", `set -oe posix; printf '%s' S=~/x`, "arg", 0},
		{"posix-double-dash-is-operand", `set --posix; printf '%s' S=~/x`, "arg", TildeHome},
		{"posix-attached-name-is-operand", `set -oposix; printf '%s' S=~/x`, "arg", TildeHome},
		{"posix-builtin-set", `builtin set -o posix; printf '%s' S=~/x`, "arg", 0},
		{"posix-command-set", `command set -o posix; printf '%s' S=~/x`, "arg", 0},
		{"posix-escaped-command-name", `s\et -o posix; printf '%s' S=~/x`, "arg", 0},
		{"posix-eval-literal", `eval 'set -o posix'; printf '%s' S=~/x`, "arg", 0},
		{"posix-variable-set", `POSIXLY_CORRECT=1; printf '%s' S=~/x`, "arg", 0},
		{"posix-variable-export", `export POSIXLY_CORRECT=1; printf '%s' S=~/x`, "arg", 0},
		{"posix-empty-env-bash", `POSIXLY_CORRECT= bash -c 'printf "%s" S=~/x'`, "arg", 0},
		{"posix-env-prefix-bash", `env POSIXLY_CORRECT=1 bash -c 'printf "%s" S=~/x'`, "arg", 0},
		{"posix-exec-argv0-sh", `exec -a sh bash -c 'printf "%s" S=~/x'`, "arg", 0},
		{"posix-exec-argv0-bash", `exec -a bash sh -c 'printf "%s" S=~/x'`, "arg", TildeHome},
		{"posix-option-after-command", `bash -c 'printf "%s" S=~/x' --posix`, "arg", TildeHome},
		{"array-index-tilde-escaped", `printf '%s' a[\~]=~/x`, "arg", TildeHome},
		{"array-index-tilde-colon", `printf '%s' a[\~]=x:~/y`, "arg", TildeHome},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got Tilde
			switch tt.kind {
			case "assign":
				assigns, err := Assigns(tt.source)
				if err != nil {
					t.Fatal(err)
				}
				if len(assigns) == 0 {
					t.Fatal("no assignment")
				}
				got = assigns[0].Tilde
			case "redir":
				calls, err := Calls(tt.source)
				if err != nil || len(calls) == 0 || len(calls[len(calls)-1].Redirs) == 0 {
					t.Fatalf("Calls: %v", err)
				}
				redirs := calls[len(calls)-1].Redirs
				got = redirs[len(redirs)-1].Tilde
			case "decl":
				calls, err := Calls(tt.source)
				if err != nil || len(calls) == 0 || len(calls[len(calls)-1].Args) == 0 {
					t.Fatalf("Calls: %v", err)
				}
				got = calls[len(calls)-1].ArgTilde(len(calls[len(calls)-1].Args) - 1)
			default:
				calls, err := Calls(tt.source)
				if err != nil {
					t.Fatal(err)
				}
				for i := len(calls) - 1; i >= 0; i-- {
					if calls[i].Name == "printf" && len(calls[i].Args) > 0 {
						got = calls[i].ArgTilde(len(calls[i].Args) - 1)
						break
					}
				}
			}
			if got != tt.want {
				t.Fatalf("Tilde=%d, want %d", got, tt.want)
			}
		})
	}
}
