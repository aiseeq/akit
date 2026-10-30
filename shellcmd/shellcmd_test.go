package shellcmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
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
		{"wrapper options", `timeout 5 env -u X sudo -n rg x`, []string{"timeout", "rg"}},
		{"xargs option value", `xargs -I{} jq .`, []string{"xargs", "jq"}},
		{"bash command string", `bash -euo pipefail -c 'rg x; echo ok'`, []string{"bash", "rg", "echo"}},
		{"invalid nested script", `bash -c 'echo "unterminated'; echo after`, nil},
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Calls(tt.command)
			if tt.want == nil {
				if err == nil {
					t.Fatal("Calls() не вернул ошибку разбора")
				}
				var nestedErr *nestedParseError
				if !errors.As(err, &nestedErr) {
					t.Fatalf("ошибка вложенного разбора не обёрнута: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
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
	found, err := Find("echo x; rg y", "rg")
	if err != nil || len(found) != 1 || found[0].Name != "rg" {
		t.Fatalf("Find = %#v, %v", found, err)
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
	var total, parsed, failures, bashRejected, nestedRejected int
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
		_, parseErr := Calls(row.Command)
		if parseErr == nil {
			parsed++
			continue
		}
		failures++
		var nestedErr *nestedParseError
		if errors.As(parseErr, &nestedErr) {
			if err := exec.Command("bash", "-n", "-c", nestedErr.script).Run(); err == nil {
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
		t.Errorf("строка %d: shellcmd не разобрал команду, принятую bash -n: %v", total, parseErr)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("корпус: всего %d, разобрано %d, ошибок %d (bash -n тоже не принял %d; вложенный скрипт не разобран: %d), время %s", total, parsed, failures, bashRejected, nestedRejected, time.Since(started).Round(time.Millisecond))
}
