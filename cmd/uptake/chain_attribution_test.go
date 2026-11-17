package main

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/akit/transcript"
	"mvdan.cc/sh/v3/syntax"
)

func TestChainAttributionWrappers(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	for _, tc := range []struct{ command, tool, wrapper string }{
		{"timeout 90 tl x", "tl", "timeout"},
		{"graft exec --heavy -- tl x", "tl", "graft"},
	} {
		calls, err := shellcmd.Find(tc.command, tc.tool)
		if err != nil || len(calls) != 1 || !slices.Contains(calls[0].Via, tc.wrapper) {
			t.Fatalf("Find(%q) = %#v, %v; expected Via %q", tc.command, calls, err, tc.wrapper)
		}
	}
	for _, cmd := range []string{
		"timeout 90 tl x",
		"graft exec --heavy -- tl x",
		"cd /a && sub A B f",
	} {
		t.Run(cmd, func(t *testing.T) {
			e := transcript.Event{Text: "Process exited with code 1\nOutput:\ntl: invalid input\n"}
			tool := "tl"
			if cmd == "cd /a && sub A B f" {
				tool = "sub"
				e.Text = "Process exited with code 1\nOutput:\nsub: nothing changed: expected 1, found 0\n"
			}
			code, err := u.utilityCode(e, tool, cmd, e.Text)
			if err != nil || code != 1 {
				t.Fatalf("utilityCode = %d, %v; want 1", code, err)
			}
			if got := u.toolFault(tool, e.Text, code, cmd, 0); got != "refusal" {
				t.Errorf("toolFault = %q, want refusal", got)
			}
			plain := transcript.Event{Text: "Process exited with code 0\nOutput:\n"}
			if code, err := u.utilityCode(plain, tool, cmd, plain.Text); err != nil || code != 0 {
				t.Errorf("utilityCode on successful chain = %d, %v; want 0", code, err)
			}
		})
	}
	for _, tc := range []struct {
		command string
		code    int
		want    int
	}{
		{"timeout 90 tl x", 1, 1},
		{"graft exec --heavy -- tl x", 1, 1},
		{"timeout 5 tl x", 124, -1},
		{"graft exec --heavy -- tl x", 125, -1},
		{"ssh h tl x", 255, -1},
		{"xargs tl < f", 123, 123},
	} {
		t.Run(tc.command+"/empty", func(t *testing.T) {
			e := transcript.Event{Text: "Process exited with code " + strconv.Itoa(tc.code) + "\nOutput:\n"}
			got, err := u.utilityCode(e, "tl", tc.command, e.Text)
			if err != nil || got != tc.want {
				t.Errorf("utilityCode = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
	if got := u.toolFault("tl", "tl: invalid input\n", 1, "echo 'tl: invalid input' && tl x", 0); got != "" {
		t.Errorf("fault after output-producing step = %q, want none", got)
	}
}

func TestChainAttributionRepeatedCalls(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	for _, tc := range []struct {
		cmd   string
		code  int
		out   string
		wants []int
	}{
		{"tl a; tl b", 0, "tl: invalid ref a\n", []int{-1, 0}},
		{"tl a && tl b", 1, "tl: invalid ref a\n", []int{1, -1}},
		{"tl a && tl b && tl c", 0, "tl: 3 event(s)\ntl: 2 event(s)\ntl: 1 event(s)\n", []int{0, 0, 0}},
		{"tl a && tl b && tl c", 1, "tl: invalid ref b\n", []int{-1, 1, -1}},
		{"tl a; tl b; echo done", 1, "tl: invalid ref a\n", []int{-1, -1}},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			e := transcript.Event{Text: "Process exited with code " + strconv.Itoa(tc.code) + "\nOutput:\n" + tc.out}
			for ordinal, want := range tc.wants {
				got, err := u.utilityCodeAt(e, "tl", tc.cmd, e.Text, ordinal)
				if err != nil || got != want {
					t.Errorf("ordinal %d utilityCode = %d, %v; want %d", ordinal, got, err, want)
				}
			}
			faults := 0
			for ordinal := range tc.wants {
				if u.toolFault("tl", e.Text, tc.wants[ordinal], tc.cmd, ordinal) != "" {
					faults++
				}
			}
			wantFaults := 0
			if strings.Contains(tc.out, "invalid ref") {
				wantFaults = 1
			}
			if faults != wantFaults {
				t.Errorf("fault count = %d, want %d", faults, wantFaults)
			}
		})
	}
}

func TestChainAttributionPipelineUnchanged(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	for _, filter := range []string{"head", "tail", "cut", "grep", "sed", "awk"} {
		cmd := "tl a | " + filter
		e := transcript.Event{Text: "Process exited with code 0\nOutput:\ntl: invalid ref a\n"}
		code, err := u.utilityCode(e, "tl", cmd, e.Text)
		if err != nil || code != -1 {
			t.Fatalf("%s utilityCode = %d, %v; want unknown", filter, code, err)
		}
		if !u.callHasTruncator("tl", cmd, 0) {
			t.Errorf("%s was not recognized as a utility output filter", filter)
		}
		if got := u.toolFault("tl", e.Text, code, cmd, 0); got != "" {
			t.Errorf("%s toolFault = %q, want empty fault", filter, got)
		}
	}
}

func TestChainAttributionSourceAndWrapperBoundaries(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	if got := u.toolFault("tk", "", 128, "tk ls", 0); got != "" {
		t.Errorf("tk high exit fault = %q, want none", got)
	}
	e := transcript.Event{Text: "Process exited with code 1\nOutput:\nsudo: warning\ntl: invalid input\n"}
	if code, err := u.utilityCode(e, "tl", "sudo -u u tl x", e.Text); err != nil || code != 1 {
		t.Errorf("sudo plus utility output code = %d, %v; want 1", code, err)
	}
	if got := u.toolFault("tl", e.Text, 1, "sudo -u u tl x", 0); got != "refusal" {
		t.Errorf("sudo plus utility output fault = %q, want refusal", got)
	}
	sudoFailure := "Process exited with code 1\nOutput:\nsudo: permission denied\n"
	if code, err := u.utilityCode(transcript.Event{Text: sudoFailure}, "tl", "sudo tl x", sudoFailure); err != nil || code != -1 {
		t.Errorf("sudo wrapper failure code = %d, %v; want unknown", code, err)
	}
	if !lineMentionsArgument("sub: failure in /tmp/notes.go:3", "/tmp/notes.go") {
		t.Error("path in explanatory output was not matched to its invocation")
	}
}

func TestChainAttributionR3RepeatedMessagesAndCallErrors(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	for _, tc := range []struct {
		command string
		code    int
		output  string
		codes   []int
		faults  []string
		outcome []string
	}{
		{
			command: "sub 'aaa' 'x' f.go && sub 'bbb' 'y' f.go", code: 1,
			output: "sub: nothing changed: 1 of 1 edits mismatched\n  OLD: expected 1, found 0 in f.go; first line \"aaa\" not found either\n",
			codes:  []int{1, -1}, faults: []string{"refusal", ""}, outcome: []string{"not-found", "unknown"},
		},
		{
			command: "sub a b f.go\nsub c d f.go", code: 0,
			output: "sub: 1 replacement in 1 file: f.go 1 (line 3)\nsub: 1 replacement in 1 file: f.go 1 (line 9)\n",
			codes:  []int{-1, 0}, faults: []string{"", ""}, outcome: []string{"success", "success"},
		},
		{
			command: "sub <<'EOF'\nFILE f.go\nOLD\na\nNEW\nb\nEOF\nsub <<'EOF'\nFILE f.go\nOLD\nc\nNEW\nd\nEOF", code: 0,
			output: "sub: read FILE OLD/NEW form; translated to SEARCH/REPLACE:\nf.go\n<<<<<<< SEARCH\na\n=======\nb\n>>>>>>> REPLACE\nsub: 1 replacement in 1 file: f.go 1 (line 3)\n" +
				"sub: read FILE OLD/NEW form; translated to SEARCH/REPLACE:\nf.go\n<<<<<<< SEARCH\nc\n=======\nd\n>>>>>>> REPLACE\nsub: 1 replacement in 1 file: f.go 1 (line 9)\n",
			codes: []int{-1, 0}, faults: []string{"", ""}, outcome: []string{"success", "success"},
		},
		{
			command: "touch f && sub '' 'x' f && cat f", code: 2,
			output: "sub: OLD is empty\n(sub -h for usage)\n",
			codes:  []int{2}, faults: []string{"call-error"}, outcome: []string{"error"},
		},
		{
			command: "sub A B errors.go", code: 0,
			output: "sub: 1 replacement in 1 file: errors.go 1 (line 3)\n",
			codes:  []int{0}, faults: []string{""}, outcome: []string{"success"},
		},
		{
			command: "sub A B", code: 2, output: "sub: need FILE\n",
			codes: []int{2}, faults: []string{"call-error"}, outcome: []string{"error"},
		},
	} {
		t.Run(tc.command, func(t *testing.T) {
			calls, err := shellcmd.Find(tc.command, "sub")
			if err != nil || len(calls) != len(tc.codes) {
				t.Fatalf("Find(%q) = %#v, %v", tc.command, calls, err)
			}
			for ordinal := range calls {
				got := u.attributeUtility("sub", tc.command, tc.output, tc.code, ordinal)
				if got.code != tc.codes[ordinal] || got.fault != tc.faults[ordinal] || u.outcome("sub", got.output, got.code) != tc.outcome[ordinal] {
					t.Errorf("call %d = code %d, fault %q, outcome %q; want %d, %q, %q", ordinal,
						got.code, got.fault, u.outcome("sub", got.output, got.code), tc.codes[ordinal], tc.faults[ordinal], tc.outcome[ordinal])
				}
				if strings.Contains(tc.command, "FILE f.go") && !strings.Contains(got.output, "translated to SEARCH/REPLACE") {
					t.Errorf("call %d lost the nonindented FILE translation output: %q", ordinal, got.output)
				}
			}
		})
	}
}

func TestChainAttributionR3WrappedScriptsAndLoopFilters(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	for _, tc := range []struct {
		command string
		code    int
		output  string
		want    int
	}{
		{"graft exec --heavy -- bash -c 'tl x; echo done'", 0, "graft: exec: heavy\ntl: invalid input\ndone\n", -1},
		{"timeout 5 bash -c 'tl x; echo done'", 0, "tl: invalid input\ndone\n", -1},
		{"sudo sh -c 'tl x || true; false'", 1, "tl: invalid input\n", -1},
		{"ssh h 'tl x; echo done'", 0, "tl: invalid input\ndone\n", -1},
	} {
		calls, err := shellcmd.Find(tc.command, "tl")
		if err != nil || len(calls) != 1 {
			t.Fatalf("Find(%q) = %#v, %v", tc.command, calls, err)
		}
		got := u.attributeUtility("tl", tc.command, tc.output, tc.code, 0)
		if got.code != tc.want || got.fault != "refusal" {
			t.Errorf("%q: got code=%d fault=%q; want code=%d refusal", tc.command, got.code, got.fault, tc.want)
		}
	}
	graftCommand := "graft exec --heavy -- tl x"
	graftOutput := "graft: exec: heavy, on this machine: X\ntl: invalid input\n"
	graftCall := u.attributeUtility("tl", graftCommand, graftOutput, 1, 0)
	if strings.Contains(graftCall.output, "graft: exec:") || !strings.Contains(graftCall.output, "tl: invalid input") {
		t.Errorf("graft wrapper output was not removed from utility output: %q", graftCall.output)
	}
	command := "for r in a b; do tl $r | cut -c1-10; done"
	got := u.attributeUtility("tl", command, "== a\n== b\n", 0, 0)
	if got.code != -1 || got.fault != "" || !u.callHasTruncator("tl", command, 0) {
		t.Errorf("loop filter = code %d fault %q truncated %v", got.code, got.fault, u.callHasTruncator("tl", command, 0))
	}
}

func TestChainAttributionNoEventsDoesNotNameFailedAndCall(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	command := "touch f && tl a && echo done"
	output := "tl: no events in 3 transcript(s)\n"
	got := u.attributeUtility("tl", command, output, 1, 0)
	if got.code != -1 || got.fault != "" {
		t.Errorf("no-events chain = code %d fault %q; want unknown without a refusal", got.code, got.fault)
	}
}

func TestChainAttributionR4MessagesAndLoops(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	for _, tc := range []struct {
		command string
		code    int
		output  string
		tools   []string
		codes   []int
		faults  []string
		outcome []string
	}{
		{"tl e3c289f6:150", 1, "tl: no event at e3c289f6:150\n", []string{"tl"}, []int{1}, []string{"refusal"}, []string{"unknown"}},
		{"page-shot --element '#days .day' --out d p.html", 1, "page-shot: --element \"#days .day\": на странице нет таких элементов\n", []string{"page-shot"}, []int{1}, []string{"refusal"}, []string{""}},
		{"page-shot --out d --element '.error-banner' p.html", 0, "d/el-1.png\npage-shot: error-banner: элемент нулевого размера, не снят\n", []string{"page-shot"}, []int{0}, []string{""}, []string{""}},
		{"tk block a --by fix-invalid-input", 0, "a blocked by fix-invalid-input\ntk: warning: missing blocking link fix-invalid-input from a\n", []string{"tk"}, []int{0}, []string{""}, []string{""}},
		{"sub -F s; sub -h | head -3", 0, "sub: line 3: \"=======\" inside the REPLACE part of block 1 (line 2)\n(sub -h for usage)\nsub — exact-text replace\n", []string{"sub", "sub"}, []int{-1, -1}, []string{"call-error", ""}, []string{"unknown", "truncated"}},
		{"sub A B f.go && tl -x", 2, "sub: 1 replacement in 1 file: f.go 1 (line 2)\ntl: unknown flag -x\n", []string{"sub", "tl"}, []int{-1, 2}, []string{"", "call-error"}, []string{"success", "error"}},
		{"sub A B f.go | cat", 1, "sub: nothing changed: 1 of 1 edits mismatched\n  OLD: expected 1, found 0 in f.go\n", []string{"sub"}, []int{-1}, []string{"refusal"}, []string{"not-found"}},
		{"for r in a b; do tl $r | cat; done", 0, "tl: invalid ref a\n", []string{"tl"}, []int{-1}, []string{"refusal"}, []string{"unknown"}},
		{"bash -lc 'tl x; echo done'", 0, "tl: invalid input\ndone\n", []string{"tl"}, []int{-1}, []string{"refusal"}, []string{"unknown"}},
		{"bash -lc 'tl x' && echo done", 0, "tl: 3 event(s)\ndone\n", []string{"tl"}, []int{0}, []string{""}, []string{"events"}},
		{"bash -c 'sub A B f.go' && echo done", 0, "sub: 1 replacement in 1 file: f.go 1 (line 2)\ndone\n", []string{"sub"}, []int{0}, []string{""}, []string{"success"}},
		{"bash -c 'sub A B f.go && sub C D g.go'; echo done", 0, "sub: 1 replacement in 1 file: f.go 1 (line 2)\nsub: 1 replacement in 1 file: g.go 1 (line 2)\ndone\n", []string{"sub", "sub"}, []int{-1, -1}, []string{"", ""}, []string{"success", "success"}},
		{"bash -c 'sub A B f.go; sub C D g.go' && echo done", 0, "sub: 1 replacement in 1 file: f.go 1 (line 2)\nsub: 1 replacement in 1 file: g.go 1 (line 2)\ndone\n", []string{"sub", "sub"}, []int{-1, 0}, []string{"", ""}, []string{"success", "success"}},
		{"! bash -c 'tl x'", 0, "tl: invalid ref x\n", []string{"tl"}, []int{-1}, []string{""}, []string{"unknown"}},
		{"bash -c 'tl x' | head -3", 0, "tl: invalid ref x\n", []string{"tl"}, []int{-1}, []string{""}, []string{"truncated"}},
		{"for f in a.go b.go; do sub A B $f; done", 0, "sub: 1 replacement in 1 file: a.go 1 (line 2)\nsub: nothing changed: 1 of 1 edits mismatched\n  OLD: expected 1, found 0 in b.go\n", []string{"sub"}, []int{-1}, []string{"refusal"}, []string{"not-found"}},
	} {
		t.Run(tc.command, func(t *testing.T) {
			calls, err := shellcmd.Find(tc.command, tc.tools...)
			if err != nil || len(calls) != len(tc.codes) {
				t.Fatalf("Find(%q) = %#v, %v", tc.command, calls, err)
			}
			ordinal := map[string]int{}
			for i, call := range calls {
				index := ordinal[call.Name]
				ordinal[call.Name]++
				got := u.attributeUtility(call.Name, tc.command, tc.output, tc.code, index)
				outcome := u.outcome(call.Name, got.output, got.code)
				if outcome == "unknown" && u.callHasTruncator(call.Name, tc.command, index) {
					outcome = "truncated"
				}
				if got.code != tc.codes[i] || got.fault != tc.faults[i] || outcome != tc.outcome[i] {
					t.Errorf("call %d: code=%d fault=%q outcome=%q; want %d, %q, %q", i, got.code, got.fault, outcome, tc.codes[i], tc.faults[i], tc.outcome[i])
				}
			}
		})
	}
}

func TestChainAttributionScriptOffsetChecks(t *testing.T) {
	command := "bash -c 'echo skip; tl x'"
	calls, err := shellcmd.Find(command, "tl")
	if err != nil || len(calls) != 1 {
		t.Fatalf("Find(%q) = %#v, %v", command, calls, err)
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		t.Fatal(err)
	}
	target := calls[0]
	target.Offset = strings.Index(command, "'") + 1
	script, offset, _, found := embeddedScript(command, file, target)
	if !found || offset != strings.Index(script, "tl x") {
		t.Errorf("embeddedScript offset = %d, found=%v; want %d", offset, found, strings.Index(script, "tl x"))
	}
	if got := uniqueScriptCallOffset("tl x; tl x", "tl", []string{"x"}); got != -1 {
		t.Errorf("ambiguous repeated script call offset = %d, want -1", got)
	}
	if got := trimScriptVia([]string{"sudo", "sh", "-c", "tl"}, []string{"bash", "-c"}); got != nil {
		t.Errorf("trimScriptVia accepted a nonmatching wrapper prefix: %v", got)
	}

}

func TestChainAttributionExplicitSuccessWords(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	for _, tc := range []struct{ tool, output string }{
		{"sub", "sub: 1 replacement in 1 file: error.go\n"},
		{"sub", "sub: dry run, would make invalid.go\n"},
		{"tl", "tl: 2 event(s); invalid role was ignored\n"},
		{"wfor", "wfor: done in 1s: not yet after 2s\n"},
	} {
		if u.explicitRefusal(tc.tool, tc.output) {
			t.Errorf("explicitRefusal(%q, %q) = true for a success line", tc.tool, tc.output)
		}
	}
	for _, tc := range []struct{ tool, output string }{
		{"tk", "work noted\n"},
		{"wfor", "wfor: done in 1s: not yet after 2s\n"},
	} {
		u.addError(tc.tool, "test", -1, tc.output, false)
		if stat := u.errorStats[tc.tool+"\ttest"]; stat == nil || stat.unknown != 0 || stat.fail != 0 {
			t.Errorf("success output for %s recorded as error: %#v", tc.tool, stat)
		}
	}
}

func TestChainAttributionR5SingleUtilityAndRedirectOrder(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	for _, tc := range []struct {
		name, command, output  string
		code, wantCode         int
		wantOutcome, wantFault string
	}{
		{"cd then heredoc success", "cd /d && sub <<'EOF'\n=== f.md\nold\n---\nnew\nEOF", "sub: 1 replacement in 1 file: f.md 1 (line 1)\n", 0, 0, "success", ""},
		{"cd then heredoc call error", "mkdir -p d && cd d && sub <<'EOF'\n=== page.html 3\nold\n---\nnew\nEOF", "sub: open page.html 3: lstat page.html 3: no such file or directory; nothing changed\n", 2, 2, "unknown", "call-error"},
		{"cd then heredoc refusal", "cd /d && sub <<'EOF'\n=== f.md\nold\n---\nnew\nEOF", "sub: nothing changed: 1 of 1 edits mismatched\n  block 1 (line 2): expected 1, found 0 in f.md; first line \"old\" not found either\n", 1, 1, "not-found", "refusal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := u.attributeUtility("sub", tc.command, tc.output, tc.code, 0)
			if got.code != tc.wantCode || got.fault != tc.wantFault || u.outcome("sub", got.output, got.code) != tc.wantOutcome {
				t.Errorf("got code=%d outcome=%q fault=%q; want %d %q %q", got.code, u.outcome("sub", got.output, got.code), got.fault, tc.wantCode, tc.wantOutcome, tc.wantFault)
			}
		})
	}
	for _, tc := range []struct {
		command string
		both    bool
	}{
		{"sub A B f.go >/dev/null", false},
		{"sub A B f.go 2>&1 >/dev/null", false},
		{"sub A B f.go >/dev/null 2>&1", true},
		{"sub A B f.go &>/dev/null", true},
	} {
		calls, err := shellcmd.Find(tc.command, "sub")
		if err != nil || len(calls) != 1 {
			t.Fatalf("Find(%q) = %#v, %v", tc.command, calls, err)
		}
		if got := redirectsBothStreamsToNull(calls[0]); got != tc.both {
			t.Errorf("%q redirects both streams to null = %v, want %v (redirs=%+v)", tc.command, got, tc.both, calls[0].Redirs)
		}
		output := "sub: nothing changed: expected 1, found 0 in f.go\n"
		got := u.attributeUtility("sub", tc.command, output, 1, 0)
		wantFault, wantOutcome := "refusal", "not-found"
		if tc.both {
			wantFault, wantOutcome = "", "unknown"
		}
		if got.fault != wantFault || u.outcome("sub", got.output, got.code) != wantOutcome {
			t.Errorf("%q: output=%q fault=%q outcome=%q; want fault=%q outcome=%q", tc.command, got.output, got.fault, u.outcome("sub", got.output, got.code), wantFault, wantOutcome)
		}
	}
}

func TestChainAttributionR5FilteredOutputDoesNotShift(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	command := "cat n.txt | tk note x --text - && tk done x 2>&1 | tail -2"
	output := "x noted\ntk: done requires --check\n(tk -h for usage)\n"
	for ordinal, want := range []struct {
		code  int
		fault string
	}{{0, ""}, {-1, ""}} {
		got := u.attributeUtility("tk", command, output, 0, ordinal)
		if got.code != want.code || got.fault != want.fault {
			t.Errorf("call %d = code %d fault %q; want %d %q (output %q)", ordinal, got.code, got.fault, want.code, want.fault, got.output)
		}
	}
	command = "tl -z x | head -3; tl b --since 1d"
	output = "tl: unknown flag -z (tl -h for usage)\ntl: no events in 1 transcript(s) (since 15.11 00:00)\n"
	for ordinal, want := range []struct {
		code    int
		outcome string
	}{{-1, "unknown"}, {0, "no-events"}} {
		got := u.attributeUtility("tl", command, output, 0, ordinal)
		if got.code != want.code || u.outcome("tl", got.output, got.code) != want.outcome {
			t.Errorf("tl call %d = code %d outcome %q; want %d %q (output %q)", ordinal, got.code, u.outcome("tl", got.output, got.code), want.code, want.outcome, got.output)
		}
		if ordinal == 0 && !u.callHasTruncator("tl", command, ordinal) {
			t.Error("filtered first tl call was not marked truncated")
		}
	}
}

func TestChainAttributionR5StdoutRedirectDoesNotTakeNeighborSuccess(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	command := "sub A B f.go >/dev/null && sub -F s; echo x"
	output := "sub: 1 replacement in 1 file: g.go 1 (line 3)\nx\n"
	for ordinal, want := range []struct {
		code    int
		outcome string
	}{{-1, "unknown"}, {-1, "success"}} {
		got := u.attributeUtility("sub", command, output, 0, ordinal)
		if got.code != want.code || u.outcome("sub", got.output, got.code) != want.outcome {
			t.Errorf("call %d = code %d outcome %q; want %d %q (output %q)", ordinal, got.code, u.outcome("sub", got.output, got.code), want.code, want.outcome, got.output)
		}
	}
}

func TestChainAttributionR5ArgumentQuotesAndMutAdvice(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	if u.explicitRefusalArgs("page-shot", "page-shot: found target error-banner", nil) {
		t.Error("refusal word embedded in a compound token was treated as refusal")
	}
	if u.explicitRefusalArgs("page-shot", "page-shot: found target 'error'", []string{"error"}) {
		t.Error("exactly quoted argument text was treated as refusal")
	}
	if u.explicitRefusalArgs("page-shot", "page-shot: found target error-banner", []string{".error-banner"}) {
		t.Error("refusal word embedded in an argument token was treated as refusal")
	}
	if !u.explicitRefusalArgs("page-shot", "page-shot: found target 'error'", []string{"Error"}) {
		t.Error("case-different text was stripped as an invocation argument")
	}
	if got := utilityFaultArgs(u, "sub", "sub: 'invalid'\n", 2, false, []string{"invalid"}); got != "" {
		t.Errorf("code-2 output that only quotes an argument became fault %q", got)
	}
	if got := utilityFaultArgs(u, "sub", "sub: 1 replacement in 1 file: f.go\n(sub -h for usage)\n", 2, false, []string{"-h"}); got != "" {
		t.Errorf("usage marker beside a success line became fault %q", got)
	}
	wfor := "wfor: not yet after 1m0s; still waiting: line 'not yet' in f.log"
	if !u.explicitRefusalArgs("wfor", wfor, []string{"not yet"}) {
		t.Error("unquoted refusal text was removed along with a quoted argument")
	}
	sub := `sub: nothing changed: OLD "nothing changed" not found`
	if !u.explicitRefusalArgs("sub", sub, []string{"nothing changed"}) {
		t.Error("unquoted refusal text was removed along with the quoted OLD argument")
	}
	mutOutput := "mut: outer graft exec --heavy is unnecessary; mut queues each test run\n"
	if got := u.toolFault("mut", mutOutput, 1, "mut -F spec -- -count=1 ./cmd/uptake", 0); got != "" {
		t.Errorf("mut advice line became fault %q", got)
	}
}

func TestChainAttributionR5OutputBlockBoundaries(t *testing.T) {
	if lineMentionsArgument("tk: done requires --check", "-") {
		t.Error("standalone '-' argument matched a longer option")
	}
	blocks := toolOutputBlocks("sub", []string{
		"sub: nothing changed: expected 1, found 0 in a.go",
		"\tOLD: expected 1, found 0 in a.go",
		"cat output that is not from sub",
		"  cat detail that is not from sub",
		"sub: 1 replacement in 1 file: b.go",
	})
	if len(blocks) != 2 || !strings.Contains(blocks[0], "\tOLD:") || strings.Contains(blocks[0], "cat output") || strings.Contains(blocks[0], "cat detail") || strings.Contains(blocks[1], "cat output") || strings.Contains(blocks[1], "cat detail") {
		t.Fatalf("tool output blocks = %#v; want two separated blocks with tab continuation retained", blocks)
	}
	withDetachedUsage := toolOutputBlocks("sub", []string{"sub: need FILE", "", "(sub -h for usage)"})
	if len(withDetachedUsage) != 1 || !strings.Contains(withDetachedUsage[0], "(sub -h for usage)") {
		t.Fatalf("detached usage marker blocks = %#v; want marker joined to preceding error", withDetachedUsage)
	}
	call := shellcmd.Call{Name: "sh", Args: []string{"-h"}}
	if got := outputBlockCall("sub: error\n(tl -h for usage)\n", []shellcmd.Call{call}); got != -1 {
		t.Errorf("usage marker was matched as an argument: outputBlockCall = %d, want -1", got)
	}
}

func TestChainAttributionR5CatBeforeUtilityOwnsItsOutput(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	command := "cat notes.md; sub A B f"
	output := "sub: nothing changed: x\nsub: 1 replacement in 1 file: f 1 (line 2)\n"
	got := u.attributeUtility("sub", command, output, 0, 0)
	if got.fault != "" || u.outcome("sub", got.output, got.code) != "success" {
		t.Errorf("cat output was attributed to sub: output=%q fault=%q outcome=%q", got.output, got.fault, u.outcome("sub", got.output, got.code))
	}
}

func TestChainAttributionR5ShortShellFlags(t *testing.T) {
	file, err := syntax.NewParser().Parse(strings.NewReader("bash --config -lc 'tl a'"), "")
	if err != nil {
		t.Fatal(err)
	}
	var shellCall *syntax.CallExpr
	syntax.Walk(file, func(node syntax.Node) bool {
		if call, ok := node.(*syntax.CallExpr); ok {
			shellCall = call
			return false
		}
		return true
	})
	if shellCall == nil {
		t.Fatal("shell command was not parsed")
	}
	word, found := shellCommandScriptWord(shellCall.Args, 0)
	if !found || word == nil {
		t.Fatalf("script word = %v, found=%v; want grouped short flag -lc script", word, found)
	}
	script, _, ok := scriptWordContents("bash --config -lc 'tl a'", word)
	if !ok || script != "tl a" {
		t.Errorf("script = %q, valid=%v; want grouped short flag script tl a", script, ok)
	}
}

func TestChainAttributionR6SingleCallRedirectsAndUsageOwnership(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	for _, tc := range []struct {
		command, output, wantFault, wantOutcome string
	}{
		{"wfor file /tmp/x --timeout 1m >/dev/null; cat wfor.log", "wfor: not yet after 1m0s; still waiting: file /tmp/y (absent); to keep waiting run the same wfor again\n", "", "unknown"},
		{"tl x --since 1d 2>/dev/null; cat t.log", "tl: unknown flag -q (tl -h for usage)\n", "", "unknown"},
	} {
		got := u.attributeUtility(strings.Fields(tc.command)[0], tc.command, tc.output, 0, 0)
		if got.fault != tc.wantFault || u.outcome(strings.Fields(tc.command)[0], got.output, got.code) != tc.wantOutcome {
			t.Errorf("%q: output=%q fault=%q outcome=%q; want fault=%q outcome=%q", tc.command, got.output, got.fault, u.outcome(strings.Fields(tc.command)[0], got.output, got.code), tc.wantFault, tc.wantOutcome)
		}
	}

	command := "sub -F s1; sub C D g.go; sub -h | head -3"
	output := "sub: line 3: \"=======\" inside the REPLACE part of block 1 (line 2)\n(sub -h for usage)\nsub: 1 replacement in 1 file: g.go 1 (line 2)\n"
	for ordinal, want := range []struct {
		fault, outcome string
	}{{"call-error", "unknown"}, {"", "success"}, {"", "unknown"}} {
		got := u.attributeUtility("sub", command, output, 0, ordinal)
		if got.fault != want.fault || u.outcome("sub", got.output, got.code) != want.outcome {
			t.Errorf("usage call %d: output=%q fault=%q outcome=%q; want %q %q", ordinal, got.output, got.fault, u.outcome("sub", got.output, got.code), want.fault, want.outcome)
		}
	}

	command = "sub -F s1 2>/dev/null; sub -F s2"
	output = "sub: line 3: \"=======\" inside the REPLACE part of block 1 (line 2)\n(sub -h for usage)\n"
	for ordinal, want := range []string{"", "call-error"} {
		got := u.attributeUtility("sub", command, output, 0, ordinal)
		if got.fault != want {
			t.Errorf("redirected usage call %d fault=%q; want %q (output %q)", ordinal, got.fault, want, got.output)
		}
	}
}

func TestChainAttributionR6VisibleBlockReassignment(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	command := "sub <<'EOF'\nf.go\n<<<<<<< SEARCH\na\n=======\nb\n>>>>>>> REPLACE\nEOF\nsub x y g.go >/dev/null && sub y z g.go; sub m n h.go"
	output := "sub: 1 replacement in 1 file: f.go 1 (line 1)\nsub: 1 replacement in 1 file: g.go 1 (line 2)\n"
	for ordinal, want := range []string{"success", "unknown", "success", "unknown"} {
		got := u.attributeUtility("sub", command, output, 0, ordinal)
		if u.outcome("sub", got.output, got.code) != want {
			t.Errorf("call %d: output=%q code=%d outcome=%q; want %q", ordinal, got.output, got.code, u.outcome("sub", got.output, got.code), want)
		}
	}

	command = "sub -F s1 2>/dev/null; sub C D g.go"
	output = "sub: line 3: \"=======\" inside the REPLACE part of block 1 (line 2)\n(sub -h for usage)\nsub: 1 replacement in 1 file: g.go 1 (line 2)\n"
	for ordinal, want := range []struct {
		fault, outcome string
	}{{"", "unknown"}, {"", "success"}} {
		got := u.attributeUtility("sub", command, output, 0, ordinal)
		if got.fault != want.fault || u.outcome("sub", got.output, got.code) != want.outcome {
			t.Errorf("equal-block call %d output=%q fault=%q outcome=%q; want %q %q", ordinal, got.output, got.fault, u.outcome("sub", got.output, got.code), want.fault, want.outcome)
		}
	}
}

func TestChainAttributionR6ArgumentQuoteFormsAndDryRun(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	for _, quote := range []string{`"invalid"`, "`invalid`"} {
		if u.explicitRefusalArgs("sub", "sub: found target "+quote, []string{"invalid"}) {
			t.Errorf("quoted argument %s was treated as refusal", quote)
		}
	}
	if !u.explicitSuccess("sub", "sub: dry run, would make invalid.go\n") {
		t.Error("sub dry-run success output was not recognized")
	}
}

func TestChainAttributionR4ScorePrefersNamedOldText(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	command := "sub foo bar f.go; sub baz qux f.go; sub q r h.go"
	output := "sub: nothing changed: 1 of 1 edits mismatched\n  OLD: expected 1, found 0 in f.go; first line \"foo\" not found either\n"
	for ordinal, want := range []struct {
		outcome, fault string
	}{{"not-found", "refusal"}, {"unknown", ""}, {"unknown", ""}} {
		got := u.attributeUtility("sub", command, output, 0, ordinal)
		if u.outcome("sub", got.output, got.code) != want.outcome || got.fault != want.fault {
			t.Errorf("call %d output=%q outcome=%q fault=%q; want %q %q", ordinal, got.output, u.outcome("sub", got.output, got.code), got.fault, want.outcome, want.fault)
		}
	}
}

func TestChainAttributionR4EqualArgumentScoresStayUnknown(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	command := "sub foo bar f.go; sub baz qux f.go"
	output := "sub: nothing changed: 1 of 1 edits mismatched in f.go\n"
	for ordinal := range 2 {
		got := u.attributeUtility("sub", command, output, 0, ordinal)
		if got.fault != "" || u.outcome("sub", got.output, got.code) != "unknown" {
			t.Errorf("ambiguous block call %d output=%q fault=%q outcome=%q; want unknown without a fault", ordinal, got.output, got.fault, u.outcome("sub", got.output, got.code))
		}
	}
}

func TestChainAttributionR7CatRedirectIsNotAnOutputSource(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	command := "sub a b f.go >/dev/null && sub <<'EOF'\nFILE g.go\nOLD\nc\nNEW\nd\nEOF\ncat >> t.go <<'EOF'\nx\nEOF"
	output := "sub: read FILE OLD/NEW form; translated to SEARCH/REPLACE:\ng.go\n<<<<<<< SEARCH\nc\n=======\nd\n>>>>>>> REPLACE\nsub: 1 replacement in 1 file: g.go 1 (line 1)\n"
	for ordinal, want := range []string{"unknown", "success"} {
		got := u.attributeUtility("sub", command, output, 0, ordinal)
		if outcome := u.outcome("sub", got.output, got.code); outcome != want {
			t.Errorf("call %d output=%q outcome=%q; want %q", ordinal, got.output, outcome, want)
		}
	}
	for _, command := range []string{"cat", "cat >> t.go", "cat >/dev/null", "grep x f", "grep x f >out", "grep x f >&out", "rg x f", "rg x f &>/dev/null"} {
		calls, err := shellcmd.Calls(command)
		if err != nil || len(calls) != 1 {
			t.Fatalf("Calls(%q) = %#v, %v", command, calls, err)
		}
		want := command == "grep x f" || command == "rg x f"
		if got := isVisibleOutputSource(command, calls[0]); got != want {
			t.Errorf("isVisibleOutputSource(%q)=%v; want %v (redirs=%+v)", command, got, want, calls[0].Redirs)
		}
	}
	for _, command := range []string{"go test ./... 2>&1 | grep -v '^graft' | tail -3"} {
		calls, err := shellcmd.Calls(command)
		if err != nil {
			t.Fatalf("Calls(%q): %v", command, err)
		}
		for _, call := range calls {
			if isVisibleOutputSource(command, call) {
				t.Errorf("pipeline/non-file command %q call %+v was classified as visible output source", command, call)
			}
		}
	}

	got := u.attributeUtility("sub", "sub A B f.go >/dev/null", "sub: 1 replacement in 1 file: f.go\n", 0, 0)
	if got.output != "" || u.outcome("sub", got.output, got.code) != "unknown" {
		t.Errorf("single stdout-hidden call output=%q outcome=%q; want empty unknown", got.output, u.outcome("sub", got.output, got.code))
	}
	got = u.attributeUtility("sub", "cat notes.md; sub A B f.go >/dev/null", "sub: 1 replacement in 1 file: f.go\n", 0, 0)
	if got.output != "" || u.outcome("sub", got.output, got.code) != "unknown" {
		t.Errorf("cat-before stdout-hidden call output=%q outcome=%q; want empty unknown", got.output, u.outcome("sub", got.output, got.code))
	}
	command = "sub A B f.go >/dev/null; sub C D g.go; cat notes.md"
	output = "sub: 1 replacement in 1 file: notes.go\n"
	for ordinal := range 2 {
		got = u.attributeUtility("sub", command, output, 0, ordinal)
		if got.output != "" || u.outcome("sub", got.output, got.code) != "unknown" {
			t.Errorf("cat-after block call %d output=%q outcome=%q; want empty unknown", ordinal, got.output, u.outcome("sub", got.output, got.code))
		}
	}
	command = "sub a b g.go >/dev/null; sub b g.go; cat notes.md"
	output = "sub: 1 replacement in 1 file: a b g.go\n"
	for ordinal := range 2 {
		got = u.attributeUtility("sub", command, output, 0, ordinal)
		if got.output != "" || u.outcome("sub", got.output, got.code) != "unknown" {
			t.Errorf("cat-after argument match call %d output=%q outcome=%q; want empty unknown", ordinal, got.output, u.outcome("sub", got.output, got.code))
		}
	}
	command = "cat notes.md; sub a b g.go >/dev/null 2>&1; sub a g.go; sub E F j.go"
	output = "sub: invalid b in g.go\n"
	for ordinal := 1; ordinal <= 2; ordinal++ {
		got = u.attributeUtility("sub", command, output, 0, ordinal)
		if got.output != "" || u.outcome("sub", got.output, got.code) != "unknown" {
			t.Errorf("fully redirected source block call %d output=%q outcome=%q; want empty unknown", ordinal, got.output, u.outcome("sub", got.output, got.code))
		}
	}

	command = "sub a b f.go >/dev/null; sub c d g.go >/dev/null 2>&1; sub e f h.go; cat notes.md"
	output = "sub: 1 replacement in 1 file: f.go 1 (line 1)\nsub: 1 replacement in 1 file: h.go 1 (line 3)\n"
	for ordinal, want := range []string{"unknown", "unknown", "success"} {
		got := u.attributeUtility("sub", command, output, 0, ordinal)
		if outcome := u.outcome("sub", got.output, got.code); outcome != want {
			t.Errorf("visible-block call %d output=%q outcome=%q; want %q", ordinal, got.output, outcome, want)
		}
		if ordinal == 2 && (!strings.Contains(got.output, "h.go") || strings.Contains(got.output, "f.go")) {
			t.Errorf("visible third call received wrong blocks: %q", got.output)
		}
	}
	command = "sub a b f.go >/dev/null; sub c d g.go >/dev/null 2>&1; sub e f h.go"
	output = "sub: 1 replacement in 1 file: f.go 1 (line 1)\nsub: 1 replacement in 1 file: h.go 1 (line 3)\n"
	for ordinal, want := range []string{"unknown", "unknown", "success"} {
		got := u.attributeUtility("sub", command, output, 0, ordinal)
		outcome := u.outcome("sub", got.output, got.code)
		if outcome != want {
			t.Errorf("aligned visible call %d output=%q outcome=%q; want %q", ordinal, got.output, outcome, want)
		}
		if ordinal == 0 && got.output != "" || ordinal == 2 && (!strings.Contains(got.output, "h.go") || strings.Contains(got.output, "f.go")) {
			t.Errorf("aligned visible call %d got block %q", ordinal, got.output)
		}
	}

	command = "sub foo x f.go; sub bar y g.go; sub q r hidden.go >/dev/null 2>&1; sub s t hidden2.go >/dev/null"
	output = "sub: 1 replacement in 1 file: f.go\nsub: 1 replacement in 1 file\n"
	for ordinal := range 4 {
		got := u.attributeUtility("sub", command, output, 0, ordinal)
		want := "unknown"
		if ordinal < 2 {
			want = "success"
		}
		if outcome := u.outcome("sub", got.output, got.code); outcome != want {
			t.Errorf("unmatched-block call %d output=%q outcome=%q; want %q", ordinal, got.output, outcome, want)
		}
	}

	command = "sub foo x f.go; sub bar y g.go; sub q r hidden.go >/dev/null"
	output = "sub: 1 replacement in 1 file: f.go\nsub: 1 replacement in 1 file\n"
	for ordinal := range 3 {
		got := u.attributeUtility("sub", command, output, 0, ordinal)
		want := "success"
		if ordinal == 2 {
			want = "unknown"
		}
		if u.outcome("sub", got.output, got.code) != want {
			t.Errorf("unique unmatched output block call %d got %q (%q); want %q", ordinal, got.output, u.outcome("sub", got.output, got.code), want)
		}
	}
	command = "sub a b g.go >/dev/null; sub a g.go; sub b g.go"
	output = "sub: 1 replacement in 1 file: a b g.go\n"
	for ordinal := range 3 {
		got := u.attributeUtility("sub", command, output, 0, ordinal)
		if outcome := u.outcome("sub", got.output, got.code); outcome != "unknown" {
			t.Errorf("tied visible block call %d output=%q outcome=%q; want unknown", ordinal, got.output, outcome)
		}
	}
}

func TestChainAttributionR8VisibleSourcesKeepPositionalBlocks(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	for _, tc := range []struct {
		command string
		output  string
	}{
		{
			"sub a b f.go; sub c d f.go; grep -n x f.go",
			"sub: 1 replacement in 1 file: f.go 1 (line 1)\nsub: 1 replacement in 1 file: f.go 1 (line 2)\nf.go:3:x\n",
		},
		{
			"sub <<'EOF'\nFILE f.go\nOLD\na\nNEW\nb\nEOF\nsub <<'EOF'\nFILE g.go\nOLD\nc\nNEW\nd\nEOF\ngrep -rn x README.md | head",
			"sub: 1 replacement in 1 file: f.go 1 (line 1)\nsub: 1 replacement in 1 file: g.go 1 (line 1)\nREADME.md:3:x\n",
		},
	} {
		for ordinal := range 2 {
			got := u.attributeUtility("sub", tc.command, tc.output, 0, ordinal)
			if outcome := u.outcome("sub", got.output, got.code); outcome != "success" {
				t.Errorf("%q call %d output=%q outcome=%q; want success", tc.command, ordinal, got.output, outcome)
			}
		}
	}
	command := "sub a b f.go >/dev/null && sub <<'EOF'\nFILE g.go\nOLD\nc\nNEW\nd\nEOF\ngo test ./... 2>&1 | grep -v \"^graft\" | tail -3"
	output := "sub: read FILE OLD/NEW form; translated to SEARCH/REPLACE:\ng.go\n<<<<<<< SEARCH\nc\n=======\nd\n>>>>>>> REPLACE\nsub: 1 replacement in 1 file: g.go 1 (line 1)\nok  \tx/pkg\t0.01s\n"
	for ordinal, want := range []string{"unknown", "success"} {
		got := u.attributeUtility("sub", command, output, 0, ordinal)
		if outcome := u.outcome("sub", got.output, got.code); outcome != want {
			t.Errorf("filtered pipeline source call %d output=%q outcome=%q; want %q", ordinal, got.output, outcome, want)
		}
	}
}

func TestChainAttributionR8UsageMarkerSkipsOwnedAndHiddenCalls(t *testing.T) {
	const marker = "sub: line 3: \"=======\" inside the REPLACE part of block 1 (line 2)\n(sub -h for usage)\n"
	for _, tc := range []struct {
		command string
		owner   int
	}{
		{"sub 3 b f.go && sub -F s2", 1},
		{"sub -F s1 && sub -F s2 >/dev/null 2>&1 && sub -F s3", 2},
	} {
		calls, err := shellcmd.Calls(tc.command)
		if err != nil {
			t.Fatalf("Calls(%q): %v", tc.command, err)
		}
		toolCalls := make([]shellcmd.Call, 0, len(calls))
		for _, call := range calls {
			if call.Name == "sub" {
				toolCalls = append(toolCalls, call)
			}
		}
		visible := visibleOutputCalls(toolCalls)
		matched := make([][]string, len(toolCalls))
		matched[0] = []string{"sub: 1 replacement in 1 file: f.go 3 (line 3)\n"}
		assignOutputBlock(marker, toolCalls, visible, matched, true)
		markerOwners := 0
		for i := range matched {
			for _, block := range matched[i] {
				if strings.Contains(block, "-h for usage") {
					markerOwners++
					if i != tc.owner {
						t.Errorf("%q marker assigned to unexpected call %d: %v", tc.command, i, matched)
					}
				}
			}
		}
		if markerOwners != 1 {
			t.Errorf("%q marker assigned to %v; want call %d", tc.command, matched, tc.owner)
		}
	}
}

func TestChainAttributionR9LoopSuccessBlocksStayWithLoopCall(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	const output = "sub: 1 replacement in 1 file: a.go 1 (line 1)\nsub: 1 replacement in 1 file: b.go 1 (line 1)\nsub: line 5: \"=======\" inside the REPLACE part of block 1 (line 2)\n(sub -h for usage)\n"
	for _, tc := range []struct {
		command string
		output  string
		wants   []string
	}{
		{"for f in a.go b.go; do sub x y $f; done; sub -F s1", output, []string{"success", "error"}},
		{"sub x y a.go; sub x y b.go; sub -F s1", output, []string{"success", "success", "error"}},
		{
			"sub c d g.go; for f in a.go b.go; do sub x y $f; done; sub -F s1",
			"sub: 1 replacement in 1 file: g.go 1 (line 1)\nsub: 1 replacement in 1 file: a.go 1 (line 1)\nsub: 1 replacement in 1 file: b.go 1 (line 1)\nsub: line 5: \"=======\" inside the REPLACE part of block 1 (line 2)\n(sub -h for usage)\n",
			[]string{"success", "success", "error"},
		},
	} {
		calls, err := shellcmd.Find(tc.command, "sub")
		if err != nil || len(calls) != len(tc.wants) {
			t.Fatalf("Find(%q)=%+v, %v; wants %d utility calls", tc.command, calls, err, len(tc.wants))
		}
		for ordinal, want := range tc.wants {
			got := u.attributeUtility("sub", tc.command, tc.output, 2, ordinal)
			if outcome := u.outcome("sub", got.output, got.code); outcome != want {
				t.Errorf("%q call %d output=%q code=%d outcome=%q; want %q", tc.command, ordinal, got.output, got.code, outcome, want)
			}
			wantFault := ""
			if ordinal == len(tc.wants)-1 {
				wantFault = "call-error"
			}
			if got.fault != wantFault {
				t.Errorf("%q call %d fault=%q; want %q", tc.command, ordinal, got.fault, wantFault)
			}
		}
	}
}

func TestChainAttributionR9PrefixSkipsNonEmittingCall(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	command := "sub a b f.go >/dev/null; sub c d g.go; sub -F s1"
	output := "sub: 1 replacement in 1 file: g.go 1 (line 1)\nsub: line 5: \"=======\" inside the REPLACE part of block 1 (line 2)\n(sub -h for usage)\n"
	for ordinal, want := range []struct{ outcome, fault string }{{"unknown", ""}, {"success", ""}, {"error", "call-error"}} {
		got := u.attributeUtility("sub", command, output, 2, ordinal)
		if outcome := u.outcome("sub", got.output, got.code); outcome != want.outcome || got.fault != want.fault {
			t.Errorf("call %d output=%q code=%d outcome=%q fault=%q; want %q, %q", ordinal, got.output, got.code, outcome, got.fault, want.outcome, want.fault)
		}
	}
}

func TestChainAttributionR9PrefixOnlyAssignsSuccessBlocks(t *testing.T) {
	calls, err := shellcmd.Find("sub a b f.go; sub -F s1", "sub")
	if err != nil || len(calls) != 2 {
		t.Fatalf("Find: calls=%+v err=%v", calls, err)
	}
	blocks := []string{"sub: OLD is empty\n", "sub: line 5: invalid REPLACE\n(sub -h for usage)\n"}
	matched := make([][]string, len(calls))
	assigned := assignSuccessfulPrefixBeforeUsageError("sub", blocks, calls, []int{0, 1}, matched)
	if slices.Contains(assigned, true) || len(matched[0]) != 0 || len(matched[1]) != 0 {
		t.Errorf("non-success block was prefixed: assigned=%v matched=%v", assigned, matched)
	}
}

func TestChainAttributionR9OutputSourceBoundaries(t *testing.T) {
	for _, command := range []string{"sub a b f.go && cat h.go", "sub a b f.go && grep x f"} {
		calls, err := shellcmd.Calls(command)
		if err != nil {
			t.Fatalf("Calls(%q): %v", command, err)
		}
		found := false
		for _, call := range calls {
			if isOtherOutputSource(call.Name) {
				found = true
				if !isVisibleOutputSource(command, call) {
					t.Errorf("%q call %+v should be a visible source after &&", command, call)
				}
			}
		}
		if !found {
			t.Errorf("Calls(%q) did not contain cat/grep", command)
		}
	}
	for _, command := range []string{"cat f | grep x", "grep pattern", "rg pattern"} {
		calls, err := shellcmd.Calls(command)
		if err != nil {
			t.Fatalf("Calls(%q): %v", command, err)
		}
		for _, call := range calls {
			if isVisibleOutputSource(command, call) {
				t.Errorf("%q call %+v should not be a visible file output source", command, call)
			}
		}
	}
	for _, command := range []string{"grep x f", "rg x f"} {
		calls, err := shellcmd.Calls(command)
		if err != nil || len(calls) != 1 {
			t.Fatalf("Calls(%q)=%+v, %v", command, calls, err)
		}
		if !readsFile(calls[0]) {
			t.Errorf("%q should read an input file", command)
		}
	}
}

func TestChainAttributionR9StdinSubCanHaveUsageError(t *testing.T) {
	calls, err := shellcmd.Find("sub <<'EOF'\nnot a valid spec\nEOF", "sub")
	if err != nil || len(calls) != 1 {
		t.Fatalf("Find: calls=%+v err=%v", calls, err)
	}
	if len(calls[0].Args) != 0 || !callMayHaveUsageError(calls[0]) {
		t.Errorf("stdin sub call %+v should be eligible for a usage error", calls[0])
	}
}

func TestChainAttributionR7UsageErrorMarkerChoosesUnassignedParserCall(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	const parseError = "sub: line 3: \"=======\" inside the REPLACE part of block 1 (line 2)\n(sub -h for usage)\n"
	for _, tc := range []struct {
		command string
		output  string
		faults  []string
	}{
		{
			"sub a b f.go && sub -F s1 && sub -F s2",
			"sub: 1 replacement in 1 file: f.go 1 (line 1)\n" + parseError,
			[]string{"", "call-error", ""},
		},
		{"sub -F s1 && sub -F s2", parseError, []string{"call-error", ""}},
		{
			"sub a b f.go; sub c d g.go >/dev/null; sub -F s1",
			"sub: 1 replacement in 1 file: f.go 1 (line 1)\n" + parseError,
			[]string{"", "", "call-error"},
		},
	} {
		for ordinal, want := range tc.faults {
			got := u.attributeUtility("sub", tc.command, tc.output, 2, ordinal)
			if got.fault != want {
				t.Errorf("%q call %d output=%q fault=%q; want %q", tc.command, ordinal, got.output, got.fault, want)
			}
			if want == "call-error" && u.outcome("sub", got.output, got.code) != "error" {
				t.Errorf("%q call %d parse failure outcome=%q; want error", tc.command, ordinal, u.outcome("sub", got.output, got.code))
			}
		}
	}
	command := "sub a b f.go && sub -F s1 && sub -F s2 && sub -F s3"
	output := "sub: 1 replacement in 1 file: f.go 1 (line 1)\n" + parseError
	for ordinal, want := range []string{"success", "error", "unknown", "unknown"} {
		got := u.attributeUtility("sub", command, output, 2, ordinal)
		if ordinal == 1 && got.fault != "call-error" {
			t.Errorf("three-call parser error call %d fault=%q; want call-error", ordinal, got.fault)
		}
		if ordinal != 1 && got.fault != "" {
			t.Errorf("three-call parser error call %d fault=%q; want none", ordinal, got.fault)
		}
		if outcome := u.outcome("sub", got.output, got.code); outcome != want {
			t.Errorf("three-call parser error call %d output=%q outcome=%q; want %q", ordinal, got.output, outcome, want)
		}
	}
	command = "sub 3 b f.go && sub -F s2"
	output = "sub: 1 replacement in 1 file: f.go 3 (line 3)\n" + parseError
	for ordinal, want := range []string{"success", "error"} {
		got := u.attributeUtility("sub", command, output, 2, ordinal)
		wantFault := ""
		if ordinal == 1 {
			wantFault = "call-error"
		}
		if got.fault != wantFault || u.outcome("sub", got.output, got.code) != want {
			t.Errorf("argument-matched usage marker call %d output=%q fault=%q outcome=%q; want %q, %q", ordinal, got.output, got.fault, u.outcome("sub", got.output, got.code), want, wantFault)
		}
	}
	command = "sub a b f.go; sub c d g.go >/dev/null 2>&1; sub -F s1"
	output = "sub: 1 replacement in 1 file: f.go 1 (line 1)\n" + parseError
	for ordinal, wantFault := range []string{"", "", "call-error"} {
		got := u.attributeUtility("sub", command, output, 2, ordinal)
		if got.fault != wantFault {
			t.Errorf("fully redirected parser candidate %d fault=%q; want %q (output=%q)", ordinal, got.fault, wantFault, got.output)
		}
	}
}
