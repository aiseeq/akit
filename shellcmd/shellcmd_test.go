package shellcmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

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
		{"xargs option value", `xargs -I{} jq .`, []string{"xargs", "jq"}},
		{"bash command string", `bash -euo pipefail -c 'rg x; echo ok'`, []string{"bash", "rg", "echo"}},
		{"bash command string with variables", `bash -c 'while grep -q "$x"; do sleep 5; done'`, []string{"bash", "grep", "sleep"}},
		{"bash double-quoted command string", `timeout 15 bash -c "while kill -0 $cp 2>/dev/null; do sleep 1; done"`, []string{"timeout", "bash", "kill", "sleep"}},
		{"ssh remote command", `ssh -o StrictHostKeyChecking=no -p 22 host 'timeout 35m bash -c "while true; do sleep 20; done"'`, []string{"ssh", "timeout", "bash", "true", "sleep"}},
		{"ssh remote command with parameter", `ssh host 'timeout 35m bash -c "while kill -0 $child 2>/dev/null; do sleep 20; done"'`, []string{"ssh", "timeout", "bash", "kill", "sleep"}},
		{"ssh without command", `ssh -p 22 host`, []string{"ssh"}},
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
		{"command lookup does not run target", "command -v wfor; command -V rg; command -p -v sed", []string{"command", "command", "command"}},
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
	if err != nil || len(calls) != 1 || calls[0].Offset != -1 {
		t.Errorf("вызов подстановки в скрипте heredoc = %#v, %v", calls, err)
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
			if call.Name == "sleep" && call.Offset != -1 {
				t.Errorf("смещение вложенного вызова = %d, want -1", call.Offset)
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
		{"bash -c", "bash -c 'pgrep x'", "pgrep", []int{-1}},
		{"shell heredoc", "bash <<'EOF'\npgrep x\nEOF", "pgrep", []int{-1}},
		{"variable argv0", "P=/usr/bin/pgrep; $P x", "pgrep", []int{-1}},
		{"substitution", "echo $(pgrep x)", "pgrep", []int{7}},
		{"substitution in heredoc", "cat <<EOF\n$(pgrep x)\nEOF", "pgrep", []int{12}},
		{"substitution in shell heredoc script", "bash <<'EOF'\necho $(pgrep x)\nEOF", "pgrep", []int{-1}},
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
	if r := nested[0].Redirs; len(r) != 2 || r[1].Body != "x\n" || r[1].BodyStart != -1 || r[1].BodyEnd != -1 {
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
		for _, r := range c.Redirs {
			if r.BodyStart >= 0 && cmd[r.BodyStart:r.BodyEnd] != r.Body {
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
