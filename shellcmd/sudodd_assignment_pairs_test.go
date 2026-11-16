package shellcmd

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

var sudoDashDashReviewForms = []string{
	// brief p.3
	`nice sudo -- X=1 kill -9 1`,
	`timeout 5 sudo -- X=1 kill -9 1`,
	`env sudo -- X=1 kill -9 1`,
	`exec sudo -- X=1 kill -9 1`,
	`command sudo -- X=1 kill -9 1`,
	`nohup sudo -- X=1 kill -9 1`,
	`setsid sudo -- X=1 kill -9 1`,
	`bash -c 'timeout 5 sudo -- X=1 kill -9 1'`,
	`sudo -- X=1 bash <<< 'kill -9 1'`,
	`sudo -- X=1 bash 0<<< 'kill -9 1'`,
	`sudo -- X=1 sh -s <<< 'kill -9 1'`,
	`echo 'kill -9 1' | sudo -- X=1 bash`,
	`echo 'kill -9 1' | sudo -u root -- X=1 bash`,
	"sudo -- X=1 bash <<'EOF'\nkill -9 1\nEOF",
	`cat <(echo kill -9 1) | sudo -- X=1 sh`,
	`K=kill; sudo -- X=1 $K -9 1`,
	`a=(kill); sudo -- X=1 "${a[@]}" -9 1`,
	// neighbours: wrappers with options, chains, -E, two assignments
	`nice -n 5 sudo -- X=1 kill 1`,
	`nice -n5 sudo -E -- A=1 B=2 kill 1`,
	`timeout -s TERM 5 sudo -- X=1 kill 1`,
	`timeout -k 1 5 sudo -E -- A=1 B=2 kill 1`,
	`env A=1 sudo -- X=1 kill 1`,
	`env -i sudo -- X=1 kill 1`,
	`env -u A sudo -E -- X=1 Y=2 kill 1`,
	`exec -a x sudo -- X=1 kill 1`,
	`command -p sudo -- X=1 kill 1`,
	`nohup sudo -E -- A=1 B=2 kill 1`,
	`setsid -f sudo -- X=1 kill 1`,
	`setsid --fork sudo -- X=1 kill 1`,
	`setsid -w sudo -E -- A=1 B=2 kill 1`,
	`setsid -- sudo -- X=1 kill 1`,
	`setsid sudo -E -- A=1 B=2 kill 1`,
	`setsid sudo -u root -- X=1 kill 1`,
	`setsid nice sudo -- X=1 kill 1`,
	`nice setsid sudo -- X=1 kill 1`,
	`setsid setsid sudo -- X=1 kill 1`,
	`setsid sudo sudo -- X=1 kill 1`,
	`setsid /usr/bin/sudo -- X=1 kill 1`,
	`/usr/bin/setsid sudo -- X=1 kill 1`,
	`nice timeout 5 sudo -- X=1 kill 1`,
	`stdbuf -o0 sudo -- X=1 kill 1`,
	`ionice -c3 sudo -- X=1 kill 1`,
	`flock /tmp/l sudo -- X=1 kill 1`,
	`flock -n /tmp/l sudo -E -- A=1 B=2 kill 1`,
	`taskset 1 sudo -- X=1 kill 1`,
	`chrt 1 sudo -- X=1 kill 1`,
	`time sudo -- X=1 kill 1`,
	`xargs sudo -- X=1 kill`,
	`sh -c 'sudo -- X=1 kill 1'`,
	`sh -c 'nice sudo -E -- A=1 B=2 kill 1'`,
	`bash -c 'setsid sudo -- X=1 kill 1'`,
	`eval 'sudo -- X=1 kill 1'`,
	`echo $(sudo -- X=1 kill 1)`,
	`sudo sudo -- X=1 kill 1`,
	`sudo -- X=1 sudo -- Y=2 kill 1`,
	`/usr/bin/sudo -- X=1 kill 1`,
	// neighbours: shell from input
	`sudo -E -- X=1 bash <<< 'kill 1'`,
	`sudo -- X=1 Y=2 bash <<< 'kill 1'`,
	`sudo -E -- X=1 Y=2 sh -s <<< 'kill 1'`,
	`sudo -- X=1 sh -c 'kill 1'`,
	`sudo -E -- X=1 Y=2 sh -c 'kill 1'`,
	"sudo -- X=1 sh <<-EOF\n\tkill 1\n\tEOF",
	"sudo -E -- A=1 B=2 bash <<-'EOF'\n\tkill 1\n\tEOF",
	"sudo -- X=1 bash -s <<EOF\nkill 1\nEOF",
	`sudo -- X=1 dash <<< 'kill 1'`,
	`sudo -- X=1 zsh <<< 'kill 1'`,
	`sudo -- X=1 /bin/bash <<< 'kill 1'`,
	`sudo -- X=1 bash -s -- a <<< 'kill 1'`,
	`sudo -- X=1 bash -x <<< 'kill 1'`,
	`sudo -- X=1 bash -l <<< 'kill 1'`,
	`sudo -- X=1 bash < <(echo kill 1)`,
	`sudo -- X=1 sh <(echo kill 1)`,
	`printf 'kill 1' | sudo -E -- X=1 Y=2 sh -s`,
	`echo 'kill 1' | sudo -E -- X=1 bash`,
	`echo 'kill 1' | sudo -- X=1 Y=2 bash`,
	`echo 'kill 1' | sudo -- X=1 sh`,
	`echo 'kill 1' | sudo -- X=1 bash -s`,
	`echo 'kill 1' | sudo -- X=1 /bin/sh`,
	`echo 'kill 1' | nice sudo -- X=1 bash`,
	`echo 'kill 1' | setsid sudo -- X=1 bash`,
	`echo 'kill 1' | timeout 5 sudo -- X=1 bash`,
	`echo 'kill 1' | env sudo -- X=1 bash`,
	`echo 'kill 1' | sudo -- X=1 env bash`,
	`echo 'kill 1' | sudo -- X=1 nice bash`,
	`echo 'kill 1' | sudo -- X=1 sudo bash`,
	`echo 'kill 1' | sudo -- X=1 sudo -- Y=2 bash`,
	`nice sudo -- X=1 bash <<< 'kill 1'`,
	`setsid sudo -- X=1 bash <<< 'kill 1'`,
	`timeout 5 sudo -E -- A=1 B=2 sh -s <<< 'kill 1'`,
	`sudo -- X=1 env bash <<< 'kill 1'`,
	`sudo -- X=1 nice bash <<< 'kill 1'`,
	"nice sudo -- X=1 bash <<'EOF'\nkill 1\nEOF",
	`bash -c "echo 'kill 1' | sudo -- X=1 bash"`,
	`sh -c 'sudo -- X=1 sh <<< "kill 1"'`,
	`cat <(echo kill 1) | sudo -- X=1 bash`,
	`cat <(echo kill 1) | sudo -E -- A=1 B=2 sh`,
	`cat <(echo kill 1) | nice sudo -- X=1 sh`,
	`cat <(echo kill 1) | cat | sudo -- X=1 sh`,
	`{ cat <(echo kill 1); } | sudo -- X=1 sh`,
	`cat < <(echo kill 1) | sudo -- X=1 sh`,
	`cat <(echo kill 1) | sudo X=1 sh`,
	`cat <(echo kill 1) | sudo sh`,
	`cat <(echo kill 1) | sudo -- sh`,
	`cat <(echo kill 1) | bash`,
	// variable / array names
	`K=kill; sudo -E -- X=1 Y=2 $K 1`,
	`K=kill; nice sudo -- X=1 $K 1`,
	`K=kill; setsid sudo -- X=1 $K 1`,
	`a=(kill 1); sudo -- X=1 "${a[@]}"`,
	`a=(kill); sudo -E -- A=1 B=2 "${a[@]}" 1`,
	`K=bash; sudo -- X=1 $K <<< 'kill 1'`,
	`K=kill; sudo -- X=1 "$K" 1`,
	`K=kill; sudo -- X=1 ${K} 1`,
	// flock and setsid after merge
	`flock /tmp/l kill 1`,
	`flock -n /tmp/l kill 1`,
	`flock --nonblock /tmp/l kill 1`,
	`flock --nb /tmp/l kill 1`,
	`flock --non /tmp/l kill 1`,
	`flock -- /tmp/l kill 1`,
	`flock -w 5 /tmp/l kill 1`,
	`flock --timeout=5 /tmp/l kill 1`,
	`flock /tmp/l -c 'kill 1'`,
	`flock -o /tmp/l sudo kill 1`,
	`flock /tmp/l sudo X=1 kill 1`,
	`setsid kill 1`,
	`setsid -f kill 1`,
	`setsid --fork kill 1`,
	`setsid --fo kill 1`,
	`setsid -w kill 1`,
	`setsid --wait kill 1`,
	`setsid -c kill 1`,
	`setsid -- kill 1`,
	`setsid sudo kill 1`,
	`setsid sudo X=1 kill 1`,
	`setsid sudo -u root kill 1`,
	`setsid sudo -E X=1 Y=2 kill 1`,
	`setsid sudo bash <<< 'kill 1'`,
	`echo 'kill 1' | setsid sudo bash`,
	`setsid sudo -- kill 1`,
	`setsid sudo -s <<< 'kill 1'`,
	`setsid -f sudo kill 1`,
	`setsid env X=1 kill 1`,
	`setsid nice kill 1`,
	`setsid sudo`,
	`setsid sudo -v`,
	`setsid sudo -- `,
	`setsid sudo -u`,
	`setsid sudo -- X=1`,
	`setsid sudo X=1`,
	// r3 LOST: value-taking sudo options and permissive wrapper options.
	`sudo -R /x -- X=1 kill 1`,
	`sudo --chroot /x -- X=1 kill 1`,
	`nice sudo -R /x -- X=1 kill 1`,
	`echo 'kill 1' | sudo -R /x -- X=1 bash`,
	`sudo --unknown -- X=1 kill 1`,
	`sudo -Z -- X=1 kill 1`,
	`exec -c sudo -- X=1 kill 1`,
	`exec -l sudo -- X=1 kill 1`,
	`exec -cl sudo -- X=1 kill 1`,
	`exec -c -a x sudo -- X=1 kill 1`,
	`echo 'kill 1' | exec -c sudo -- X=1 bash`,
	"exec -c sudo -- X=1 bash <<'EOF'\nkill 1\nEOF",
	// Assignment syntax and wrapper options around sudo.
	`sudo -- lower=1 kill 1`,
	`sudo -- LONG_NAME=1 kill 1`,
	`sudo -- X="a b" kill 1`,
	`exec -a x sudo -R /x -- lower=1 kill 1`,
	`timeout -s TERM 5 env -u OLD nice -n 2 sudo --chroot /x -- LONG_NAME=1 kill 1`,
	`nice -n 3 exec -c -a x sudo -Z -- X="a b" kill 1`,
	// These were false pairs in the reviewer's broad regex comparison.
	`sudo -- X=1 -- kill 1`,
	`sudo -- X=1 sh -c 'nice sudo -- Y=2 kill 1'`,
	`sudo -- X=1 bash -c "echo 'kill 1' | sudo -- Y=2 sh"`,
	`sudo -s -- X=1 <<< 'kill 1'`,
	`echo 'kill 1' | sudo -s -- X=1`,
}

type sudoDashDashPair struct {
	withDash, withoutDash string
	assignments           []string
}

func TestSudoDashDashPairsPreserveCalls(t *testing.T) {
	var pairs []sudoDashDashPair
	seen := make(map[string]bool)
	for _, withDash := range sudoDashDashReviewForms {
		withoutDash, assignments, ok := pairWithoutSudoDash(withDash)
		if !ok || seen[withDash] {
			continue
		}
		pairs = append(pairs, sudoDashDashPair{
			withDash: withDash, withoutDash: withoutDash,
			assignments: slices.Compact(assignments),
		})
		seen[withDash] = true
	}
	if len(pairs) == 0 {
		t.Fatal("no sudo -- VAR=value review forms")
	}
	for i, pair := range pairs {
		t.Run(fmt.Sprintf("pair-%03d", i), func(t *testing.T) {
			withCalls, withErr := Calls(pair.withDash)
			withoutCalls, withoutErr := Calls(pair.withoutDash)
			if withoutErr != nil {
				if withErr == nil {
					t.Fatalf("without -- errors but with -- succeeds: %q: %+v", pair.withDash, withCalls)
				}
				return
			}
			if withErr != nil {
				t.Fatalf("with -- errors but without -- succeeds: %q: %v", pair.withDash, withErr)
			}
			ignored := sudoOptionValueOffsets(pair.withoutDash)
			if missing := missingSudoRouteCallShapes(withoutCalls, withCalls, ignored); len(missing) > 0 {
				t.Fatalf("with -- lost calls from %q: missing %v; got %+v", pair.withoutDash, missing, withCalls)
			}
			for _, assignment := range pair.assignments {
				found := false
				for _, call := range withCalls {
					if call.Name == assignment && slices.Contains(call.Via, "sudo") {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("with -- lacks sudo call for assignment %q: %+v", assignment, withCalls)
				}
			}
		})
	}
}

type sourceEdit struct {
	start, end int
	text       string
}

func pairWithoutSudoDash(src string) (string, []string, bool) {
	without, assignments, changed := rewriteSudoAssignmentMarkers(src)
	return without, assignments, changed
}

func rewriteSudoAssignmentMarkers(src string) (string, []string, bool) {
	var edits []sourceEdit
	var assignments []string
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		return src, nil, false
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		values, _ := words(call.Args, src, nil)
		for commandIndex, value := range values {
			if baseName(commandName(value)) != "sudo" {
				continue
			}
			args := values[commandIndex+1:]
			for i := 0; i < len(args); {
				if args[i] == "--" {
					start, end := i+1, i+1
					for end < len(args) && sudoEnvWord("sudo", args[end]) {
						end++
					}
					if end > start && end < len(args) && args[end] != "--" {
						assignments = append(assignments, args[start])
						word := call.Args[commandIndex+1+i]
						edits = append(edits, sourceEdit{start: int(word.Pos().Offset()), end: int(word.End().Offset())})
					}
					break
				}
				next := optionNext("sudo", args, i)
				if next <= i {
					i++
				} else {
					i = next
				}
			}
		}
		name := baseName(commandName(values[0]))
		if isShell(name) {
			operand, command, _ := shellArgs(name, values[1:])
			wordIndex := operand + 1
			if command && wordIndex < len(call.Args) && wordIndex < len(values) {
				nested, nestedAssignments, changed := rewriteSudoAssignmentMarkers(values[wordIndex])
				if changed {
					assignments = append(assignments, nestedAssignments...)
					word := call.Args[wordIndex]
					edits = append(edits, sourceEdit{
						start: int(word.Pos().Offset()), end: int(word.End().Offset()),
						text: shellSingleQuote(nested),
					})
				}
			}
		}
		return true
	})
	if len(edits) == 0 {
		return src, assignments, false
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	unique := edits[:0]
	for _, edit := range edits {
		if len(unique) == 0 || unique[len(unique)-1].start != edit.start {
			unique = append(unique, edit)
		}
	}
	without := src
	for _, edit := range unique {
		without = without[:edit.start] + edit.text + without[edit.end:]
	}
	return without, assignments, true
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func TestSudoDashDashDoesNotMisclassifyPriorProcessSubstitution(t *testing.T) {
	for _, pair := range []sudoDashDashPair{
		{
			withDash:    "diff <(ls) <(ls)\necho 'kill 1' | sudo -- X=1 bash",
			withoutDash: "diff <(ls) <(ls)\necho 'kill 1' | sudo X=1 bash",
			assignments: []string{"X=1"},
		},
		{
			withDash:    "diff <(ls) <(ls); echo 'kill 1' | sudo bash",
			withoutDash: "diff <(ls) <(ls); echo 'kill 1' | sudo bash",
		},
	} {
		withCalls, withErr := Calls(pair.withDash)
		withoutCalls, withoutErr := Calls(pair.withoutDash)
		if withErr != nil || withoutErr != nil {
			t.Fatalf("parse errors: with -- %v, without -- %v", withErr, withoutErr)
		}
		if missing := missingSudoRouteCallShapes(withoutCalls, withCalls, sudoOptionValueOffsets(pair.withoutDash)); len(missing) > 0 {
			t.Fatalf("with -- lost calls from %q: missing %v; got %+v", pair.withoutDash, missing, withCalls)
		}
		for _, assignment := range pair.assignments {
			if !slices.ContainsFunc(withCalls, func(call Call) bool {
				return call.Name == assignment && slices.Contains(call.Via, "sudo")
			}) {
				t.Errorf("with -- lacks sudo call for %q: %+v", assignment, withCalls)
			}
		}
	}
}

func TestSudoDashDashPairBuilderSkipsRegexArtifacts(t *testing.T) {
	for _, form := range []string{
		`sudo -s -- X=1 <<< 'kill 1'`,
		`echo 'kill 1' | sudo -s -- X=1`,
		`sudo -- X=1 -- kill 1`,
	} {
		if _, assignments, ok := pairWithoutSudoDash(form); ok {
			t.Errorf("paired sudo -s stdin mode without an explicit command: %q (%v)", form, assignments)
		}
	}
}

func TestSudoDashDashAssignmentsWithoutProgram(t *testing.T) {
	for _, src := range []string{
		`setsid sudo -- X=1`,
		`sudo -- X=1`,
		`sudo -E -- A=1 B=2`,
	} {
		t.Run(src, func(t *testing.T) {
			calls, err := Calls(src)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(calls, func(call Call) bool {
				return call.Name == "X=1" || call.Name == "A=1" || call.Name == "B=2"
			}) {
				t.Fatalf("calls %+v, want the sudo assignment call", calls)
			}
		})
	}
}

func TestSudoDashDashAssignmentCallOffset(t *testing.T) {
	const src = `nice sudo -- X=1 kill 1`
	calls, err := Calls(src)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Index(src, "X=1")
	for _, call := range calls {
		if call.Name == "X=1" && slices.Contains(call.Via, "sudo") {
			if call.Offset != want {
				t.Fatalf("assignment Offset = %d, want %d: %+v", call.Offset, want, call)
			}
			if !slices.Equal(call.Args, []string{"kill", "1"}) {
				t.Fatalf("assignment Args = %q, want [kill 1]", call.Args)
			}
			return
		}
	}
	t.Fatalf("calls %+v, want X=1 via sudo", calls)
}

func TestSudoDashDashDoesNotResumeAfterNonOption(t *testing.T) {
	if _, _, ok := sudoDashDashAssignmentBounds("sudo", []string{"kill", "--", "Y=2", "kill", "1"}); ok {
		t.Fatal("assignment scanner resumed after a non-option word")
	}
	start, end, ok := sudoDashDashAssignmentBounds("sudo", []string{"X=0", "--", "Y=2", "kill", "1"})
	if !ok || start != 2 || end != 3 {
		t.Fatalf("bounds across prior environment assignment = (%d, %d, %v), want (2, 3, true)", start, end, ok)
	}
}

func TestEmitMainTargetPreservesProgramDashDash(t *testing.T) {
	for _, tc := range []struct {
		src  string
		name string
		args []string
	}{
		{`exec -c sudo rm -- -rf y`, "rm", []string{"--", "-rf", "y"}},
		{`exec -c sudo git checkout -- f`, "git", []string{"checkout", "--", "f"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, err := Calls(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			var matches []Call
			for _, call := range calls {
				if call.Name == tc.name && slices.Contains(call.Via, "sudo") {
					matches = append(matches, call)
				}
			}
			if len(matches) != 1 || !slices.Equal(matches[0].Args, tc.args) {
				t.Fatalf("program calls = %+v; want one %s %q", matches, tc.name, tc.args)
			}
		})
	}
}

func missingSudoRouteCallShapes(want, got []Call, ignoredOffsets map[int]bool) []string {
	counts := make(map[string]int, len(got))
	for _, call := range got {
		if !pairRouteCall(call) || ignoredOffsets[call.Offset] {
			continue
		}
		counts[callShapeKey(call)]++
	}
	var missing []string
	for _, call := range want {
		if !pairRouteCall(call) || ignoredOffsets[call.Offset] {
			continue
		}
		key := callShapeKey(call)
		if counts[key] == 0 {
			missing = append(missing, key)
			continue
		}
		counts[key]--
	}
	return missing
}

func sudoOptionValueOffsets(src string) map[int]bool {
	offsets := make(map[int]bool)
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		return offsets
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		values, _ := words(call.Args, src, nil)
		for commandIndex, value := range values {
			if baseName(commandName(value)) != "sudo" {
				continue
			}
			args := values[commandIndex+1:]
			for i := 0; i < len(args); {
				if args[i] == "--" || !strings.HasPrefix(args[i], "-") {
					break
				}
				next := optionNext("sudo", args, i)
				if next > i+1 {
					argIndex := commandIndex + 1 + next - 1
					if argIndex < len(call.Args) {
						offsets[int(call.Args[argIndex].Pos().Offset())] = true
					}
				}
				if next <= i {
					i++
				} else {
					i = next
				}
			}
		}
		return true
	})
	return offsets
}

// Calls whose raw source contains sudo but whose route has not reached sudo
// are the enclosing syntax being compared; their argv necessarily includes
// the spelling difference under test.
func pairRouteCall(call Call) bool {
	return slices.Contains(call.Via, "sudo") || !strings.Contains(call.Raw, "sudo")
}

func callShapeKey(call Call) string {
	return fmt.Sprintf("%q/%q/ScriptInput=%t", call.Name, call.Args, call.ScriptInput)
}
