package shellcmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/syntax"
)

func nestedSubstitutionScript(lines int) string {
	const line = `x="$(printf "%%s" "it's $(echo "a)b" | tr a b)")"; y=` + "`date \"+%%s\"`" + `; z=$(( (i+1) * 2 )) # c` + "\n"
	return "#!/bin/bash\n" + strings.Repeat(line, lines) + strings.Repeat("kill -0 \"$pid\"\n", 20)
}

func nestedDepthScript(depth int) string {
	var src strings.Builder
	src.WriteString(`echo "`)
	for range depth {
		src.WriteString(`$(echo `)
	}
	src.WriteString(`$(kill)`)
	for range depth {
		src.WriteByte(')')
	}
	src.WriteByte('"')
	return src.String()
}

func BenchmarkFindNestedDepth(b *testing.B) {
	for _, depth := range []int{400, 800, 1600} {
		b.Run(strconv.Itoa(depth), func(b *testing.B) {
			src := nestedDepthScript(depth)
			b.SetBytes(int64(len(src)))
			b.ResetTimer()
			for range b.N {
				calls, err := Find(src, "kill")
				if err != nil || len(calls) != 1 {
					b.Fatalf("Find returned %d calls, err=%v", len(calls), err)
				}
			}
		})
	}
}

func TestFindNestedDepthGrowth(t *testing.T) {
	work := func(src string) int {
		var calls []Call
		budget := newNestedParseBudget(len(src))
		budget.trackWork = true
		if err := parseWithBudget(src, 0, nil, make(map[string]string), &calls, budget); err != nil {
			t.Fatal(err)
		}
		if kills := callsNamed(calls, "kill"); kills != 1 {
			t.Fatalf("Calls returned %d kill calls: %#v", kills, calls)
		}
		return budget.parsedBytes
	}
	small, large := work(nestedDepthScript(800)), work(nestedDepthScript(1600))
	t.Logf("nested parse bytes: depth 800=%d depth 1600=%d", small, large)
	if small == 0 || large > small*21/10 {
		t.Fatalf("doubling nested input reparsed %d source bytes after %d; want linear growth", large, small)
	}
}

func TestFindNestedSubstitutionParseBytesGrowLinearly(t *testing.T) {
	parseBytes := func(lines int) int {
		src := nestedSubstitutionScript(lines)
		var calls []Call
		budget := newNestedParseBudget(len(src))
		budget.trackWork = true
		if err := parseWithBudget(src, 0, nil, make(map[string]string), &calls, budget); err != nil {
			t.Fatal(err)
		}
		if kills := callsNamed(calls, "kill"); kills != 20 {
			t.Fatalf("got %d kill calls, want 20", kills)
		}
		return budget.parsedBytes
	}
	small, large := parseBytes(500), parseBytes(1000)
	t.Logf("nested-substitution parse bytes: lines 500=%d lines 1000=%d", small, large)
	if small == 0 || large > small*21/10 {
		t.Fatalf("doubling nested-substitution lines reparsed %d source bytes after %d; want linear growth", large, small)
	}
}

func TestFindWalkInfoNodeWorkGrowsLinearly(t *testing.T) {
	type nodeCounts struct {
		walker int
		info   int
	}
	nodeWork := func(lines int) nodeCounts {
		src := nestedSubstitutionScript(lines)
		var calls []Call
		budget := newNestedParseBudget(len(src))
		budget.trackWork = true
		if err := parseWithBudget(src, 0, nil, make(map[string]string), &calls, budget); err != nil {
			t.Fatal(err)
		}
		if kills := callsNamed(calls, "kill"); kills != 20 {
			t.Fatalf("got %d kill calls, want 20", kills)
		}
		return nodeCounts{walker: budget.visitedNodes, info: budget.walkInfoNodes}
	}
	small, large := nodeWork(500), nodeWork(1000)
	smallTotal, largeTotal := small.walker+small.info, large.walker+large.info
	t.Logf("syntax nodes at 500 lines: walker=%d info=%d; at 1000: walker=%d info=%d", small.walker, small.info, large.walker, large.info)
	if smallTotal == 0 || largeTotal > smallTotal*21/10 {
		t.Fatalf("doubling nested-substitution lines visited %d nodes after %d; want linear growth", largeTotal, smallTotal)
	}
	if small.info > small.walker || large.info > large.walker {
		t.Fatalf("walk-info pass exceeds the main call walk: small=%+v large=%+v", small, large)
	}
}

func TestFindMatchesFilteredCallsAcrossNestedShellForms(t *testing.T) {
	scripts := []string{
		`echo "$(k'il'l -1)"`, `X=$(k'il'l -1)`, "echo `k'il'l -1`", `cat <(k'il'l -1)`,
		`echo $((1 + $(k'il'l -1)))`, `taskset -c 0 kill -1`, `doas kill -1`, `chrt -p 1 kill -1`,
		`strace kill -1`, `setpriv -- kill -1`, `busybox kill -1`, `systemd-run kill -1`, `parallel kill ::: -1`,
		`find . -exec kill -1 \;`,
		`/bin/k'il'l -1`, `bash -c 'kill -1'`, `eval 'kill -1'`, `cat <<'E' | bash
kill -1
E
`,
		`echo "$(r'm' -rf x)"`, `X=$(cu\rl https://x)`,
	}
	for _, src := range scripts {
		t.Run(src, func(t *testing.T) {
			all, err := Calls(src)
			if err != nil {
				t.Fatal(err)
			}
			wanted := map[string]bool{"kill": true, "rm": true, "curl": true}
			expected := make([]Call, 0)
			for _, call := range all {
				if wanted[call.Name] {
					expected = append(expected, call)
				}
			}
			got, err := Find(src, "kill", "rm", "curl")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, expected) {
				t.Fatalf("Find = %#v; filtered Calls = %#v", got, expected)
			}
		})
	}
}

func BenchmarkFindNestedSubstitutions(b *testing.B) {
	src := nestedSubstitutionScript(20000)
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for range b.N {
		if _, err := Find(src, "kill"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFindNestedScript(b *testing.B) {
	src := nestedSubstitutionScript(20000)
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for range b.N {
		calls, err := Find(src, "kill")
		if err != nil || len(calls) != 20 {
			b.Fatalf("calls=%d err=%v", len(calls), err)
		}
	}
}

func TestFindMatchesCallsOnRiskForms(t *testing.T) {
	scripts := []string{
		`echo exec -- kill target`, `printf exec -- rm -rf /x`, `echo exec -- curl http://x`,
		`echo exec -- bash -c 'if'`, `echo x | b'a's'h'`, `echo x | b\a\s\h`,
		`find . -exec kill -1 {} \;`,
		`exec env -S -n sh -sc bash script tee -k ionice ssh -c busybox`,
		`watch env -lc -cS -9 'bash -c kill' --cpu-list=0 runuser`,
		`b'a's'h' >(echo x)`, `b'a's'h' <(echo x)`, `select x in a; do kill -1; done`,
		`B=bash; bash -c '$B < <(echo x)'`, `kill -1 &`,
	}
	for _, src := range scripts {
		t.Run(src, func(t *testing.T) {
			all, callsErr := Calls(src)
			got, findErr := Find(src, "kill", "rm", "curl", "bash")
			if (callsErr == nil) != (findErr == nil) || (callsErr != nil && callsErr.Error() != findErr.Error()) {
				t.Fatalf("Calls error %v differs from Find error %v", callsErr, findErr)
			}
			want := make([]Call, 0, len(all))
			for _, call := range all {
				if call.Name == "kill" || call.Name == "rm" || call.Name == "curl" || call.Name == "bash" {
					want = append(want, call)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Find = %#v; filtered Calls = %#v", got, want)
			}
			for _, call := range all {
				switch {
				case strings.Contains(src, "select x") && call.Name == "kill" && !call.Loop:
					t.Fatalf("select body not marked as loop: %#v", call)
				case strings.Contains(src, "b'a's'h'") && strings.Contains(src, "| b'a's'h'") && call.Name == "bash" && !call.ScriptInput:
					t.Fatalf("quoted pipeline shell lacks ScriptInput: %#v", call)
				case strings.Contains(src, `b\a\s\h`) && call.Name == "bash" && !call.ScriptInput:
					t.Fatalf("escaped pipeline shell lacks ScriptInput: %#v", call)
				case strings.Contains(src, `b'a's'h' >(echo`) && call.Name == "bash" && !call.ScriptInput:
					t.Fatalf("process-substitution shell lacks ScriptInput: %#v", call)
				case strings.Contains(src, "B=bash") && call.Raw == "$B" && !call.ScriptInput:
					t.Fatalf("inherited-variable command should be script input: %#v", call)
				case strings.Contains(src, "kill -1 &") && call.Name == "kill" && !call.Background:
					t.Fatalf("background command lacks Background: %#v", call)
				}
			}
		})
	}
}

func TestFindKeepsNestedErrorWithoutRequestedNames(t *testing.T) {
	_, err := Find("echo exec -- bash -c 'if'")
	var nested *NestedError
	if !errors.As(err, &nested) {
		t.Fatalf("Find error = %v, want NestedError", err)
	}
}

func TestFindKeepsNonliteralCommandName(t *testing.T) {
	src := "CMD=kill; $CMD " + "-" + "1"
	calls, err := Calls(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Find(src, "kill")
	if err != nil || !reflect.DeepEqual(got, calls) {
		t.Fatalf("Find = %#v, Calls = %#v, err=%v", got, calls, err)
	}
}

func TestFindReturnsOnlyRequestedTimeCalls(t *testing.T) {
	got, err := Find("time kill -1", "kill")
	if err != nil || len(got) != 1 || got[0].Name != "kill" {
		t.Fatalf("Find = %#v, %v", got, err)
	}
}

func TestUntilMarksLoop(t *testing.T) {
	calls, err := Calls("until test -e x; do kill -1; done")
	if err != nil || len(calls) != 2 || !calls[1].Loop {
		t.Fatalf("Calls = %#v, %v", calls, err)
	}
}

func TestWalkInfoForRedirection(t *testing.T) {
	calls, err := Calls("echo x > f")
	if err != nil || len(calls) != 1 || len(calls[0].Redirs) != 1 {
		t.Fatalf("Calls = %#v, %v", calls, err)
	}
}

func TestWalkInfoForBackground(t *testing.T) {
	calls, err := Calls("sleep 1 &")
	if err != nil || len(calls) != 1 || !calls[0].Background {
		t.Fatalf("Calls = %#v, %v", calls, err)
	}
}

func TestSelectMarksLoop(t *testing.T) {
	calls, err := Calls("select x in a; do kill -1; done")
	if err != nil || len(calls) != 1 || !calls[0].Loop {
		t.Fatalf("Calls = %#v, %v", calls, err)
	}
}

func TestInheritedVariableShellProcessSubstitution(t *testing.T) {
	calls, err := Calls(`B=bash; bash -c '$B < <(echo x)'`)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, call := range calls {
		if call.Name == "bash" && call.Raw == "$B" {
			found = true
			if !call.ScriptInput {
				t.Fatalf("inherited variable should classify the command as shell input: %#v", call)
			}
		}
	}
	if !found {
		t.Fatalf("inherited command call not found: %#v", calls)
	}
}

func BenchmarkFindNestedSubstitution(b *testing.B) {
	for _, lines := range []int{1000, 5000, 20000} {
		b.Run(strconv.Itoa(lines), func(b *testing.B) {
			src := nestedSubstitutionScript(lines)
			b.SetBytes(int64(len(src)))
			b.ResetTimer()
			for range b.N {
				calls, err := Find(src, "kill")
				if err != nil || len(calls) != 20 {
					b.Fatalf("Find returned %d calls, err=%v", len(calls), err)
				}
			}
		})
	}
}

func TestCalls(t *testing.T) {
	tests := []struct {
		name, command string
		want          []string
	}{
		{"list and pipeline", `! echo x; sudo rg x | tail`, []string{"echo", "sudo", "rg", "tail"}},
		{"loop and function", "for x in a; do grep x f; done\nf(){ sed -n 1 f; }\nf", []string{"grep", "sed", "f"}},
		{"command substitutions", `echo "$(which rg)"; cat <(printf x)`, []string{"echo", "which", "cat", "printf"}},
		{"assignment variable", `R=~/bin/mytool; $R history`, []string{"mytool"}},
		{"xargs flag without value", `find . | xargs -r grep -l 'a b'`, []string{"find", "xargs", "grep"}},
		{"time format option", `/usr/bin/time -f "took %es" rg x`, []string{"time", "rg"}},
		{"nice and ionice values", `nice -n 5 ionice -c 3 rg x`, []string{"nice", "rg"}},
		{"wrapper options", `timeout 5 env -u X sudo -n rg x`, []string{"timeout", "rg"}},
		{"env lone dash", `env -`, []string{"env", "-"}},
		{"xargs option value", `xargs -I{} jq .`, []string{"xargs", "jq"}},
		{"bash command string", `bash -euo pipefail -c 'rg x; echo ok'`, []string{"bash", "rg", "echo"}},
		{"bash command string with variables", `bash -c 'while grep -q "$x"; do sleep 5; done'`, []string{"bash", "grep", "sleep"}},
		{"bash double-quoted command string", `timeout 15 bash -c "while kill -0 $cp 2>/dev/null; do sleep 1; done"`, []string{"timeout", "bash", "kill", "sleep"}},
		{"ssh remote command", `ssh -o StrictHostKeyChecking=no -p 22 host 'timeout 35m bash -c "while true; do sleep 20; done"'`, []string{"ssh", "timeout", "bash", "true", "sleep"}},
		{"ssh heredoc without remote command", "ssh host <<'EOF'\necho start\nkill 1\nEOF", []string{"ssh", "echo", "kill"}},
		{"ssh heredoc with bash command", "ssh -T host bash <<'EOF'\necho start\nkill 1\nEOF", []string{"ssh", "bash", "echo", "kill"}},
		{"ssh heredoc remote bash -s in one argument", "ssh host 'bash -s' <<'EOF'\necho start\nkill 1\nEOF", []string{"ssh", "bash", "echo", "kill"}},
		{"ssh heredoc remote sh -s in one argument", "ssh host 'sh -s' <<'EOF'\necho start\nkill 1\nEOF", []string{"ssh", "sh", "echo", "kill"}},
		{"ssh heredoc remote quoted bash", "ssh host \"bash\" <<'EOF'\necho start\nkill 1\nEOF", []string{"ssh", "bash", "echo", "kill"}},
		{"ssh heredoc remote sudo bash", "ssh host 'sudo bash' <<'EOF'\necho start\nkill 1\nEOF", []string{"ssh", "sudo", "bash", "echo", "kill"}},
		{"ssh heredoc remote bash command string is data", "ssh host 'bash -c \"cat\"' <<'EOF'\necho start\nkill 1\nEOF", []string{"ssh", "bash", "cat"}},
		{"ssh heredoc remote cat command is data", "ssh host 'cat > f' <<'EOF'\necho start\nkill 1\nEOF", []string{"ssh", "cat"}},
		{"ssh heredoc remote sh -c command is data", "ssh host 'sh -c cat' <<'EOF'\necho start\nkill 1\nEOF", []string{"ssh", "sh", "cat"}},
		{"ssh heredoc malformed remote prefix is data", "ssh host '; sh script.sh' <<'EOF'\necho start\nkill 1\nEOF", []string{"ssh"}},
		{"ssh heredoc with unquoted delimiter", "ssh -o BatchMode=yes host <<EOF\necho start\nkill 1\nEOF", []string{"ssh", "echo", "kill"}},
		{"ssh heredoc to data command", "ssh host cat <<'EOF'\necho start\nkill 1\nEOF", []string{"ssh", "cat"}},
		{"bash ANSI-C multiline command", "bash -c $'echo start\\nkill 1'", []string{"bash", "echo", "kill"}},
		{"ssh remote command with parameter", `ssh host 'timeout 35m bash -c "while kill -0 $child 2>/dev/null; do sleep 20; done"'`, []string{"ssh", "timeout", "bash", "kill", "sleep"}},
		{"ssh without command", `ssh -p 22 host`, []string{"ssh"}},
		{"ssh options after host", `ssh h -- 'pgrep x'; ssh h -p 22 -o A=b pgrep y; ssh -vp 22 h wc z`, []string{"ssh", "pgrep", "ssh", "pgrep", "ssh", "wc"}},
		{"ssh options after host without command", `ssh h -p 22`, []string{"ssh"}},
		{"ssh quoted remote script", `ssh host 'echo $(sleep 5)'`, []string{"ssh", "echo", "sleep"}},
		{"ssh remote loop with command substitution", `ssh host 'timeout 35m bash -c "while test $(printf x) = x; do sleep 20; done"'`, []string{"ssh", "timeout", "bash", "test", "printf", "sleep", "printf"}},
		{"bash -c in double quotes with any substitution", `bash -c "while test \"${x:-y}\"; do sleep 5; done"`, []string{"bash", "test", "sleep"}},
		{"only a variable as bash -c script", `bash -c "$cmd"`, []string{"bash"}},
		{"quoted variable argv0", "R=~/bin/mytool; \"$R\" history; \"${R}\" history", []string{"mytool", "mytool"}},
		{"quoted argv0 path from a variable", `"$HOME/bin/mytool" history x; "${HOME}/bin/mytool" x; "$HOME"/bin/mytool x; '/opt/my tool'"/$v/mytool" x`, []string{"mytool", "mytool", "mytool", "mytool"}},
		{"shell -c under an unknown program", `frame.sh exec /tmp/t bash -c 'grep -rl x d'`, []string{"frame.sh", "bash", "grep"}},
		{"echo of bash -c is data", `echo bash -c 'rg x'`, []string{"echo"}},
		{"substitution in a redirect target", `tr a b < /proc/$(pgrep -f x | head -1)/cmdline`, []string{"tr", "pgrep", "head"}},
		{"heredoc body substitution executes", "cat <<EOF\n$(rg x)\nEOF", []string{"cat", "rg"}},
		{"heredoc substitutions execute", "cat <<EOF\n$(kill 1) `which rg`\nEOF", []string{"cat", "kill", "which"}},
		{"shell heredoc after script file is data", "bash script.sh <<'EOF'\nSELECT * FROM t;\nEOF", []string{"bash"}},
		{"shell heredoc after command string is data", "bash -c 'cat' <<EOF\n$(kill 1)\nEOF", []string{"bash", "cat", "kill"}},
		{"shell heredoc after options and script file is data", "timeout 5 bash -euo pipefail -- script.sh <<'EOF'\nSELECT * FROM t;\nEOF", []string{"timeout", "bash"}},
		{"shell heredoc after plus option value", "bash +o pipefail <<'EOF'\nrg x\nEOF", []string{"bash", "rg"}},
		{"shell heredoc after long option", "bash --norc <<'EOF'\nrg x\nEOF", []string{"bash", "rg"}},
		{"shell stdin option heredoc is script", "bash -s -- arg <<'EOF'\nrg x\nEOF", []string{"bash", "rg"}},
		{"command lookup does not run target", "command -v wfor; command -V rg; command -p -v sed; command -pv jq", []string{"command", "command", "command", "command"}},
		{"-c after a script file is its argument", "bash script.sh -c 'kill 1'; bash -x script.sh -c 'kill 1'", []string{"bash", "bash"}},
		{"bash -c with end of options", "bash -c -- 'rg x'; sh -e -c -- 'wc -l'", []string{"bash", "rg", "sh", "wc"}},
		{"value option in a cluster", "sudo -iu root rg x; sudo -uroot wc y; timeout -sKILL 5 jq .", []string{"sudo", "rg", "sudo", "wc", "timeout", "jq"}},
		{"program options after command target", "cd /x; command grep -v a f; command -p grep -V x | command -- rg -v y", []string{"cd", "command", "grep", "command", "grep", "command", "rg"}},
		{"command wrapper runs target", "command -p rg x", []string{"command", "rg"}},
		{"escapes in words", `sh -c "rg \"a b\" \$HOME"; echo a\ b`, []string{"sh", "rg", "echo"}},
		{"unclosed heredoc runs to the end like bash", "bash <<'EOF'\nrg x", []string{"bash", "rg"}},
		{"invalid nested script", `bash -c 'echo "unterminated'; echo after`, []string{"bash", "echo"}},
		{"invalid nested script syntax", `bash -c 'rg x; if then'`, nil},
		{"bash long option is not command string", `bash --norc -c 'rg x'`, []string{"bash", "rg"}},
		{"timeout shell heredoc", "timeout 5 bash <<'EOF'\nrg x\nEOF", []string{"timeout", "bash", "rg"}},
		{"zsh shell heredoc", "zsh <<'EOF'\nrg x\nEOF", []string{"zsh", "rg"}},
		{"sudo shell heredoc", "sudo bash <<'EOF'\nrg x\nEOF", []string{"sudo", "bash", "rg"}},
		{"env shell heredoc", "env A=1 bash <<'EOF'\nrg x\nEOF", []string{"env", "bash", "rg"}},
		{"graft shell heredoc", "graft exec -- bash <<'EOF'\nrg x\nEOF", []string{"graft", "bash", "rg"}},
		{"text is not code", `echo "rg x"; cat <<'EOF'
rg x
EOF`, []string{"echo", "cat"}},
		{"shell heredoc", "bash <<'EOF'\nrg x\nEOF", []string{"bash", "rg"}},
		{"negated command", `! rg x`, []string{"rg"}},
		{"if condition and body", `if rg x; then sed y; fi`, []string{"rg", "sed"}},
		{"if branches", `if rg x; then sed y; elif grep z; then awk q; else cut f; fi`, []string{"rg", "sed", "grep", "awk", "cut"}},
		{"while body", `while grep x; do awk y; done`, []string{"grep", "awk"}},
		{"until body", `until test -e x; do touch x; done`, []string{"test", "touch"}},
		{"for body", `for f in a b; do cat "$f"; done`, []string{"cat"}},
		{"case arms", `case "$x" in a) rg x;; b) sed y;; esac`, []string{"rg", "sed"}},
		{"subshell", `(rg x; grep y)`, []string{"rg", "grep"}},
		{"brace group", `{ rg x; sed y; }`, []string{"rg", "sed"}},
		{"backtick substitution", "echo `which rg`", []string{"echo", "which"}},
		{"process substitutions", `diff <(sort a) <(sort b)`, []string{"diff", "sort", "sort"}},
		{"conditional expression", `[[ -f x && -n $x ]]; echo done`, []string{"echo"}},
		{"arithmetic expression", `(( x++ )); printf ok`, []string{"printf"}},
		{"function body", `f() { rg x; }`, []string{"rg"}},
		{"continued pipeline", "printf x \\\n| sed y", []string{"printf", "sed"}},
		{"quoted executable path", `"/usr/bin/rg" x`, []string{"rg"}},
		{"concatenated executable word", `r''g x`, []string{"rg"}},
		{"command assignment", `A=1 rg x`, []string{"rg"}},
		{"timeout signal option", `timeout --signal=TERM 5 rg x`, []string{"timeout", "rg"}},
		{"env assignments", `env -i A=1 B=2 rg x`, []string{"env", "rg"}},
		{"nice priority option", `nice -n 3 rg x`, []string{"nice", "rg"}},
		{"xargs zero delimiter", `xargs -0 rg`, []string{"xargs", "rg"}},
		{"xargs parallel options", `xargs -P4 -n8 rg`, []string{"xargs", "rg"}},
		{"nohup wrapper", `nohup rg x`, []string{"nohup", "rg"}},
		{"time wrapper", `time rg x`, []string{"time", "rg"}},
		{"command wrapper", `command rg x`, []string{"command", "rg"}},
		{"builtin wrapper", `builtin printf x`, []string{"builtin", "printf"}},
		{"exec wrapper", `exec rg x`, []string{"exec", "rg"}},

		{"nonliteral name", `$(which rg) x`, []string{"", "which"}},
		{"-- command wrapper", `graft exec --heavy -- go test`, []string{"graft", "go"}},
		{"nested command wrapper", `graft exec --heavy -- sudo sed -i x f`, []string{"graft", "sudo", "sed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Calls(tt.command)
			if tt.want == nil {
				if err == nil {
					t.Fatal("Calls() не вернул ошибку разбора")
				}
				var nestedErr *NestedError
				if !errors.As(err, &nestedErr) {
					t.Fatalf("ошибка вложенного разбора не обёрнута: %v", err)
				}
				return
			}
			if err != nil {
				var nestedErr *NestedError
				if !errors.As(err, &nestedErr) {
					t.Fatal(err)
				}
			}
			var names []string
			for _, call := range got {
				names = append(names, call.Name)
			}
			if !reflect.DeepEqual(names, tt.want) {
				t.Fatalf("names = %#v, want %#v; calls: %#v", names, tt.want, got)
			}
		})
	}
}

func TestNestedErrorsKeepCalls(t *testing.T) {
	calls, err := Calls(`echo a; sh -c 'echo "x'; rg y; bash -c 'if then'; sed z`)
	var nested *NestedError
	if !errors.As(err, &nested) || nested.Script == "" {
		t.Fatalf("NestedError = %#v, %v", nested, err)
	}
	var names []string
	for _, call := range calls {
		names = append(names, call.Name)
	}
	if !reflect.DeepEqual(names, []string{"echo", "sh", "rg", "bash", "sed"}) {
		t.Fatalf("вызовы при ошибках вложенных скриптов = %#v", names)
	}
	found, findErr := Find(`echo a; sh -c 'echo "x'; rg y`, "rg")
	if !errors.As(findErr, &nested) || len(found) != 1 || found[0].Name != "rg" {
		t.Fatalf("Find при ошибке вложенного скрипта = %#v, %v", found, findErr)
	}
}

func TestBackgroundCalls(t *testing.T) {
	calls, err := Calls(`(bash -c 'sleep 5') & { rg x; } && sleep 1 & echo done`)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if call.Name == "echo" {
			if call.Background {
				t.Errorf("передний вызов помечен фоновым: %#v", call)
			}
		} else if !call.Background {
			t.Errorf("вызов не унаследовал фон: %#v", call)
		}
	}
}

func TestHeredocSubstitutionOffset(t *testing.T) {
	command := "cat <<EOF\n$(kill 1)\nEOF"
	calls, err := Find(command, "kill")
	if err != nil || len(calls) != 1 {
		t.Fatalf("вызов из heredoc = %#v, %v", calls, err)
	}
	if want := strings.Index(command, "kill"); calls[0].Offset != want {
		t.Errorf("Offset = %d, want %d", calls[0].Offset, want)
	}
	parent, err := Find(command, "cat")
	if err != nil || len(parent) != 1 || len(parent[0].Redirs) != 1 || parent[0].Redirs[0].Body != "$(kill 1)\n" {
		t.Errorf("данные heredoc изменились: %#v, %v", parent, err)
	}

	script := "bash <<EOF\n$(kill 1)\necho ok\nEOF"
	calls, err = Find(script, "kill")
	if err != nil || len(calls) != 1 || calls[0].Offset != strings.Index(script, "kill") {
		t.Errorf("вызов подстановки в скрипте heredoc = %#v, %v", calls, err)
	}
}

func TestLocalShellStdinUsesEffectiveDescriptor(t *testing.T) {
	for _, src := range []string{
		"bash 3<<EOF\nkill 1\nEOF",
		"bash <<A </dev/null\nkill 1\nA",
	} {
		calls, err := Find(src, "kill")
		if err != nil || len(calls) != 0 {
			t.Errorf("Find(%q) = %#v, %v; want fd 0 not to receive script", src, calls, err)
		}
	}
}

func TestPipedHeredocScripts(t *testing.T) {
	tests := []struct {
		name, command string
		via           []string
		line          int
		noScript      bool
	}{
		{name: "cat to bash", command: "cat <<'END' |\nkill 0\nEND\nbash", via: []string{"pipe", "bash"}, line: 2},
		{name: "expanded body", command: "cat <<E | bash\nkill 1\nE", via: []string{"pipe", "bash"}, line: 2},
		{name: "body substitutions", command: "cat <<E | bash\n$(kill 1)\nE", via: []string{"pipe", "bash"}, line: 2},
		{name: "tabs to sh stdin", command: "cat - <<-E | sh -s\n\tkill 1\n\tE", via: []string{"pipe", "sh"}, line: 2},
		{name: "sudo shell", command: "cat - <<E | sudo bash\nkill 1\nE", via: []string{"pipe", "sudo", "bash"}, line: 2},
		{name: "runuser shell", command: "cat <<'E' | runuser sh\nkill 1\nE", via: []string{"pipe", "runuser", "sh"}, line: 2},
		{name: "sudo shell from stdin", command: "cat <<'E' | sudo -s\nkill 1\nE", via: []string{"pipe", "sudo", "bash"}, line: 2},
		{name: "tee to bash", command: "tee /tmp/x <<E | bash\nkill 1\nE", via: []string{"pipe", "bash"}, line: 2},
		{name: "timeout shell", command: "cat <<E | timeout 5 bash\nkill 1\nE", via: []string{"pipe", "timeout", "bash"}, line: 2},
		{name: "nested piped heredoc", command: "cat <<'A' | bash\ncat <<'B' | bash\nkill 1\nB\nA", via: []string{"pipe", "bash", "pipe", "bash"}, line: 3},
		{name: "non-shell pipe", command: "cat <<E | grep x\nkill 0\nE", noScript: true},
		{name: "no-exec shell", command: "cat <<E | bash -n\nkill 0\nE", noScript: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls, err := Calls(tt.command)
			if err != nil {
				t.Fatalf("Calls: %v", err)
			}
			var found bool
			for _, call := range calls {
				if call.Name == "kill" {
					found = true
					if tt.noScript || call.Line != tt.line || !reflect.DeepEqual(call.Via, tt.via) {
						t.Errorf("kill call = %#v, want line %d via %q", call, tt.line, tt.via)
					}
				}
				if (call.Name == "bash" || call.Name == "sh") && call.ScriptInput {
					t.Errorf("known piped script marked ScriptInput: %#v", call)
				}
			}
			if found != !tt.noScript {
				t.Errorf("kill found = %t, want %t; calls = %#v", found, !tt.noScript, calls)
			}
		})
	}
}

func TestPipedHeredocDepthLimit(t *testing.T) {
	script := "kill 1"
	for range maxNestedScripts {
		script = "cat <<'E' | bash\n" + script + "\nE"
	}
	calls, err := Calls(script)
	var nested *NestedError
	if !errors.As(err, &nested) {
		t.Fatalf("deep piped heredocs returned no depth error: calls=%d err=%v", len(calls), err)
	}
}

func TestLoopCalls(t *testing.T) {
	calls, err := Calls("while true; do sleep 5; done; bash -c 'for x in a; do sleep 2; done'")
	if err != nil {
		t.Fatal(err)
	}
	var loops []string
	for _, call := range calls {
		if call.Name == "sleep" && call.Loop {
			loops = append(loops, call.Args[0])
		}
	}
	if !reflect.DeepEqual(loops, []string{"5", "2"}) {
		t.Fatalf("sleep в циклах = %#v", loops)
	}
	calls, err = Calls("sleep 1; echo done")
	if err != nil || len(calls) != 2 || calls[0].Loop {
		t.Fatalf("вызовы вне цикла = %#v, %v", calls, err)
	}
	for _, command := range []string{
		`for i in 1 2; do bash -c 'sleep 30'; done`,
		"for i in 1 2; do bash <<'END'\nsleep 30\nEND\ndone",
		`ssh host 'timeout 35m bash -c "while true; do sleep 20; done"'`,
	} {
		calls, err = Calls(command)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, call := range calls {
			if call.Name == "sleep" && call.Loop {
				found = true
			}
		}
		if !found {
			t.Errorf("вложенный вызов в цикле не помечен: %#v", calls)
		}
		for _, call := range calls {
			if call.Name != "sleep" {
				continue
			}
			if want := strings.Index(command, "sleep "+call.Args[0]); call.Offset != want {
				t.Errorf("смещение вложенного вызова = %d, want %d", call.Offset, want)
			}
		}
	}
}

func TestCallDetails(t *testing.T) {
	got, err := Calls("echo x\nsudo /usr/bin/rg 'a b' $q")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d calls: %#v", len(got), got)
	}
	if got[2].Name != "rg" || !reflect.DeepEqual(got[2].Args, []string{"a b", "$q"}) || got[2].Line != 2 || !reflect.DeepEqual(got[2].Via, []string{"sudo"}) {
		t.Fatalf("call details = %#v", got[2])
	}
	esc, err := Find(`rg "a \"b\" \$x" c\ d`, "rg")
	if err != nil || len(esc) != 1 || !reflect.DeepEqual(esc[0].Args, []string{`a "b" $x`, "c d"}) {
		t.Fatalf("escaped args = %#v, %v", esc, err)
	}
	found, err := Find("echo x; rg y", "rg")
	if err != nil || len(found) != 1 || found[0].Name != "rg" {
		t.Fatalf("Find = %#v, %v", found, err)
	}
}

func TestKillGuardForms(t *testing.T) {
	tests := []struct {
		command, name string
		args, via     []string
	}{
		{`taskset -c 0 kill 1`, "kill", []string{"1"}, []string{"taskset"}},
		{`chrt -o 0 kill 1`, "kill", []string{"1"}, []string{"chrt"}},
		{`strace -o /dev/null kill 1`, "kill", []string{"1"}, []string{"strace"}},
		{`setpriv --reuid=u1 kill 1`, "kill", []string{"1"}, []string{"setpriv"}},
		{`unshare -U kill 1`, "kill", []string{"1"}, []string{"unshare"}},
		{`flock /tmp/x kill 1`, "kill", []string{"1"}, []string{"flock"}},
		{`busybox kill 1`, "kill", []string{"1"}, []string{"busybox"}},
		{`systemd-run --user --scope pkill -u u1`, "pkill", []string{"-u", "u1"}, []string{"systemd-run"}},
		{`parallel kill ::: 1`, "kill", []string{"1"}, []string{"parallel"}},
		{`runuser -u u1 -- kill 1`, "kill", []string{"1"}, []string{"runuser"}},
		{`doas -u u1 kill 1`, "kill", []string{"1"}, []string{"doas"}},
		{`eval "kill $P"`, "kill", []string{"$P"}, []string{"eval"}},
		{`watch -n1 'kill 1'`, "kill", []string{"1"}, []string{"watch"}},
		{`trap 'kill 1' EXIT`, "kill", []string{"1"}, []string{"trap"}},
		{`su u1 -c 'kill 1'`, "kill", []string{"1"}, []string{"su"}},
		{`runuser -u u1 -c 'kill 1'`, "kill", []string{"1"}, []string{"runuser"}},
		{`flock /tmp/x -c 'kill 1'`, "kill", []string{"1"}, []string{"flock"}},
		{`script -qc 'pkill -u u1' /dev/null`, "pkill", []string{"-u", "u1"}, []string{"script"}},
		{`tmux new-session -d 'pkill -u u1'`, "pkill", []string{"-u", "u1"}, []string{"tmux"}},
		{`env -S 'kill 1'`, "kill", []string{"1"}, []string{"env"}},
		{`env -iS 'kill 1'`, "kill", []string{"1"}, []string{"env"}},
		{`env --split-string='kill 1'`, "kill", []string{"1"}, []string{"env"}},
		{`find . -exec kill {} +`, "kill", []string{"{}"}, []string{"find", "-exec"}},
		{`find . -execdir pkill -u u1 \;`, "pkill", []string{"-u", "u1"}, []string{"find", "-execdir"}},
		{`find . -ok kill {} \;`, "kill", []string{"{}"}, []string{"find", "-ok"}},
		{`find . -okdir pkill -u u1 +`, "pkill", []string{"-u", "u1"}, []string{"find", "-okdir"}},
		{`bash <<< 'kill 1'`, "kill", []string{"1"}, []string{"bash"}},
		{`bash -s -- -n <<'E'
kill 1
E`, "kill", []string{"1"}, []string{"bash"}},
		{`echo k | bash -s -- -n`, "kill", nil, nil},
		{`parallel bash -c 'kill 1' ::: x`, "kill", []string{"1"}, []string{"parallel", "bash", "-c"}},
		{`parallel -j1 kill ::: 1`, "kill", []string{"1"}, []string{"parallel"}},
		{`parallel kill -9 ::: 1`, "kill", []string{"-9", "1"}, []string{"parallel"}},
		{`parallel -j 1 kill -9 ::: 1`, "kill", []string{"-9", "1"}, []string{"parallel"}},
		{`parallel kill ::: 1 2`, "kill", []string{"1", "2"}, []string{"parallel"}},
		{`sudo su -c 'pkill -u u1'`, "pkill", []string{"-u", "u1"}, []string{"sudo", "su"}},
		{`nice -n 5 env -S 'kill 1'`, "kill", []string{"1"}, []string{"nice", "env"}},
		{`timeout 5 flock /tmp/x -c 'kill 1'`, "kill", []string{"1"}, []string{"timeout", "flock"}},
		{`sudo runuser -u u1 -c 'kill 1'`, "kill", []string{"1"}, []string{"sudo", "runuser"}},
		{`sudo su - u1 -c 'kill 1'`, "kill", []string{"1"}, []string{"sudo", "su"}},
		{`flock /tmp/x bash -c 'kill 1'`, "kill", []string{"1"}, []string{"flock", "bash", "-c"}},
		{`flock /tmp/x pkill -c -u u1`, "pkill", []string{"-c", "-u", "u1"}, []string{"flock"}},
		{`runuser -u u1 -- pkill -c -u u1`, "pkill", []string{"-c", "-u", "u1"}, []string{"runuser"}},
		{`flock /tmp/x pkill -u u1 -c`, "pkill", []string{"-u", "u1", "-c"}, []string{"flock"}},
		{`sudo flock /tmp/x pkill -c -u u1`, "pkill", []string{"-c", "-u", "u1"}, []string{"sudo", "flock"}},
		{`su -c 'kill 1' u1`, "kill", []string{"1"}, []string{"su"}},
		{`su --command='kill 1' u1`, "kill", []string{"1"}, []string{"su"}},
		{`su -lc 'kill 1' u1`, "kill", []string{"1"}, []string{"su"}},
		{`su -c'kill 1' u1`, "kill", []string{"1"}, []string{"su"}},
		{`runuser -c'kill 1' u1`, "kill", []string{"1"}, []string{"runuser"}},
		{`runuser --command='kill 1' u1`, "kill", []string{"1"}, []string{"runuser"}},
		{`flock /tmp/x -c'kill 1'`, "kill", []string{"1"}, []string{"flock"}},
		{`flock /tmp/x --command='kill 1'`, "kill", []string{"1"}, []string{"flock"}},
		{`taskset 0x1 kill 1`, "kill", []string{"1"}, []string{"taskset"}},
		{`taskset 1 pkill -u u1`, "pkill", []string{"-u", "u1"}, []string{"taskset"}},
		{`chrt 5 kill 1`, "kill", []string{"1"}, []string{"chrt"}},
		{`chrt -b 0 kill 1`, "kill", []string{"1"}, []string{"chrt"}},
		{`chrt --other 0 kill 1`, "kill", []string{"1"}, []string{"chrt"}},
		{`watch -t 'kill 1'`, "kill", []string{"1"}, []string{"watch"}},
		{`watch -d 'kill 1'`, "kill", []string{"1"}, []string{"watch"}},
		{`watch -g 'kill 1'`, "kill", []string{"1"}, []string{"watch"}},
		{`watch -x kill 1`, "kill", []string{"1"}, []string{"watch"}},
		{`watch -n 1 'kill 1'`, "kill", []string{"1"}, []string{"watch"}},
		{`strace -T -D kill 1`, "kill", []string{"1"}, []string{"strace"}},
		{`strace -E X=1 kill 1`, "kill", []string{"1"}, []string{"strace"}},
		{`flock -o /tmp/x kill 1`, "kill", []string{"1"}, []string{"flock"}},
		{`systemd-run --same-dir kill 1`, "kill", []string{"1"}, []string{"systemd-run"}},
		{`systemd-run -E X=1 kill 1`, "kill", []string{"1"}, []string{"systemd-run"}},
		{`systemd-run -u x kill 1`, "kill", []string{"1"}, []string{"systemd-run"}},
		{`unshare -S 0 kill 1`, "kill", []string{"1"}, []string{"unshare"}},
		{`script --echo always -c 'kill 1'`, "kill", []string{"1"}, []string{"script"}},
		{`tmux new -d 'kill 1'`, "kill", []string{"1"}, []string{"tmux"}},
		{`trap -- 'kill 1' EXIT`, "kill", []string{"1"}, []string{"trap"}},
		{`eval -- 'kill 1'`, "kill", []string{"1"}, []string{"eval"}},
		{`env -i -S 'kill 1'`, "kill", []string{"1"}, []string{"env"}},
		{`env -u X -S 'kill 1'`, "kill", []string{"1"}, []string{"env"}},
		{`env --ignore-environment --split-string='kill 1'`, "kill", []string{"1"}, []string{"env"}},
		{`env -S'kill 1'`, "kill", []string{"1"}, []string{"env"}},
		{`eval kill 1`, "kill", []string{"1"}, []string{"eval"}},
		{`find . -exec kill {} \; -exec pkill -u u1 +`, "kill", []string{"{}"}, []string{"find", "-exec"}},
		{`find . -exec kill {} \; -exec pkill -u u1 +`, "pkill", []string{"-u", "u1"}, []string{"find", "-exec"}},
		{`trap - SIG`, "kill", nil, nil},
		{`parallel kill :::: /tmp/x`, "kill", []string{"/tmp/x"}, []string{"parallel"}},
		{`env -S kill -1`, "kill", []string{"-1"}, []string{"env"}},
		{`env -S pkill -u u1`, "pkill", []string{"-u", "u1"}, []string{"env"}},
		{`env -S nice kill 1`, "kill", []string{"1"}, []string{"env", "nice"}},
		{`nice env -S sudo pkill -u u1`, "pkill", []string{"-u", "u1"}, []string{"nice", "env", "sudo"}},
		{`env --split-string=nice kill 1`, "kill", []string{"1"}, []string{"env", "nice"}},
		{`env - kill 1`, "kill", []string{"1"}, []string{"env"}},
		{`sudo env - pkill -u u1`, "pkill", []string{"-u", "u1"}, []string{"sudo", "env"}},
		{`watch kill -1`, "kill", []string{"-1"}, []string{"watch"}},
		{`watch sudo pkill -u u1`, "pkill", []string{"-u", "u1"}, []string{"watch", "sudo"}},
		{`nice parallel bash -c 'kill 1' ::: x`, "kill", []string{"1"}, []string{"nice", "parallel", "bash", "-c"}},
		{`sudo parallel kill -9 ::: 1`, "kill", []string{"-9", "1"}, []string{"sudo", "parallel"}},
		{`timeout 5 parallel kill ::: 1`, "kill", []string{"1"}, []string{"timeout", "parallel"}},
		{`parallel --halt now,fail=1 kill ::: 1`, "kill", []string{"1"}, []string{"parallel"}},
		{`parallel --timeout 5 kill ::: 1`, "kill", []string{"1"}, []string{"parallel"}},
		{`parallel --delay 1 kill ::: 1`, "kill", []string{"1"}, []string{"parallel"}},
		{`runuser - u1 -c 'kill 1'`, "kill", []string{"1"}, []string{"runuser"}},
		{`runuser u1 -c 'kill 1'`, "kill", []string{"1"}, []string{"runuser"}},
		{`runuser -l u1 -c 'kill 1'`, "kill", []string{"1"}, []string{"runuser"}},
		{`taskset -c0 kill 1`, "kill", []string{"1"}, []string{"taskset"}},
		{`taskset --cpu-list=0 kill 1`, "kill", []string{"1"}, []string{"taskset"}},
		{`taskset -ac 0 kill 1`, "kill", []string{"1"}, []string{"taskset"}},
		{`unshare --fork --pid --mount-proc kill 1`, "kill", []string{"1"}, []string{"unshare"}},
		{`tmux new -d -e X=1 'kill 1'`, "kill", []string{"1"}, []string{"tmux"}},
		{`tmux -L /tmp/socket new -d -e X=1 'kill 1'`, "kill", []string{"1"}, []string{"tmux"}},
		{`script -E always /dev/null -c 'kill 1'`, "kill", []string{"1"}, []string{"script"}},
		{`flock --timeout 5 /tmp/x -c 'kill 1'`, "kill", []string{"1"}, []string{"flock"}},
		{`flock --conflict-exit-code 3 /tmp/x kill 1`, "kill", []string{"1"}, []string{"flock"}},
		{`strace --output /dev/null kill 1`, "kill", []string{"1"}, []string{"strace"}},
		{`strace -U name kill 1`, "kill", []string{"1"}, []string{"strace"}},
		{`systemd-run --on-active 5 kill 1`, "kill", []string{"1"}, []string{"systemd-run"}},
		{`systemd-run --machine m kill 1`, "kill", []string{"1"}, []string{"systemd-run"}},
		{`setpriv --ruid 1 kill 1`, "kill", []string{"1"}, []string{"setpriv"}},
		{`chrt -T 1 -P 2 -D 3 -d 0 kill 1`, "kill", []string{"1"}, []string{"chrt"}},
		{`script -E never -c 'kill 1'`, "kill", []string{"1"}, []string{"script"}},
		{`su --session-command 'kill 1' u1`, "kill", []string{"1"}, []string{"su"}},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			calls, err := Calls(tt.command)
			if err != nil {
				t.Fatal(err)
			}
			var found []Call
			for _, call := range calls {
				if call.Name == tt.name {
					found = append(found, call)
				}
			}
			if tt.args == nil {
				if len(found) != 0 {
					t.Fatalf("%s calls = %#v; want no %s call", tt.command, found, tt.name)
				}
				return
			}
			if len(found) != 1 || !reflect.DeepEqual(found[0].Args, tt.args) || !reflect.DeepEqual(found[0].Via, tt.via) {
				t.Fatalf("%s calls = %#v (all %#v); want %s %q via %q", tt.name, found, calls, tt.name, tt.args, tt.via)
			}
		})
	}
}

func TestWrapperOptionAndProgressRegressions(t *testing.T) {
	tests := []struct {
		command string
		name    string
		args    []string
		via     []string
	}{
		{command: `runuser - u1 -c 'kill 1'`, name: "kill", args: []string{"1"}, via: []string{"runuser"}},
		{command: `runuser u1 -c 'kill 1'`, name: "kill", args: []string{"1"}, via: []string{"runuser"}},
		{command: `runuser -l u1 -c 'kill 1'`, name: "kill", args: []string{"1"}, via: []string{"runuser"}},
		{command: `runuser - u1 -c 'kill 1'`, name: "kill", args: []string{"1"}, via: []string{"runuser"}},
		{command: `taskset -c0 kill 1`, name: "kill", args: []string{"1"}, via: []string{"taskset"}},
		{command: `taskset --cpu-list=0 kill 1`, name: "kill", args: []string{"1"}, via: []string{"taskset"}},
		{command: `taskset -ac 0 kill 1`, name: "kill", args: []string{"1"}, via: []string{"taskset"}},
		{command: `unshare --fork --pid --mount-proc kill 1`, name: "kill", args: []string{"1"}, via: []string{"unshare"}},
		{command: `flock --timeout 5 /tmp/x -c 'kill 1'`, name: "kill", args: []string{"1"}, via: []string{"flock"}},
		{command: `flock --conflict-exit-code 3 /tmp/x kill 1`, name: "kill", args: []string{"1"}, via: []string{"flock"}},
		{command: `strace --output /tmp/x -U name --trace=process kill 1`, name: "kill", args: []string{"1"}, via: []string{"strace"}},
		{command: `systemd-run --on-active 5 --on-calendar '*-*-* 00:00' --machine m kill 1`, name: "kill", args: []string{"1"}, via: []string{"systemd-run"}},
		{command: `setpriv --ruid 1 --euid 2 --rgid 3 --egid 4 kill 1`, name: "kill", args: []string{"1"}, via: []string{"setpriv"}},
		{command: `chrt -T 1 -P 2 -D 3 -d 0 kill 1`, name: "kill", args: []string{"1"}, via: []string{"chrt"}},
		{command: `script -E never -O /tmp/out -B /tmp/io -T /tmp/time -I /tmp/in -m advanced -o 1 -c 'kill 1'`, name: "kill", args: []string{"1"}, via: []string{"script"}},
		{command: `su --session-command 'kill 1' u1`, name: "kill", args: []string{"1"}, via: []string{"su"}},
		{command: `tmux new -F '#{session_name}' -d -e X=1 'kill 1'`, name: "kill", args: []string{"1"}, via: []string{"tmux"}},
		{command: `env -S 'echo a' b`, name: "echo", args: []string{"a", "b"}, via: []string{"env"}},
		{command: `parallel kill ::: 1 ::: 2`, name: "kill", args: []string{"1", "2"}, via: []string{"parallel"}},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			calls, err := Calls(tt.command)
			if err != nil {
				t.Fatalf("Calls: %v", err)
			}
			found := 0
			for _, call := range calls {
				if call.Name == tt.name {
					found++
					if !reflect.DeepEqual(call.Args, tt.args) || !reflect.DeepEqual(call.Via, tt.via) {
						t.Errorf("call = %#v, want %s %q via %q", call, tt.name, tt.args, tt.via)
					}
				}
			}
			if found != 1 {
				t.Fatalf("found %d calls named %q: %#v", found, tt.name, calls)
			}
		})
	}
}

func TestBaselineFallbackAndWrapperOptions(t *testing.T) {
	tests := []struct {
		command string
		name    string
		args    []string
		via     []string
	}{
		{command: `env -uSSH_AUTH_SOCK kill -1`, name: "kill", args: []string{"-1"}, via: []string{"env"}},
		{command: `env -CSOMEDIR kill -1`, name: "kill", args: []string{"-1"}, via: []string{"env"}},
		{command: `env -a x kill -1`, name: "kill", args: []string{"-1"}, via: []string{"env"}},
		{command: `env --argv0 x kill -1`, name: "kill", args: []string{"-1"}, via: []string{"env"}},
		{command: `tmux new-window bash -c 'kill -1'`, name: "kill", args: []string{"-1"}, via: []string{"tmux", "bash", "-c"}},
		{command: `tmux neww bash -c 'kill -1'`, name: "kill", args: []string{"-1"}, via: []string{"tmux", "bash", "-c"}},
		{command: `tmux split-window -h bash -c 'kill -1'`, name: "kill", args: []string{"-1"}, via: []string{"tmux", "bash", "-c"}},
		{command: `parallel bash -c 'kill -1'`, name: "kill", args: []string{"-1"}, via: []string{"parallel", "bash", "-c"}},
		{command: `parallel bash -c 'kill -1' < list`, name: "kill", args: []string{"-1"}, via: []string{"parallel", "bash", "-c"}},
		{command: `parallel -a list bash -c 'kill -1'`, name: "kill", args: []string{"-1"}, via: []string{"parallel", "bash", "-c"}},
		{command: `runuser -u u1 kill -1`, name: "kill", args: []string{"-1"}, via: []string{"runuser"}},
		{command: `taskset -a -c 0 kill -1`, name: "kill", args: []string{"-1"}, via: []string{"taskset"}},
		{command: `taskset 1 pkill -c -u u1`, name: "pkill", args: []string{"-c", "-u", "u1"}, via: []string{"taskset"}},
		{command: `su -s/bin/csh -c 'kill -1' u1`, name: "kill", args: []string{"-1"}, via: []string{"su"}},
		{command: `tmux new -d kill -1`, name: "kill", args: []string{"-1"}, via: []string{"tmux"}},
		{command: `sudo -R /x kill -1`, name: "kill", args: []string{"-1"}, via: []string{"sudo"}},
		{command: `unshare -l binfmt_misc kill -1`, name: "kill", args: []string{"-1"}, via: []string{"unshare"}},
		{command: `unshare --map-users 0:1000:1 --map-groups 0:1000:1 --monotonic 5 --boottime 5 --setgroups deny kill -1`, name: "kill", args: []string{"-1"}, via: []string{"unshare"}},
		{command: `setpriv --ptracer any --landlock-access fs --landlock-rule path-beneath:read:/x --seccomp-filter /tmp/x kill -1`, name: "kill", args: []string{"-1"}, via: []string{"setpriv"}},
		{command: `watch --equexit 3 kill -1`, name: "kill", args: []string{"-1"}, via: []string{"watch"}},
		{command: `strace --argv0 x --secontext kill -1`, name: "kill", args: []string{"-1"}, via: []string{"strace"}},
		{command: `strace --user u1 kill -1`, name: "kill", args: []string{"-1"}, via: []string{"strace"}},
		{command: `strace --string-limit 100 kill -1`, name: "kill", args: []string{"-1"}, via: []string{"strace"}},
		{command: `strace --env A=1 kill -1`, name: "kill", args: []string{"-1"}, via: []string{"strace"}},
		{command: `strace --columns 40 kill -1`, name: "kill", args: []string{"-1"}, via: []string{"strace"}},
		{command: `strace --const-print-style raw kill -1`, name: "kill", args: []string{"-1"}, via: []string{"strace"}},
		{command: `systemd-run --root-directory /x kill -1`, name: "kill", args: []string{"-1"}, via: []string{"systemd-run"}},
		{command: `systemd-run --json pretty kill -1`, name: "kill", args: []string{"-1"}, via: []string{"systemd-run"}},
		{command: `systemd-run --job-mode replace kill -1`, name: "kill", args: []string{"-1"}, via: []string{"systemd-run"}},
		{command: `systemd-run --path-property PathExists=/x kill -1`, name: "kill", args: []string{"-1"}, via: []string{"systemd-run"}},
		{command: `systemd-run --socket-property ListenStream=1 kill -1`, name: "kill", args: []string{"-1"}, via: []string{"systemd-run"}},
		{command: `systemd-run --on-clock-change --on-timezone-change kill -1`, name: "kill", args: []string{"-1"}, via: []string{"systemd-run"}},
		{command: `parallel --colsep , kill ::: 1`, name: "kill", args: []string{"1"}, via: []string{"parallel"}},
		{command: `parallel -C , kill ::: 1`, name: "kill", args: []string{"1"}, via: []string{"parallel"}},
		{command: `parallel -I {} kill {} ::: 1`, name: "kill", args: []string{"{}", "1"}, via: []string{"parallel"}},
		{command: `parallel -E end kill ::: 1`, name: "kill", args: []string{"1"}, via: []string{"parallel"}},
		{command: `parallel --retries 3 kill ::: 1`, name: "kill", args: []string{"1"}, via: []string{"parallel"}},
		{command: `parallel --tmpdir /x kill ::: 1`, name: "kill", args: []string{"1"}, via: []string{"parallel"}},
		{command: `parallel --header : kill ::: 1`, name: "kill", args: []string{"1"}, via: []string{"parallel"}},
		{command: `parallel --termseq INT kill ::: 1`, name: "kill", args: []string{"1"}, via: []string{"parallel"}},
		{command: `parallel --sshdelay 1 kill ::: 1`, name: "kill", args: []string{"1"}, via: []string{"parallel"}},
		{command: `parallel --group-by 1 kill ::: 1`, name: "kill", args: []string{"1"}, via: []string{"parallel"}},
		{command: `parallel --basefile x kill ::: 1`, name: "kill", args: []string{"1"}, via: []string{"parallel"}},
		{command: `parallel --compress-program gzip kill ::: 1`, name: "kill", args: []string{"1"}, via: []string{"parallel"}},
		{command: `parallel --limit 1 kill ::: 1`, name: "kill", args: []string{"1"}, via: []string{"parallel"}},
		{command: `parallel --memsuspend 1G kill ::: 1`, name: "kill", args: []string{"1"}, via: []string{"parallel"}},
		{command: `systemd-run --on-clock-change --on-timezone-change kill -1`, name: "kill", args: []string{"-1"}, via: []string{"systemd-run"}},
		{command: `su --session-command='kill -1' u1`, name: "kill", args: []string{"-1"}, via: []string{"su"}},
		{command: `su -gcdrom -c 'kill -1' u1`, name: "kill", args: []string{"-1"}, via: []string{"su"}},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			calls, err := Calls(tt.command)
			if err != nil {
				t.Fatalf("Calls: %v", err)
			}
			found := 0
			for _, call := range calls {
				if call.Name == tt.name {
					found++
					if !reflect.DeepEqual(call.Args, tt.args) || !reflect.DeepEqual(call.Via, tt.via) {
						t.Errorf("call = %#v, want %s %q via %q", call, tt.name, tt.args, tt.via)
					}
				}
			}
			if found != 1 {
				t.Fatalf("found %d calls named %q: %#v", found, tt.name, calls)
			}
		})
	}
}

func TestShellInputWrapperBoundaries(t *testing.T) {
	for _, command := range []string{
		`echo x | sudo -u u1 -s`,
		`echo x | sudo -u u1 -i`,
	} {
		calls, err := Calls(command)
		if err != nil {
			t.Fatalf("%q: %v", command, err)
		}
		found := false
		for _, call := range calls {
			found = found || call.ScriptInput
		}
		if !found {
			t.Errorf("%q: expected ScriptInput, calls=%#v", command, calls)
		}
	}
	for _, command := range []string{
		`echo x | sudo grep -i x`,
		`echo x | sudo sort -s`,
		`echo x | sudo --preserve-env=PATH grep x`,
		`echo x | sudo -u postgres psql -d db -i`,
		`echo x | runuser -u u1 cat`,
		`echo x | su -c ls u1`,
	} {
		calls, err := Calls(command)
		if err != nil {
			t.Fatalf("%q: %v", command, err)
		}
		for _, call := range calls {
			if call.ScriptInput {
				t.Errorf("%q: unexpected ScriptInput: %#v", command, call)
			}
		}
		if strings.Contains(command, "runuser -u") {
			foundCat := false
			for _, call := range calls {
				foundCat = foundCat || call.Name == "cat"
			}
			if !foundCat {
				t.Errorf("%q: cat call was lost: %#v", command, calls)
			}
		}
	}
	for _, tt := range []struct {
		command string
		via     []string
	}{
		{"sudo -s <<E\nkill -1\nE", []string{"sudo", "bash"}},
		{"su u1 <<E\nkill -1\nE", []string{"su", "sh"}},
	} {
		calls, err := Calls(tt.command)
		if err != nil {
			t.Fatalf("%q: %v", tt.command, err)
		}
		found := false
		for _, call := range calls {
			if call.Name == "kill" {
				found = true
				if !reflect.DeepEqual(call.Via, tt.via) {
					t.Errorf("%q: kill Via = %q, want %q", tt.command, call.Via, tt.via)
				}
			}
		}
		if !found {
			t.Errorf("%q: no kill call: %#v", tt.command, calls)
		}
	}
}

func TestPipedCatOptions(t *testing.T) {
	for _, command := range []string{"cat -v <<E | bash\nkill -1\nE", "cat -vu <<E | bash\nkill -1\nE", "cat --show-nonprinting <<E | bash\nkill -1\nE"} {
		calls, err := Calls(command)
		if err != nil {
			t.Fatalf("%q: %v", command, err)
		}
		found := false
		for _, call := range calls {
			found = found || call.Name == "kill"
		}
		if !found {
			t.Errorf("%q: expected kill in unchanged cat output: %#v", command, calls)
		}
	}
	calls, err := Calls("cat -n <<E | bash\nkill -1\nE")
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if call.Name == "kill" {
			t.Errorf("numbered cat output parsed as original script: %#v", calls)
		}
	}
}

func TestParseNestedDepthLimitDirectly(t *testing.T) {
	calls := []Call(nil)
	via := make([]string, maxNestedScripts+1)
	err := parseNested("", nil, 0, via, false, false, map[string]string{}, &calls)
	var nested *NestedError
	if !errors.As(err, &nested) {
		t.Fatalf("parseNested over limit returned %v", err)
	}
}

func TestNestedScriptSizeBudgetAllowsLargeSingleScript(t *testing.T) {
	script := strings.Repeat("echo x; ", 15000)
	calls, err := Calls("bash -c '" + script + "echo x'")
	if err != nil {
		t.Fatalf("large single nested script: %v", err)
	}
	if len(calls) < 2 {
		t.Fatalf("large script calls = %d", len(calls))
	}
}

func TestNestedParseBudgetAndDepth(t *testing.T) {
	budget := &nestedParseBudget{limit: len("echo x")}
	calls := []Call(nil)
	if err := parseNestedWithBudget("echo x", nil, 0, []string{"eval"}, false, false, map[string]string{}, &calls, budget); err != nil {
		t.Fatal(err)
	}
	if err := parseNestedWithBudget("echo y", nil, 0, []string{"eval"}, false, false, map[string]string{}, &calls, budget); err == nil {
		t.Fatal("cumulative nested parse budget was not enforced")
	}
	large := strings.Repeat("x ", maxNestedScriptBytes/2+1)
	if err := parseNested(large, nil, 0, []string{"bash"}, false, false, map[string]string{}, &calls); err == nil {
		t.Fatal("individual nested script size limit was not enforced")
	}
}

func TestNoExecAndFallbackShellHeredocs(t *testing.T) {
	for _, command := range []string{
		"bash -E -n <<E\nkill 1\nE",
		"nice - bash <<E\nkill 1\nE",
	} {
		calls, err := Calls(command)
		if err != nil {
			t.Fatalf("%q: %v", command, err)
		}
		found := false
		for _, call := range calls {
			found = found || call.Name == "kill"
		}
		if !found {
			t.Errorf("%q: heredoc call missing: %#v", command, calls)
		}
	}
}

func TestCallDedupKeysAndParallelOffset(t *testing.T) {
	first, _ := callKeyBase(Call{Name: "task", Offset: 2, Args: []string{"a"}})
	second, _ := callKeyBase(Call{Name: "task", Offset: 2, Args: []string{"b"}})
	if first == second {
		t.Fatal("different argument vectors share a deduplication key")
	}
	parallelArgs := []string{"parallel", "-j1", "bash", "-c", "echo x", ":::", "x"}
	parallelCall := cmd{call: Call{Name: "nice", Args: parallelArgs}, words: make([]word, len(parallelArgs)+1)}
	if got := wrapperTargetStart(parallelCall, []string{"bash", "-c", "echo x"}, []string{"parallel", "bash", "-c"}); got != 3 {
		t.Fatalf("parallel command start = %d, want 3", got)
	}
	command := `nice parallel -j1 bash -c 'echo x; kill 1' ::: x`
	calls, err := Calls(command)
	if err != nil {
		t.Fatal(err)
	}
	wantOffset := strings.Index(command, "bash")
	for _, call := range calls {
		if call.Name == "bash" && slices.Contains(call.Via, "parallel") {
			if call.Offset != wantOffset {
				t.Fatalf("parallel bash offset = %d, want %d: %#v", call.Offset, wantOffset, call)
			}
			return
		}
	}
	t.Fatalf("parallel bash call missing: %#v", calls)
}

func TestTmuxQuotedShellCommandAndOptionValues(t *testing.T) {
	command := `tmux new -d bash -c 'echo "it'\''s"; kill 1'`
	calls, err := Calls(command)
	if err != nil {
		t.Fatal(err)
	}
	foundKill := false
	for _, call := range calls {
		foundKill = foundKill || call.Name == "kill"
	}
	if !foundKill {
		t.Fatalf("quoted tmux shell command missing: %#v", calls)
	}
	for _, command := range []string{
		`sudo -R /tmp/x kill -1`,
		`systemd-run --json pretty kill -1`,
		`watch --equexit 3 kill -1`,
		`tmux -T /tmp/name new -d kill -1`,
		`tmux split-window -l 10 -t %1 kill -1`,
		`taskset -pc 0 1`,
	} {
		calls, err := Calls(command)
		if err != nil {
			t.Fatalf("%q: %v", command, err)
		}
		for _, call := range calls {
			if call.Name == "/tmp/x" || call.Name == "pretty" || call.Name == "3" || call.Name == "/tmp/name" || call.Name == "10" || call.Name == "%1" || call.Name == "0" || call.Name == "1" {
				t.Errorf("option value treated as command in %q: %#v", command, calls)
			}
		}
	}
}

func TestNonCommandMarkersAndArguments(t *testing.T) {
	for _, command := range []string{`trap - SIG`, `parallel ::: 1`, `bash --command 'kill 1'`, `su u1 extra -c 'kill 1'`} {
		calls, err := Calls(command)
		if err != nil {
			t.Fatalf("%q: %v", command, err)
		}
		if command == `trap - SIG` {
			if len(calls) != 1 || calls[0].Name != "trap" {
				t.Errorf("trap - expanded as a script: %#v", calls)
			}
			continue
		}
		if command == `parallel ::: 1` {
			if len(calls) != 1 || calls[0].Name != "parallel" {
				t.Errorf("empty parallel command expanded: %#v", calls)
			}
			continue
		}
		for _, call := range calls {
			if call.Name == "kill" {
				t.Errorf("%q unexpectedly expanded as a kill command: %#v", command, calls)
			}
		}
	}
}

func TestWrapperArgumentScansAlwaysAdvance(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{command: `runuser -`},
		{command: `runuser - u1 -c 'kill 1'`, want: true},
		{command: `sudo runuser - a`},
		{command: `script -`},
		{command: `script - x`},
		{command: `runuser - u1 -c 'kill 1'`, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			calls, err := Calls(tt.command)
			if err != nil {
				t.Fatalf("Calls: %v", err)
			}
			found := false
			for _, call := range calls {
				found = found || call.Name == "kill"
			}
			if found != tt.want {
				t.Errorf("kill found = %t, want %t; calls = %#v", found, tt.want, calls)
			}
		})
	}
}

func TestUnknownShellScriptInput(t *testing.T) {
	for _, command := range []string{
		`echo 'kill 1' | bash`,
		`echo 'kill 1' | bash -s`,
		`source <(echo 'kill 1')`,
		`. <(echo 'kill 1')`,
		`bash <(echo 'kill 1')`,
	} {
		calls, err := Calls(command)
		if err != nil {
			t.Fatalf("%q: %v", command, err)
		}
		found := false
		for _, call := range calls {
			if call.ScriptInput && (IsShell(call.Name) || call.Name == "source" || call.Name == ".") {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: no call marked ScriptInput: %#v", command, calls)
		}
	}
	calls, err := Calls(`bash -n`)
	if err != nil || len(calls) != 1 || calls[0].ScriptInput {
		t.Errorf("bash -n = %#v, %v", calls, err)
	}
	calls, err = Calls(`bash -n <<< 'kill 1'`)
	if err != nil || len(calls) != 1 || calls[0].ScriptInput {
		t.Errorf("bash -n here-string = %#v, %v", calls, err)
	}
	calls, err = Calls(`echo k | bash --noexec`)
	if err != nil || len(calls) != 2 || !calls[1].ScriptInput {
		t.Errorf("unsupported --noexec option marked incorrectly: %#v, %v", calls, err)
	}
	for _, command := range []string{`echo k | bash -n`, `bash -n <(echo k)`} {
		calls, err = Calls(command)
		if err != nil {
			t.Errorf("%q: %v", command, err)
			continue
		}
		for _, call := range calls {
			if call.ScriptInput {
				t.Errorf("%q: no-exec shell marked ScriptInput: %#v", command, call)
			}
		}
	}
	for _, command := range []string{
		`echo k | bash -s arg`,
		`if true; then echo k | bash; fi`,
		`( echo k | bash )`,
		`for i in 1; do echo k | bash; done`,
		`f() { echo k | bash; }`,
		`x=$(echo k | bash)`,
		`bash < <(echo k)`,
		`echo k | sudo bash`,
		`echo k | timeout 5 sh`,
		`bash -c 'source <(echo k)'`,
		`sudo bash <(echo k)`,
		`timeout 5 bash <(echo k)`,
		`echo k | sudo -s`,
		`echo k | sudo -i`,
		`echo k | sudo su`,
		`echo k | su u1`,
	} {
		calls, err = Calls(command)
		if err != nil {
			t.Errorf("%q: %v", command, err)
			continue
		}
		found := false
		for _, call := range calls {
			found = found || call.ScriptInput
		}
		if !found {
			t.Errorf("%q: no nested ScriptInput: %#v", command, calls)
		}
	}
	calls, err = Calls("echo done\nbash <<< 'kill 1'")
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if call.Name == "kill" && call.Line != 2 {
			t.Errorf("here-string Line = %d, want 2", call.Line)
		}
	}
}

func TestScriptOptionsDoNotPanic(t *testing.T) {
	for _, command := range []string{`script -c`, `script -qc`, `script --force`, `script -`, `script - x`, `x=1; script --force > /dev/null`} {
		calls, err := Calls(command)
		if err != nil {
			t.Errorf("%q: %v", command, err)
		}
		for _, call := range calls {
			if call.Name == "kill" || call.Name == "pkill" {
				t.Errorf("%q: unexpected signal call %#v", command, call)
			}
		}
	}
}

func TestWrapperCallsAreRetained(t *testing.T) {
	for _, tt := range []struct {
		command, name string
		via           []string
	}{
		{`sudo su -c 'pkill -u u1'`, "su", []string{"sudo"}},
		{`flock /tmp/x bash -c 'kill 1'`, "bash", []string{"flock"}},
	} {
		calls, err := Calls(tt.command)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, call := range calls {
			if call.Name == tt.name && reflect.DeepEqual(call.Via, tt.via) {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: no %s via %q: %#v", tt.command, tt.name, tt.via, calls)
		}
	}
}

func TestNestedScriptDepthLimit(t *testing.T) {
	script := strings.Repeat("eval ", 2000) + "kill 1"
	calls, err := Calls(script)
	var nested *NestedError
	if !errors.As(err, &nested) {
		t.Fatalf("deep eval did not return a depth error: calls=%d err=%v", len(calls), err)
	}

	script = "kill 1"
	for range 5000 {
		script = "find . -exec " + script + ` \;`
	}
	calls, err = Calls(script)
	if !errors.As(err, &nested) {
		t.Fatalf("deep find -exec did not return a depth error: calls=%d err=%v", len(calls), err)
	}
}

func TestKillGuardExistingForms(t *testing.T) {
	for _, command := range []string{
		`sudo -iu u1 kill 1`,
		`$'kill' 1`,
		`K=kill; $K 1`,
		`f() { kill 1; }; f`,
		`coproc kill 1`,
		`while test x; do kill 1; done`,
		`\kill 1`,
		"bash <<'EOF'\nkill 1\nEOF",
	} {
		calls, err := Calls(command)
		if err != nil {
			t.Fatalf("%q: %v", command, err)
		}
		var found []Call
		for _, call := range calls {
			if call.Name == "kill" {
				found = append(found, call)
			}
		}
		if len(found) != 1 || !reflect.DeepEqual(found[0].Args, []string{"1"}) {
			t.Errorf("%q: kill calls = %#v", command, found)
		}
	}
}

func TestCallsContinueAfterNestedParseErrors(t *testing.T) {
	cases := []struct {
		command string
		name    string
		via     []string
	}{
		{`bash -c 'if' ; sudo tool history`, "tool", []string{"sudo"}},
		{`bash -c 'if' ; timeout 5 pgrep x`, "pgrep", []string{"timeout"}},
		{"bash -c 'if' ; bash <<E\nkill 1\nE", "kill", []string{"bash"}},
	}
	for _, tc := range cases {
		t.Run(tc.name+tc.command, func(t *testing.T) {
			calls, err := Calls(tc.command)
			var nested *NestedError
			if !errors.As(err, &nested) {
				t.Fatalf("Calls error = %v, want NestedError", err)
			}
			for _, call := range calls {
				if call.Name == tc.name {
					if !reflect.DeepEqual(call.Via, tc.via) {
						t.Fatalf("call = %#v, Via = %q", call, call.Via)
					}
					return
				}
			}
			t.Fatalf("%s missing after nested error: %#v", tc.name, calls)
		})
	}
}

func TestMainCompatibleWrapperCalls(t *testing.T) {
	for _, command := range []string{
		`ssh -l --command + su sh 'bash -c kill'`,
	} {
		t.Run(command, func(t *testing.T) {
			calls, err := Calls(command)
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range calls {
				if call.Name == "kill" {
					return
				}
			}
			t.Fatalf("main-compatible nested shell call missing: %#v", calls)
		})
	}
	for _, command := range []string{
		`nice - kill -1`,
		`sudo - kill -1`,
		`env -iS -1 kill`,
	} {
		t.Run(command, func(t *testing.T) {
			calls, _ := Calls(command)
			for _, call := range calls {
				if call.Name == "kill" {
					return
				}
			}
			t.Fatalf("main fallback call missing: %#v", calls)
		})
	}
}

func TestWrapperOptionTablesAndDepthLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"sudo", []string{"-R", "/tmp/x", "kill"}, 2},
		{"unshare", []string{"--setgroups", "deny", "kill"}, 2},
		{"systemd-run", []string{"--json", "pretty", "kill"}, 2},
		{"tmux", []string{"-T", "/tmp/name", "new", "-d"}, 2},
		{"strace", []string{"-efoo", "grep"}, 1},
		{"sudo", []string{"-n", "grep"}, 1},
		{"timeout", []string{"-v", "5", "grep"}, 1},
	} {
		if got := wrapperOptionEnd(tc.name, tc.args, 0); got != tc.want {
			t.Errorf("%s option end = %d, want %d", tc.name, got, tc.want)
		}
		if !wrapperOptionsKnown(tc.name, tc.args) {
			t.Errorf("%s known options rejected: %q", tc.name, tc.args)
		}
	}
	target, _, ok := unwrapMain("env", []string{"-i", "A=1", "kill", "1"})
	if !ok || !reflect.DeepEqual(target, []string{"kill", "1"}) {
		t.Fatalf("env -i baseline target = %q, %v", target, ok)
	}
	args := make([]string, maxNestedScripts+1)
	for i := range args {
		args[i] = "nice"
	}
	args = append(args, "kill")
	if _, _, ok := unwrapMain("nice", args); ok {
		t.Fatal("baseline wrapper depth limit was not enforced")
	}
}

func TestUnknownWrapperOptionsUseMainBehavior(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    string
	}{
		{"strace -Z val grep kill /tmp/notes", "strace"},
		{"sudo -Z val grep kill /tmp/notes", "val"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			calls, err := Calls(tc.command)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, call := range calls {
				if call.Name == "kill" || call.Name == "grep" {
					t.Fatalf("unknown key invented a command: %#v", calls)
				}
				found = found || call.Name == tc.want
			}
			if !found {
				t.Fatalf("main-compatible call %q missing: %#v", tc.want, calls)
			}
		})
	}
	calls, err := Calls("strace -Z val bash -c 'kill 1'")
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if call.Name == "kill" {
			return
		}
	}
	t.Fatalf("main shell search after unknown key was lost: %#v", calls)
}

func TestBenignWrapperArgumentsDoNotBecomeCommands(t *testing.T) {
	commands := []string{
		"strace -f grep kill /tmp/log",
		"watch -d grep kill notes.txt",
		"unshare -r grep kill notes.txt",
		"setpriv --init-groups grep kill notes.txt",
		"flock -x /tmp/l grep kill notes.txt",
		"sudo -A grep kill f",
		"sudo -n grep kill f",
		"timeout -v 5 grep kill f",
		"systemd-run -q --user grep kill f",
		"parallel -k grep kill ::: a b",
		"doas -n grep kill f",
		"ionice -t -c3 grep kill f",
		"xargs -i grep kill {}",
		"strace -f echo foo bar",
		"strace -efoo grep kill notes.txt",
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			calls, err := Calls(command)
			if err != nil {
				t.Fatalf("ordinary command returned error: %v", err)
			}
			for _, call := range calls {
				if call.Name == "kill" {
					t.Fatalf("argument reported as command: %#v", calls)
				}
				if command == "strace -f echo foo bar" && call.Name == "foo" {
					t.Fatalf("echo argument reported as command: %#v", calls)
				}
			}
		})
	}
}

func TestMainWrappersUseTheirOwnCommandRoute(t *testing.T) {
	args := []string{"grep", "bash", "-c", "echo x"}
	c := cmd{call: Call{Name: "sudo", Args: args}, words: make([]word, len(args)+1)}
	if _, ok := genericShellTarget(c, "sudo", args, -1); ok {
		t.Fatal("main wrapper was rescanned as a generic shell command")
	}
}

func TestWrapperRouteDeduplicationUsesSourceCall(t *testing.T) {
	call := Call{Name: "echo", Args: []string{"x"}, Offset: 10, Via: []string{"exact"}}
	calls := []Call(nil)
	seen := newCallSet()
	appendCall(seen, &calls, call)
	c := cmd{
		call:  Call{Name: "sudo", Args: []string{"echo", "x"}},
		words: []word{{offset: 0}, {offset: 10}, {offset: 15}},
	}
	if err := emitMainTarget(c, []string{"echo", "x"}, []string{"main"}, true, 0, map[string]string{}, &calls, seen); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("same source call emitted through multiple routes: %#v", calls)
	}
}

func TestAttachedShortOptionValueDoesNotHideFollowingOption(t *testing.T) {
	if wrapperOptionsKnown("strace", []string{"-efoo", "-Q", "grep"}) {
		t.Fatal("unknown option after an attached value was skipped")
	}
}

func TestKnownWrapperOptionsAndEnvAttachedValue(t *testing.T) {
	cases := []struct {
		command string
		name    string
		args    []string
		via     []string
	}{
		{`env -uSHELL pkill -u u1`, "pkill", []string{"-u", "u1"}, []string{"env"}},
		{`systemd-run --background red kill -1`, "red", []string{"kill", "-1"}, []string{"systemd-run"}},
		{`systemd-run --expand-environment no kill -1`, "no", []string{"kill", "-1"}, []string{"systemd-run"}},
		{`tmux splitw -h -p 50 kill -1`, "kill", []string{"-1"}, []string{"tmux"}},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			calls, err := Calls(tc.command)
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range calls {
				if call.Name == tc.name && reflect.DeepEqual(call.Args, tc.args) && reflect.DeepEqual(call.Via, tc.via) {
					return
				}
			}
			t.Fatalf("wanted %s %q via %q: %#v", tc.name, tc.args, tc.via, calls)
		})
	}
	calls, err := Calls(`env -uSHELL pkill -u u1`)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if call.Name == "HELL" {
			t.Fatalf("unset value split into a false command: %#v", calls)
		}
	}
}

func TestNestedSudoReadsStdinOnlyWithoutCommand(t *testing.T) {
	for _, command := range []string{
		`curl x | timeout 5 sudo -s`,
		`curl x | nice sudo -i`,
		"timeout 5 sudo -s <<E\nkill 1\nE",
		"nice sudo -i <<E\nkill 1\nE",
		"sudo -u u1 sudo -s <<E\nkill 1\nE",
	} {
		t.Run(command, func(t *testing.T) {
			calls, err := Calls(command)
			if err != nil {
				t.Fatal(err)
			}
			var foundInput bool
			for _, call := range calls {
				foundInput = foundInput || call.Name == "sudo" && call.ScriptInput
			}
			if !strings.Contains(command, "<<E") && !foundInput {
				t.Fatalf("sudo ScriptInput missing: %#v", calls)
			}
		})
	}
}

func TestSudoCommandsReceiveHeredocAsData(t *testing.T) {
	for _, command := range []string{
		"sudo -i -u u1 psql <<E\nselect 1 where (x;\nE",
		"sudo -s cat <<E\nkill 1\nE",
		"timeout 30 sudo -s cat <<E\nkill 1\nE",
		"cat <<E | nice sudo -s tee /tmp/x\nkill 1\nE",
	} {
		t.Run(command, func(t *testing.T) {
			calls, err := Calls(command)
			if err != nil {
				t.Fatalf("data heredoc returned error: %v; calls=%#v", err, calls)
			}
			for _, call := range calls {
				if call.ScriptInput || call.Name == "kill" {
					t.Fatalf("command stdin treated as shell input: %#v", calls)
				}
			}
		})
	}
}

func TestFourLargeNestedHeredocScriptsFitBudget(t *testing.T) {
	script := strings.Repeat("echo x\n", 2500)
	for range 4 {
		script = "bash <<'T'\n" + script + "T\n"
	}
	calls, err := Calls(script)
	if err != nil {
		t.Fatalf("four nested 20 KiB scripts: %v", err)
	}
	for _, call := range calls {
		if call.Name == "echo" {
			return
		}
	}
	t.Fatalf("nested script calls missing: %d calls", len(calls))
}

func TestOffset(t *testing.T) {
	tests := []struct {
		name, command, program string
		want                   []int
	}{
		{"wrapper and list", "timeout 5 pgrep -f x && pgrep -f y", "pgrep", []int{10, 24}},
		{"wrapper itself", "timeout 5 pgrep -f x", "timeout", []int{0}},
		{"second line and path", "echo a\nsudo -u r /usr/bin/pgrep x", "pgrep", []int{17}},
		{"exec target", "graft exec --heavy -- pgrep x", "pgrep", []int{22}},
		{"time keyword", "time pgrep x", "time", []int{0}},
		{"bash -c", "bash -c 'pgrep x'", "pgrep", []int{9}},
		{"bash -c double-quoted with escapes", `bash -c "echo \"a\" \$b; pgrep x"`, "pgrep", []int{25}},
		{"bash -c bare word", `sh -c pgrep`, "pgrep", []int{6}},
		{"nested bash -c", `bash -c "sh -c 'pgrep x'"`, "pgrep", []int{16}},
		{"ssh remote words", `ssh -p 22 h echo a\; pgrep x`, "pgrep", []int{21}},
		{"ssh quoted remote script", "ssh h 'cd /x && pgrep y'", "pgrep", []int{16}},
		{"line continuation in a double-quoted script", "bash -c \"echo a \\\n; pgrep x\"", "echo", []int{9}},
		{"call after a line continuation in double quotes", "bash -c \"echo a \\\n; pgrep x\"", "pgrep", []int{20}},
		{"program after a line continuation in double quotes", "bash -c \"echo a; \\\npgrep x\"", "pgrep", []int{19}},
		{"line continuation in a bare script", "bash -c echo\\ a\\\n\\;pgrep", "pgrep", []int{19}},
		{"ansi-c quoted script", "bash -c $'pgrep x'", "pgrep", []int{10}},
		{"shell heredoc", "bash <<'EOF'\npgrep x\nEOF", "pgrep", []int{13}},
		{"variable argv0", "P=/usr/bin/pgrep; $P x", "pgrep", []int{-1}},
		{"substitution", "echo $(pgrep x)", "pgrep", []int{7}},
		{"substitution in heredoc", "cat <<EOF\n$(pgrep x)\nEOF", "pgrep", []int{12}},
		{"substitution in shell heredoc script", "bash <<'EOF'\necho $(pgrep x)\nEOF", "pgrep", []int{20}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls, err := Find(tt.command, tt.program)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]int, len(calls))
			for i, c := range calls {
				got[i] = c.Offset
				word, _, _ := strings.Cut(tt.command[max(c.Offset, 0):], " ")
				if c.Offset >= 0 && !strings.HasSuffix(word, c.Name) {
					t.Errorf("offset %d does not point at %s in %q", c.Offset, c.Name, tt.command)
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("offsets = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVariableArgv0Split(t *testing.T) {
	tests := []struct {
		command, name string
		args          []string
		via           []string
		offset        int
	}{
		{`P="pgrep -f"; $P x`, "pgrep", []string{"-f", "x"}, nil, -1},
		{"P='pgrep\t-f'; ${P} x", "pgrep", []string{"-f", "x"}, nil, -1},
		{`P="pgrep -f"; "$P" x`, "pgrep -f", []string{"x"}, nil, -1},
		{`P="timeout 5"; $P pgrep x`, "pgrep", []string{"x"}, []string{"timeout"}, 18},
		{`C="curl -u $U:$K"; $C "$url"`, "curl", []string{"-u", "$U:$K", `"$url"`}, nil, -1},
		{`R="$HOME/bin/tool"; $R x`, "tool", []string{"x"}, nil, -1},
		{`P=" pgrep"; $P x`, "pgrep", []string{"x"}, nil, -1},
		{`P="pgrep "; $P x`, "pgrep", []string{"x"}, nil, -1},
		{`P="   "; $P pgrep x`, "pgrep", []string{"x"}, nil, 12},
		{`P=""; $P pgrep x`, "pgrep", []string{"x"}, nil, 9},
		{`P="$(command -v pgrep) -f"; $P x`, "", []string{"-f", "x"}, nil, -1},
		{`P="${T:-pgrep -f}"; $P x`, "${T:-pgrep -f}", []string{"x"}, nil, -1},
	}
	for _, tt := range tests {
		calls, err := Calls(tt.command)
		if err != nil || len(calls) == 0 {
			t.Fatalf("%q: Calls() = %#v, %v", tt.command, calls, err)
		}
		got := calls[len(calls)-1]
		if got.Name != tt.name || !reflect.DeepEqual(got.Args, tt.args) || !reflect.DeepEqual(got.Via, tt.via) {
			t.Errorf("%q: call = %q %q via %q; want %q %q via %q", tt.command, got.Name, got.Args, got.Via, tt.name, tt.args, tt.via)
		}
		if got.Offset != tt.offset {
			t.Errorf("%q: Offset = %d; want %d", tt.command, got.Offset, tt.offset)
		}
	}
}

func TestCallsExternalParseErrorReturnsNil(t *testing.T) {
	calls, err := Calls("echo before; if")
	if err == nil || calls != nil {
		t.Fatalf("Calls() = %#v, %v; want nil and parse error", calls, err)
	}
}

func TestRedirs(t *testing.T) {
	cmd := "echo a\ncat > out.txt <<'EOF'\nline $x\nEOF\nsudo tee -a \"$f\" 2>>err.log <<< hi"
	calls, err := Calls(cmd)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Call{}
	for _, c := range calls {
		byName[c.Name] = c
	}
	cat := byName["cat"].Redirs
	if len(cat) != 2 || cat[0].Op != ">" || cat[0].Target != "out.txt" || cat[0].BodyStart != -1 || cat[1].Op != "<<" || cat[1].Target != "EOF" {
		t.Fatalf("cat redirs = %#v", cat)
	}
	if cat[1].Body != "line $x\n" || cmd[cat[1].BodyStart:cat[1].BodyEnd] != cat[1].Body {
		t.Fatalf("heredoc body %q at %d:%d", cat[1].Body, cat[1].BodyStart, cat[1].BodyEnd)
	}
	tee := byName["tee"].Redirs
	if len(tee) != 2 || tee[0].N != "2" || tee[0].Op != ">>" || tee[0].Target != "err.log" || tee[1].Op != "<<<" || tee[1].Target != "hi" {
		t.Fatalf("tee redirs (under sudo) = %#v", tee)
	}
	if !reflect.DeepEqual(byName["sudo"].Redirs, tee) || byName["echo"].Redirs != nil {
		t.Fatalf("sudo %#v, echo %#v", byName["sudo"].Redirs, byName["echo"].Redirs)
	}
	nested, err := Find("bash -c 'cat > f <<EOF\nx\nEOF'", "cat")
	if err != nil || len(nested) != 1 {
		t.Fatalf("nested = %#v, %v", nested, err)
	}
	if r := nested[0].Redirs; len(r) != 2 || r[1].Body != "x\n" || r[1].BodyStart != 23 || r[1].BodyEnd != 25 {
		t.Fatalf("nested redirs = %#v", r)
	}
	open, err := Find("cat <<EOF\nab", "cat")
	if err != nil || len(open) != 1 || open[0].Redirs[0].Body != "ab" || open[0].Redirs[0].BodyEnd != 12 {
		t.Fatalf("unclosed heredoc = %#v, %v", open, err)
	}
	empty, err := Find("cat <<EOF\nEOF", "cat")
	if err != nil || len(empty) != 1 || len(empty[0].Redirs) != 1 || empty[0].Redirs[0].Body != "" {
		t.Fatalf("empty heredoc = %#v, %v", empty, err)
	}
}

// checkPositions — смещение указывает на слово с именем программы, границы тела heredoc вырезают Body.
func checkPositions(t *testing.T, line int, cmd string, calls []Call) {
	t.Helper()
	for _, c := range calls {
		if c.Offset >= 0 {
			word := strings.FieldsFunc(cmd[c.Offset:], func(r rune) bool {
				return r == ' ' || r == '\n' || r == '\t' || r == ';' || r == '|' || r == '&' || r == ')'
			})
			if len(word) == 0 || !strings.Contains(word[0], c.Name) {
				t.Errorf("строка %d: Offset %d не указывает на %q", line, c.Offset, c.Name)
			}
		}
		nested := slices.ContainsFunc(c.Via, func(v string) bool { return v == "ssh" || v == "-c" || IsShell(v) })
		for _, r := range c.Redirs {
			if r.BodyStart < 0 {
				continue
			}
			span := cmd[r.BodyStart:r.BodyEnd]
			// Во вложенном скрипте текст диапазона отличается от Body кавычками и экранированием, края — те же.
			if nested && span != "" && r.Body != "" && span[0] == r.Body[0] && span[len(span)-1] == r.Body[len(r.Body)-1] {
				continue
			}
			if span != r.Body {
				t.Errorf("строка %d: границы тела heredoc %d:%d не совпадают с Body", line, r.BodyStart, r.BodyEnd)
			}
		}
	}
}

func TestCorpus(t *testing.T) {
	path := os.Getenv("SHELLCMD_CORPUS")
	if path == "" {
		t.Skip("SHELLCMD_CORPUS is not set")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	started := time.Now()
	var total, parsed, failures, bashRejected, nestedRejected, backtickRejected int
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		total++
		var row struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Errorf("строка %d: некорректный JSON: %v", total, err)
			continue
		}
		calls, parseErr := Calls(row.Command)
		if parseErr == nil {
			parsed++
			checkPositions(t, total, row.Command, calls)
			continue
		}
		failures++
		var nestedErr *NestedError
		if errors.As(parseErr, &nestedErr) {
			if err := exec.Command("bash", "-n", "-c", nestedErr.Script).Run(); err == nil {
				t.Errorf("строка %d: вложенный скрипт не разобран, но bash -n его принял: %v", total, parseErr)
				continue
			}
			nestedRejected++
			bashRejected++
			continue
		}
		if err := exec.Command("bash", "-n", "-c", row.Command).Run(); err != nil {
			bashRejected++
			continue
		}
		// bash -n не разбирает подстановку в обратных кавычках: сломанная внутри неё команда
		// падает только при запуске. Без этих подстановок команда должна разбираться.
		if _, err := Calls(backticks.ReplaceAllString(row.Command, "``")); err == nil {
			backtickRejected++
			continue
		}
		t.Errorf("строка %d: shellcmd не разобрал команду, принятую bash -n: %v", total, parseErr)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("корпус: всего %d, разобрано %d, ошибок %d (bash -n тоже не принял %d; вложенный скрипт не разобран: %d; сломана подстановка в обратных кавычках: %d), время %s", total, parsed, failures, bashRejected, nestedRejected, backtickRejected, time.Since(started).Round(time.Millisecond))
}

var backticks = regexp.MustCompile("`[^`]*`")

func TestAddTimeRejectsInvalidSourceSpan(t *testing.T) {
	file, _, err := parseShell("time rg x")
	if err != nil {
		t.Fatal(err)
	}
	var clause *syntax.TimeClause
	syntax.Walk(file, func(node syntax.Node) bool {
		if got, ok := node.(*syntax.TimeClause); ok {
			clause = got
		}
		return true
	})
	if clause == nil {
		t.Fatal("parsed time clause is missing")
	}
	var calls []Call
	walker := callWalker{src: "", calls: &calls}
	walker.addTime(clause)
	if walker.parseErr == nil || walker.parseErr.Error() != "shellcmd: invalid source span for time clause" || len(calls) != 0 {
		t.Fatalf("addTime result = calls %#v, error %v", calls, walker.parseErr)
	}
}

func TestTimeClauseDetails(t *testing.T) {
	tests := []struct {
		command, raw string
		offset       int
		via          []string
	}{
		{command: `time -p rg x`, raw: `time -p rg x`, offset: 0},
		{command: `/usr/bin/time -f '%e' rg x`, raw: `/usr/bin/time -f '%e' rg x`, offset: 0},
		{command: `time -f '%e' time rg x`, raw: `time -f '%e' time rg x`, offset: 0},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			calls, err := Calls(tt.command)
			if err != nil {
				t.Fatal(err)
			}
			if len(calls) == 0 || calls[0].Name != "time" || calls[0].Raw != tt.raw || calls[0].Offset != tt.offset || !reflect.DeepEqual(calls[0].Via, tt.via) {
				t.Fatalf("time call = %#v, want raw %q offset %d via %q", calls, tt.raw, tt.offset, tt.via)
			}
		})
	}
}

func TestHeredocBodyLen(t *testing.T) {
	tests := []struct {
		name, text, delimiter string
		want                  int
	}{
		{name: "delimiter line", text: "body\nEND", delimiter: "END", want: len("body\n")},
		{name: "other last line", text: "body\nOTHER", delimiter: "END", want: len("body\nOTHER")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := heredocBodyLen(tt.text, tt.delimiter); got != tt.want {
				t.Fatalf("body length = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestHeredocBodyEdges(t *testing.T) {
	tests := []struct {
		name, command, wantBody string
	}{
		{name: "delimiter at EOF", command: `cat <<EOF
body
EOF`, wantBody: "body\n"},
		{name: "unterminated at EOF", command: `cat <<EOF
body
last`, wantBody: "body\nlast"},
		{name: "continued body line", command: `cat <<EOF
first \
second
EOF
`, wantBody: "first " + string([]byte{92}) + "\nsecond\n"},
		{name: "nested heredoc", command: `cat <<OUT
outer
cat <<IN
inner
IN
OUT
`, wantBody: "outer\ncat <<IN\ninner\nIN\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls, err := Find(tt.command, "cat")
			if err != nil || len(calls) != 1 || len(calls[0].Redirs) != 1 || calls[0].Redirs[0].Op != "<<" {
				t.Fatalf("Find = %#v, %v", calls, err)
			}
			if got := calls[0].Redirs[0].Body; got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}

func TestNestedHeredocBounds(t *testing.T) {
	tests := []struct {
		name, command, first, last string // first, last — начало и конец тела в исходной команде
	}{
		{"double-quoted script with splices", "/bin/bash -lc \"cat > R.md <<'EOF'\nchecks: \"'`bash -n x.sh`'\" \\\"done\\\"\nEOF\ngit status\"", "checks: ", "done\\\"\n"},
		{"single-quoted script", "bash -c 'cat > f <<EOF\nab\nEOF'", "ab", "ab\n"},
		{"ssh remote script", "ssh h 'cat > f <<EOF\nab\nEOF\necho ok'", "ab", "ab\n"},
		{"nested shell", "bash -c \"sh -c 'cat > f <<EOF\nab\nEOF'\"", "ab", "ab\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls, err := Find(tt.command, "cat")
			if err != nil || len(calls) != 1 || len(calls[0].Redirs) != 2 {
				t.Fatalf("calls = %#v, %v", calls, err)
			}
			r := calls[0].Redirs[1]
			start, end := strings.Index(tt.command, tt.first), strings.LastIndex(tt.command, tt.last)+len(tt.last)
			if r.BodyStart != start || r.BodyEnd != end {
				t.Errorf("body %q at %d:%d, want %d:%d (%q)", r.Body, r.BodyStart, r.BodyEnd, start, end, tt.command[max(start, 0):max(end, 0)])
			}
		})
	}
	ansi := "bash -c $'cat > f <<EOF\\nab\\nEOF'"
	calls, err := Find(ansi, "cat")
	if err != nil || len(calls) != 1 || len(calls[0].Redirs) != 2 ||
		calls[0].Redirs[1].BodyStart != strings.Index(ansi, "ab") ||
		calls[0].Redirs[1].BodyEnd != strings.Index(ansi, "ab")+3 ||
		ansi[calls[0].Redirs[1].BodyStart:calls[0].Redirs[1].BodyEnd] != "ab\\" {
		t.Errorf("ansi-c quoted script bounds = %#v, %v", calls, err)
	}
}

// Входы, на которых разбор падал паникой: недочитанные heredoc подряд, `time` без команды, проза.
var noPanicInputs = []string{
	"a <<A; b <<B",
	"a <<'B'\\n+x; b <<'B'\\n+y",
	"time",
	"echo `time`; x=$(time)",
	"Приёмка дошла до `graft gate` и поймала ошибку: не хватало импорта `time`.",
	"bash -c 'a <<A; b <<B'",
	"bash <<'E'\na <<A; b <<B\nE",
	"bash -c 'time'",
	"sh<<E\n!",
	"<<0 \\",
}

func TestCallsNoPanic(t *testing.T) {
	for _, command := range noPanicInputs {
		calls, err := Calls(command)
		for _, c := range calls {
			for _, r := range c.Redirs {
				if !validBounds(r, command) {
					t.Errorf("%q: heredoc bounds %d:%d outside %d", command, r.BodyStart, r.BodyEnd, len(command))
				}
			}
		}
		t.Logf("%q: %d calls, %v", command, len(calls), err)
	}
}

func FuzzCalls(f *testing.F) {
	for _, command := range noPanicInputs {
		f.Add(command)
	}
	for _, command := range []string{
		"bash -c 'pgrep x'", "ssh h -p 22 -- 'cat <<E\n$(kill 1)\nE'", "P=\"pgrep -f\"; $P x",
		"cat > f <<'EOF'\nbody\nEOF\nsudo tee -a \"$f\" 2>>err.log <<< hi", "bash -c \"echo a\\\n;pgrep\"",
		"bash -c $'pgrep x'", "for x in a; do sleep 1 & done", "timeout 5 bash <<'EOF'\necho $(pgrep x)\nEOF",
	} {
		f.Add(command)
	}
	f.Fuzz(func(t *testing.T, command string) {
		calls, _ := Calls(command)
		for _, c := range calls {
			if c.Offset >= len(command) {
				t.Errorf("Offset %d outside %d", c.Offset, len(command))
			}
			for _, r := range c.Redirs {
				if !validBounds(r, command) {
					t.Errorf("heredoc bounds %d:%d outside %d", r.BodyStart, r.BodyEnd, len(command))
				}
			}
		}
	})
}

// validBounds — границы тела heredoc: обе -1 или диапазон внутри команды.
func validBounds(r Redir, command string) bool {
	if r.BodyStart == -1 && r.BodyEnd == -1 {
		return true
	}
	return r.BodyStart >= 0 && r.BodyStart <= r.BodyEnd && r.BodyEnd <= len(command)
}

func TestAppendedDelimiterNotArgument(t *testing.T) {
	for command, want := range map[string][]string{
		"pgrep a <<E \\":       {"a"},
		"cat <<E; pgrep z \\":  {"z"},
		"pgrep b\\\\ <<E":      {"b\\"},
		"pgrep c <<E\nline \\": {"c"},
	} {
		calls, err := Find(command, "pgrep")
		if err != nil || len(calls) != 1 || !reflect.DeepEqual(calls[0].Args, want) {
			t.Errorf("%q: Find = %#v, %v; want args %q", command, calls, err, want)
		}
	}
}

func TestShadowedByFunction(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
	}{
		{"w() { echo running; }; w", true},
		{"w; w() { echo running; }", false},
		{"uptime() { :; }; w", false},
		{"w", false},
	} {
		calls, err := Calls(tc.cmd)
		if err != nil {
			t.Fatal(err)
		}
		var probe Call
		for _, c := range calls {
			if c.Name == "w" {
				probe = c
			}
		}
		got, err := probe.ShadowedByFunction(tc.cmd)
		if err != nil || got != tc.want {
			t.Errorf("%q: got %v, %v; want %v", tc.cmd, got, err, tc.want)
		}
	}
	if _, err := (Call{Name: "w"}).ShadowedByFunction("w() {"); err == nil {
		t.Error("unparsable command: no error")
	}
}

func callsNamed(calls []Call, name string) int {
	n := 0
	for _, call := range calls {
		if call.Name == name {
			n++
		}
	}
	return n
}

func TestANSIQuotedMultilineCallKeepsPhysicalLine(t *testing.T) {
	src := "true\ntrue\nbash -c $'echo start\\nkill 1'"
	calls, err := Find(src, "kill")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Line != 3 {
		t.Fatalf("ANSI-C call = %#v, want one call on physical line 3", calls)
	}
}

func TestSSHMalformedRemotePrefixPreservesNestedError(t *testing.T) {
	for _, src := range []string{
		"ssh host '; sh script.sh' <<'EOF'\necho start\nkill 1\nEOF",
		"ssh host '; sh script.sh' <<<x",
	} {
		_, err := Find(src, "kill")
		var nested *NestedError
		if !errors.As(err, &nested) {
			t.Fatalf("Find(%q) error = %v; want NestedError", src, err)
		}
	}
}

func TestANSIEscapesBecomeShellSyntax(t *testing.T) {
	for _, tt := range []struct {
		name, escaped string
	}{
		{"newline", `\n`},
		{"hex newline", `\x0a`},
		{"octal newline", `\012`},
		{"unicode newline", `\u000a`},
		{"long unicode newline", `\U0000000a`},
		{"control newline", `\cJ`},
		{"hex semicolon", `\x3b`},
		{"octal semicolon", `\073`},
		{"unicode semicolon", `\u003b`},
		{"long unicode semicolon", `\U0000003b`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := "bash -c $'echo x" + tt.escaped + "kill 1'"
			calls, err := Find(src, "kill")
			if err != nil || len(calls) != 1 {
				t.Fatalf("Find(%q) = %#v, %v; want kill call", src, calls, err)
			}
		})
	}
}

func TestANSILiteralBackslashNDoesNotSplitScript(t *testing.T) {
	calls, err := Find(`bash -c $'echo x\\nkill 1'`, "kill")
	if err != nil || len(calls) != 0 {
		t.Fatalf("literal \\n split script: %#v, %v", calls, err)
	}
}

func TestANSIDecodingAcrossScriptEntryPoints(t *testing.T) {
	for _, src := range []string{
		`sh -c $'echo x\x0akill 1'`,
		`eval $'echo x\x0akill 1'`,
		`ssh host $'echo x\x0akill 1'`,
	} {
		t.Run(src[:strings.IndexByte(src, ' ')], func(t *testing.T) {
			calls, err := Find(src, "kill")
			if err != nil || len(calls) != 1 {
				t.Fatalf("Find(%q) = %#v, %v; want kill call", src, calls, err)
			}
		})
	}
}

func TestNestedCallLinesFollowSourceOffsets(t *testing.T) {
	for _, tt := range []struct {
		name, src string
		line      int
	}{
		{"literal multiline bash", "bash -c 'echo start\nkill 1'", 2},
		{"ANSI multiline bash", "true\ntrue\nbash -c $'echo start\\nkill 1'", 3},
		{"ANSI multiline eval", `eval $'echo start\nkill 1'`, 1},
		{"ANSI multiline ssh", `ssh host $'echo start\nkill 1'`, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls, err := Find(tt.src, "kill")
			if err != nil || len(calls) != 1 || calls[0].Line != tt.line {
				t.Fatalf("Find(%q) = %#v, %v; want line %d", tt.src, calls, err, tt.line)
			}
		})
	}
}

func TestCallSetKeepsSameCallWithDifferentVia(t *testing.T) {
	seen := newCallSet()
	base := Call{Name: "kill", Args: []string{"-9", "1234"}, Offset: 7}
	direct, viaSudo, viaNice := base, base, base
	viaSudo.Via = []string{"sudo"}
	viaNice.Via = []string{"nice"}
	for _, call := range []Call{direct, viaSudo, viaNice} {
		if seen.hasExact(call) {
			t.Fatalf("%v reported seen before add", call.Via)
		}
		seen.add(call)
		if !seen.hasExact(call) {
			t.Fatalf("%v not seen after add", call.Via)
		}
	}
	seen.add(viaNice)
	key, _ := callBaseIdentity(base)
	if got := len(seen.more[key]); got != 2 {
		t.Fatalf("calls with the same identity kept beyond the first: %d, want 2 (sudo and nice once each)", got)
	}
	if !seen.hasBase(Call{Name: "kill", Args: []string{"-9", "1234"}, Offset: 7, Via: []string{"timeout"}}) {
		t.Fatal("same name, args and offset with another Via not found by base identity")
	}
}

func TestSSHRemoteHeredocRoundThree(t *testing.T) {
	body := "echo start\nkill 1\nEOF"
	for _, tt := range []struct {
		name    string
		src     string
		wantVia [][]string
	}{
		{"ansi remote shell word", "ssh host $'bash\\x20-s' <<'EOF'\n" + body, [][]string{{"ssh", "bash"}}},
		{"second remote command reads stdin", "ssh host 'true; bash -s' <<'EOF'\n" + body, [][]string{{"ssh", "bash"}}},
		{"stdin shell heredoc substitution is local", "ssh host <<EOF\necho $(kill 1)\nEOF", [][]string{nil}},
		{"quoted heredoc substitution is remote only", "ssh host <<'EOF'\necho $(kill 1)\nEOF", [][]string{{"ssh", "bash"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls, err := Find(tt.src, "kill")
			if err != nil {
				t.Fatalf("Find error: %v", err)
			}
			if len(calls) != len(tt.wantVia) {
				t.Fatalf("Find = %#v; want %d calls", calls, len(tt.wantVia))
			}
			for _, via := range tt.wantVia {
				found := false
				for _, call := range calls {
					if reflect.DeepEqual(call.Via, via) {
						found = true
					}
				}
				if !found {
					t.Errorf("Find = %#v; missing Via %v", calls, via)
				}
			}
		})
	}
}

func TestSSHHeredocDataAndDisabledRemoteExecution(t *testing.T) {
	for _, src := range []string{
		"ssh -N host <<'EOF'\necho start\nkill 1\nEOF",
		"ssh -W target:22 host <<'EOF'\necho start\nkill 1\nEOF",
		"ssh -O check host <<'EOF'\necho start\nkill 1\nEOF",
		"ssh -s host sftp <<'EOF'\necho start\nkill 1\nEOF",
		"ssh <<'EOF'\necho start\nkill 1\nEOF",
		"ssh host 'bash -s </dev/null' <<'EOF'\necho start\nkill 1\nEOF",
		"ssh -n host <<'EOF'\necho start\nkill 1\nEOF",
	} {
		calls, err := Find(src, "kill")
		if err != nil || len(calls) != 0 {
			t.Errorf("Find(%q) = %#v, %v; want no kill", src, calls, err)
		}
	}
}

func TestSSHWithForkOptionDisablesStdin(t *testing.T) {
	calls, err := Find("ssh -f host 'bash -s' <<'EOF'\necho start\nkill 1\nEOF", "kill")
	if err != nil || len(calls) != 0 {
		t.Fatalf("Find = %#v, %v; -f implies -n, so body is data", calls, err)
	}
}

func TestSSHRemoteSyntaxErrorsArePreserved(t *testing.T) {
	for _, src := range []string{
		"ssh host '; bad' <<'EOF'\nx\nEOF",
		"ssh host '; bad' <<<x",
	} {
		_, err := Find(src, "kill")
		var nested *NestedError
		if !errors.As(err, &nested) {
			t.Errorf("Find(%q) error = %v; want NestedError", src, err)
		}
	}
}

func TestANSILiteralNewlineMapsPhysicalLines(t *testing.T) {
	for _, tt := range []struct {
		name, src string
		lines     []int
	}{
		{"bash", "true\nbash -c $'echo x\nkill 1'", []int{3}},
		{"eval", "eval $'echo x\nkill 1'", []int{2}},
		{"ssh", "ssh host $'echo x\nkill 1'", []int{2}},
		{"mixed literal and escaped newline", "bash -c $'echo x\nkill 1\\nkill 1'", []int{2, 2}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls, err := Find(tt.src, "kill")
			if err != nil || len(calls) != len(tt.lines) {
				t.Fatalf("Find = %#v, %v; want lines %v", calls, err, tt.lines)
			}
			got := make([]int, len(calls))
			for i, call := range calls {
				got[i] = call.Line
			}
			if !reflect.DeepEqual(got, tt.lines) {
				t.Errorf("Find lines = %v, want %v", got, tt.lines)
			}
		})
	}
}

func TestSSHRemoteStdinShellsThroughCallsAndCompoundSyntax(t *testing.T) {
	body := "echo start\nkill 1\nEOF"
	for _, tt := range []struct {
		name, command string
		via           []string
	}{
		{"env wrapper with ansi shell word", "env ssh host $'bash\\x20-s'", []string{"env", "ssh", "bash"}},
		{"sudo wrapper", "sudo ssh -T host bash", []string{"sudo", "ssh", "bash"}},
		{"timeout wrapper", "timeout 5 ssh host 'bash -s'", []string{"timeout", "ssh", "bash"}},
		{"command wrapper", "command ssh host 'true && bash -s'", []string{"command", "ssh", "bash"}},
		{"and list", "ssh host 'true && bash -s'", []string{"ssh", "bash"}},
		{"or list", "ssh host 'false || bash -s'", []string{"ssh", "bash"}},
		{"if clause", "ssh host 'if true; then bash -s; fi'", []string{"ssh", "bash"}},
		{"first pipeline stage", "ssh host 'bash -s | cat'", []string{"ssh", "bash"}},
		{"group", "ssh host '{ true; bash -s; }'", []string{"ssh", "bash"}},
		{"while body", "ssh host 'while true; do bash -s; break; done'", []string{"ssh", "bash"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := tt.command + " <<'EOF'\n" + body
			calls, err := Find(src, "kill")
			if err != nil || len(calls) != 1 {
				t.Fatalf("Find = %#v, %v; want one kill", calls, err)
			}
			if !reflect.DeepEqual(calls[0].Via, tt.via) || calls[0].Line != 3 {
				t.Errorf("kill = %#v; want Via %v and line 3", calls[0], tt.via)
			}
		})
	}
}

func TestSSHRemotePipelineReaderGetsPipeNotHeredoc(t *testing.T) {
	src := "ssh host 'cat | bash -s' <<'EOF'\necho start\nkill 1\nEOF"
	calls, err := Find(src, "kill")
	if err != nil || len(calls) != 0 {
		t.Fatalf("Find = %#v, %v; later pipeline stage reads the pipe, not SSH stdin", calls, err)
	}
}

func TestSSHStdinSourceAndExpandedRemoteBody(t *testing.T) {
	for _, tt := range []struct {
		name, src string
		wantVia   []string
		line      int
	}{
		{"escaped expansion in double quotes", `ssh host bash <<EOF
echo "\$(kill 1)"
EOF`, []string{"ssh", "bash"}, 2},
		{"escaped expansion unquoted", `ssh host bash <<EOF
echo \$(kill 1)
EOF`, []string{"ssh", "bash"}, 2},
		{"here string", "ssh host bash <<< 'kill 1'", []string{"ssh", "bash"}, 1},
		{"stdin null disabled", `ssh -o StdinNull=no host bash <<'EOF'
kill 1
EOF`, []string{"ssh", "bash"}, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls, err := Find(tt.src, "kill")
			if err != nil || len(calls) != 1 {
				t.Fatalf("Find = %#v, %v; want one call", calls, err)
			}
			if !reflect.DeepEqual(calls[0].Via, tt.wantVia) || calls[0].Line != tt.line {
				t.Errorf("call = %#v; want Via %v, line %d", calls[0], tt.wantVia, tt.line)
			}
		})
	}
}

func TestSSHFunctionCallRedirectControlsFunctionStdin(t *testing.T) {
	for _, tt := range []struct {
		name string
		src  string
		want bool
	}{
		{"redirected call gets data", "ssh host 'f() { bash -s; }; f </dev/null' <<'EOF'\nkill 1\nEOF", false},
		{"plain call gets ssh stdin", "ssh host 'f() { bash -s; }; f' <<'EOF'\nkill 1\nEOF", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls, err := Find(tt.src, "kill")
			if err != nil {
				t.Fatalf("Find error: %v", err)
			}
			if tt.want {
				if len(calls) != 1 || !reflect.DeepEqual(calls[0].Via, []string{"ssh", "bash"}) || calls[0].Line != 2 {
					t.Errorf("Find = %#v; want Via [ssh bash], line 2", calls)
				}
			} else if len(calls) != 0 {
				t.Errorf("Find = %#v; redirected function stdin should be data", calls)
			}
		})
	}
}

func TestSSHStdinDataSourcesDoNotBecomeRemoteScripts(t *testing.T) {
	for _, src := range []string{
		"ssh host bash 3<<'EOF'\nkill 1\nEOF",
		"ssh host bash <<'EOF' </dev/null\nkill 1\nEOF",
		"ssh host '{ bash -s; } </dev/null' <<'EOF'\nkill 1\nEOF",
		"ssh host '( bash -s ) </dev/null' <<'EOF'\nkill 1\nEOF",
		"xargs ssh host bash <<'EOF'\nkill 1\nEOF",
	} {
		calls, err := Find(src, "kill")
		if err != nil || len(calls) != 0 {
			t.Errorf("Find(%q) = %#v, %v; want body treated as data", src, calls, err)
		}
	}
}

func TestSSHUnquotedHeredocSplitsLocalAndRemoteSubstitutions(t *testing.T) {
	src := "ssh host bash <<EOF\necho $(kill 1)\nkill 1\nEOF"
	calls, err := Find(src, "kill")
	if err != nil || len(calls) != 2 {
		t.Fatalf("Find = %#v, %v; want local substitution and remote script calls", calls, err)
	}
	var local, remote *Call
	for i := range calls {
		if len(calls[i].Via) == 0 {
			local = &calls[i]
		} else {
			remote = &calls[i]
		}
	}
	if local == nil || local.Line != 2 {
		t.Errorf("local substitution = %#v; want Via nil, line 2", local)
	}
	if remote == nil || !reflect.DeepEqual(remote.Via, []string{"ssh", "bash"}) || remote.Line != 3 {
		t.Errorf("remote script = %#v; want Via [ssh bash], line 3", remote)
	}
}

func TestRoundSixLocalHeredocUsesExpandedReaderText(t *testing.T) {
	for _, src := range []string{
		"bash <<EOF\necho \\$(kill 1)\nEOF",
		"bash <<EOF\necho \"\\$(kill 1)\"\nEOF",
		"K=kill; bash <<EOF\n\"$K\" 1\nEOF",
	} {
		calls, err := Find(src, "kill")
		if err != nil || len(calls) != 1 {
			t.Fatalf("Find(%q) = %#v, %v; want one kill", src, calls, err)
		}
		if !reflect.DeepEqual(calls[0].Via, []string{"bash"}) || calls[0].Line != 2 {
			t.Errorf("kill = %#v; want Via [bash], line 2", calls[0])
		}
	}
}

func TestRoundSixExpandedBodyKeepsAssignedVariables(t *testing.T) {
	for _, tt := range []struct {
		src string
		via []string
		raw string
	}{
		{"K=kill; runuser -k <<EOF\n$K 1\nEOF", []string{"runuser", "sh"}, "$K 1"},
		{"K=kill; setpriv --split-string runuser <<EOF\n${K} 1\nEOF", []string{"setpriv", "runuser", "sh"}, "${K} 1"},
	} {
		calls, err := Find(tt.src, "kill")
		if err != nil || len(calls) != 1 {
			t.Errorf("Find(%q) = %#v, %v; want one kill", tt.src, calls, err)
			continue
		}
		if !reflect.DeepEqual(calls[0].Via, tt.via) || calls[0].Line != 2 || calls[0].Raw != tt.raw || calls[0].Offset != -1 {
			t.Errorf("kill = %#v; want Via %v, line 2, Raw %q, and Offset -1", calls[0], tt.via, tt.raw)
		}
	}
}

func TestRoundSixLocalSSHInheritsEffectiveStdin(t *testing.T) {
	for _, src := range []string{
		"{ ssh host bash; } <<'EOF'\nkill 1\nEOF",
		"( ssh host bash ) <<'EOF'\nkill 1\nEOF",
		"f() { ssh host bash; }; f <<'EOF'\nkill 1\nEOF",
		"ssh host bash 3<<'EOF' 0<&3\nkill 1\nEOF",
	} {
		calls, err := Find(src, "kill")
		if err != nil || len(calls) != 1 {
			t.Fatalf("Find(%q) = %#v, %v; want one kill", src, calls, err)
		}
		if !reflect.DeepEqual(calls[0].Via, []string{"ssh", "bash"}) || calls[0].Line != 2 {
			t.Errorf("kill = %#v; want Via [ssh bash], line 2", calls[0])
		}
	}
}

func TestRoundSixXargsConsumesOrReplacesSSHStdin(t *testing.T) {
	for _, src := range []string{
		"ssh host 'xargs bash -s' <<'EOF'\nkill 1\nEOF",
		"xargs -o ssh host bash <<'EOF'\nkill 1\nEOF",
		"xargs --open-tty ssh host bash <<'EOF'\nkill 1\nEOF",
	} {
		calls, err := Find(src, "kill")
		if err != nil || len(calls) != 0 {
			t.Errorf("Find(%q) = %#v, %v; want no kill from data on stdin", src, calls, err)
		}
	}
}

func TestSSHRemoteCommandParsedOnce(t *testing.T) {
	src := "ssh host bash <<'EOF'\nkill 1\nEOF"
	budget := newNestedParseBudget(len(src))
	budget.trackWork = true
	var calls []Call
	if err := parseWithBudget(src, 0, nil, make(map[string]string), &calls, budget); err != nil {
		t.Fatalf("parse = %v", err)
	}
	if got := budget.parsedShells["bash"]; got != 1 {
		t.Fatalf("remote command parsed %d times; want once", got)
	}
}

func TestSSHStdinRedirectionsRespectOrderAndDescriptor(t *testing.T) {
	for _, tt := range []struct {
		name, src string
		line      int
	}{
		{"fd 3 does not replace stdin", "ssh host bash <<'A' 3<<'B'\nkill 1\nA\ndata\nB", 2},
		{"fd 3 duplicated to stdin", "ssh host bash 3<<'A' 0<&3\nkill 1\nA", 2},
		{"dup fd zero retains heredoc", "ssh host bash <<'A' <&0\nkill 1\nA", 2},
		{"last stdin redirect wins", "ssh host bash <<'A' </dev/null <<'B'\nkill 2\nA\nkill 1\nB", 4},
		{"group dup of fd zero", "ssh host '{ bash -s; } <&0' <<'EOF'\nkill 1\nEOF", 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls, err := Find(tt.src, "kill")
			if err != nil {
				t.Fatalf("Find error: %v", err)
			}
			if len(calls) != 1 || !reflect.DeepEqual(calls[0].Via, []string{"ssh", "bash"}) || calls[0].Line != tt.line {
				t.Errorf("Find = %#v; want Via [ssh bash] on line %d", calls, tt.line)
			}
		})
	}
}

func TestSSHConfigOptionsThatDisableRemoteStdin(t *testing.T) {
	for _, src := range []string{
		"ssh -o StdinNull=yes host bash <<'EOF'\nkill 1\nEOF",
		"ssh -oStdinNull=yes host bash <<'EOF'\nkill 1\nEOF",
		"ssh -o 'StdinNull yes' host bash <<'EOF'\nkill 1\nEOF",
		"ssh -o ForkAfterAuthentication=yes host bash <<'EOF'\nkill 1\nEOF",
		"ssh -o SessionType=none host bash <<'EOF'\nkill 1\nEOF",
		"ssh -o SessionType=subsystem host bash <<'EOF'\nkill 1\nEOF",
		"ssh -Tf host bash <<'EOF'\nkill 1\nEOF",
	} {
		calls, err := Find(src, "kill")
		if err != nil || len(calls) != 0 {
			t.Errorf("Find(%q) = %#v, %v; want body treated as data", src, calls, err)
		}
	}
}

func manyHeredocSubstitutionScript(heredocs int) string {
	var b strings.Builder
	b.WriteString("x=$(")
	for i := range heredocs {
		fmt.Fprintf(&b, "cat <<E%d\nbody %d\nE%d\n", i, i, i)
	}
	b.WriteString("kill -1\n)\n")
	return b.String()
}

func TestFindStdinResolutionGrowsLinearlyWithHeredocs(t *testing.T) {
	steps := func(heredocs int) int {
		src := manyHeredocSubstitutionScript(heredocs)
		var calls []Call
		budget := newNestedParseBudget(len(src))
		budget.trackWork = true
		if err := parseWithBudget(src, 0, nil, make(map[string]string), &calls, budget); err != nil {
			t.Fatal(err)
		}
		if kills := callsNamed(calls, "kill"); kills != 1 {
			t.Fatalf("got %d kill calls, want 1", kills)
		}
		return budget.stdinSteps
	}
	small, large := steps(400), steps(1600)
	t.Logf("stdin lookup steps: 400 heredocs=%d 1600 heredocs=%d", small, large)
	if small == 0 || large > small*5 {
		t.Fatalf("4x heredocs took %d stdin lookup steps after %d; want linear growth", large, small)
	}
}

func TestStdinIndexContainingMatchesFullScan(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ranges [][2]int
		nested bool
	}{
		{"nested", [][2]int{{0, 20}, {2, 5}, {6, 15}, {7, 9}, {16, 18}}, true},
		{"same start", [][2]int{{0, 10}, {0, 4}, {5, 8}}, true},
		{"overlap", [][2]int{{0, 10}, {2, 5}, {4, 12}}, false},
		{"start out of order", [][2]int{{5, 9}, {1, 3}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			index := &stdinIndex{nested: true}
			for _, r := range tc.ranges {
				index.addRange(r[0], r[1], nil)
			}
			if index.nested != tc.nested {
				t.Fatalf("nested = %v, want %v", index.nested, tc.nested)
			}
			for offset := -1; offset <= 22; offset++ {
				var want [][2]int
				for _, r := range tc.ranges {
					if r[0] <= offset && offset < r[1] {
						want = append(want, r)
					}
				}
				slices.SortStableFunc(want, func(a, b [2]int) int { return (b[1] - b[0]) - (a[1] - a[0]) })
				var got [][2]int
				for _, stmt := range index.containing(offset) {
					got = append(got, [2]int{stmt.start, stmt.end})
				}
				if !slices.Equal(got, want) {
					t.Fatalf("containing(%d) = %v, want %v", offset, got, want)
				}
			}
		})
	}
}
