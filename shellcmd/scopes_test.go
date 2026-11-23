package shellcmd

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestScopesKindsBoundsAndMetadata(t *testing.T) {
	type want struct {
		kind     ScopeKind
		text     string
		name     string
		decl     int
		isolated bool
		last     bool
	}
	w := func(kind ScopeKind, text string) want { return want{kind: kind, text: text, decl: -1} }
	for _, tc := range []struct {
		src  string
		want []want
	}{
		{`f() { S=1; }; f`, []want{{kind: ScopeFunctionBody, text: "{ S=1; }", name: "f", decl: 0}}},
		{`function g { S=1; }`, []want{{kind: ScopeFunctionBody, text: "{ S=1; }", name: "g", decl: 0}}},
		{`echo; function g { :; }`, []want{{kind: ScopeFunctionBody, text: "{ :; }", name: "g", decl: 6}}},
		{`echo x; g() { :; }`, []want{{kind: ScopeFunctionBody, text: "{ :; }", name: "g", decl: 8}}},
		{`f() ( S=1 ); f`, []want{{kind: ScopeFunctionBody, text: "( S=1 )", name: "f", decl: 0, isolated: true}}},
		{`( S=1 )`, []want{w(ScopeSubshell, "( S=1 )")}},
		{`echo $(S=1)`, []want{w(ScopeCmdSubst, "$(S=1)")}},
		{`echo ${ S=1; }`, nil},
		{`echo ${| S=1; }`, nil},
		{`echo "${ S=1; }"`, nil},
		{`echo "${| S=1; }"`, nil},
		{"echo `S=1`", []want{w(ScopeCmdSubst, "`S=1`")}},
		{`cat <(S=1)`, []want{w(ScopeProcSubst, "<(S=1)")}},
		{`coproc { S=1; }`, []want{w(ScopeCoproc, "coproc { S=1; }")}},
		{`S=1 &`, []want{w(ScopeBackground, "S=1 &")}},
		{`true | S=B`, []want{w(ScopePipePart, "true"), {kind: ScopePipePart, text: "S=B", decl: -1, last: true}}},
		{`a | S=1 | b`, []want{w(ScopePipePart, "a"), w(ScopePipePart, "S=1"), {kind: ScopePipePart, text: "b", decl: -1, last: true}}},
		{`f() { ( a | b ); }`, []want{
			{kind: ScopeFunctionBody, text: "{ ( a | b ); }", name: "f", decl: 0},
			w(ScopeSubshell, "( a | b )"), w(ScopePipePart, "a"),
			{kind: ScopePipePart, text: "b", decl: -1, last: true},
		}},
		{`f() { g() { S=1; }; }`, []want{
			{kind: ScopeFunctionBody, text: "{ g() { S=1; }; }", name: "f", decl: 0},
			{kind: ScopeFunctionBody, text: "{ S=1; }", name: "g", decl: 6},
		}},
		{`echo plain; S=1`, nil},
	} {
		got, err := Scopes(tc.src)
		if err != nil {
			t.Errorf("Scopes(%q): %v", tc.src, err)
			continue
		}
		gotWant := make([]want, 0, len(got))
		for _, scope := range got {
			if scope.Start < 0 || scope.End > len(tc.src) || scope.Start >= scope.End {
				t.Errorf("Scopes(%q): invalid bounds %+v", tc.src, scope)
				continue
			}
			gotWant = append(gotWant, want{scope.Kind, tc.src[scope.Start:scope.End], scope.Name, scope.DeclOffset, scope.Isolated, scope.Last})
		}
		if !slices.Equal(gotWant, tc.want) {
			t.Errorf("Scopes(%q) = %+v, want %+v", tc.src, gotWant, tc.want)
		}
	}
}

func TestFunsubsDoNotIsolateAssignments(t *testing.T) {
	for _, src := range []string{
		`S=good; x=${ S=bad; }; : "$S"`,
		`S=good; x=${| S=bad; }; : "$S"`,
		`S=good; x="${ S=bad; }"; : "$S"`,
		`S=good; x="${| S=bad; }"; : "$S"`,
	} {
		off := strings.Index(src, "S=bad")
		assigns, err := Assigns(src)
		if err != nil {
			t.Fatalf("Assigns(%q): %v", src, err)
		}
		visible := false
		for _, a := range assigns {
			if a.Name == "S" && a.Value == "bad" && a.Offset == off && a.VisibleAt(len(src)) {
				visible = true
			}
		}
		if !visible {
			t.Errorf("%q: funsub assignment is not visible after the substitution: %+v", src, assigns)
		}
	}
	for _, src := range []string{`echo $(S=1)`, "echo `S=1`"} {
		scopes, err := Scopes(src)
		if err != nil || len(scopes) != 1 || scopes[0].Kind != ScopeCmdSubst {
			t.Errorf("ordinary command substitution Scopes(%q) = %+v, %v; want CmdSubst", src, scopes, err)
		}
	}
}

func TestFunsubLocalAssignments(t *testing.T) {
	tests := []struct {
		name        string
		src         string
		varName     string
		wantVisible bool
	}{
		{"declare", `S=good; x=${ declare S=bad; }; : "$S"`, "S", false},
		{"reply declare other variable", `S=good; x=${| declare S=bad; }; : "$S"`, "S", false},
		{"typeset", `S=good; x=${ typeset S=bad; }; : "$S"`, "S", false},
		{"local", `S=good; x=${ local S=bad; }; : "$S"`, "S", false},
		{"local export attribute", `S=good; x=${ local -x S=bad; }; : "$S"`, "S", false},
		{"declare global", `S=good; x=${ declare -g S=bad; }; : "$S"`, "S", true},
		{"declare global then assignment", `S=good; x=${ declare -g S=x; S=bad; }; : "$S"`, "S", true},
		{"declare global over funsub local", `S=good; x=${ local S; declare -g S=bad; }; : "$S"`, "S", true},
		{"export", `S=good; x=${ export S=bad; }; : "$S"`, "S", true},
		{"plain assignment", `S=good; x=${ S=bad; }; : "$S"`, "S", true},
		{"declare after assignment", `S=good; x=${ S=bad; declare S=x; }; : "$S"`, "S", true},
		{"if local", `S=good; x=${ if true; then local S=bad; fi; }; : "$S"`, "S", false},
		{"and declare", `S=good; x=${ true && declare S=bad; }; : "$S"`, "S", false},
		{"or local", `S=good; x=${ false || local S=bad; }; : "$S"`, "S", false},
		{"while local", `S=good; x=${ while true; do local S=bad; break; done; }; : "$S"`, "S", false},
		{"case typeset", `S=good; x=${ case a in a) typeset S=bad;; esac; }; : "$S"`, "S", false},
		{"for local", `S=good; x=${ for i in 1; do local S=bad; done; }; : "$S"`, "S", false},
		{"if local then assign", `S=good; x=${ if true; then local S; fi; S=bad; }; : "$S"`, "S", false},
		{"assignment after funsub", `S=good; x=${ local S; }; S=bad; : "$S"`, "S", true},
		{"function local", `f() { S=good; x=${ local S=bad; }; : "$S"; }; f`, "S", false},
		{"function local then assignment", `f() { S=good; x=${ local S; S=bad; }; : "$S"; }; f`, "S", false},
		{"reply local", `REPLY=good; x=${| REPLY=bad; }; : "$REPLY"`, "REPLY", false},
		{"reply declaration local", `REPLY=good; x=${| declare REPLY=bad; }; : "$REPLY"`, "REPLY", false},
		{"plain reply assignment", `REPLY=good; x=${ REPLY=bad; }; : "$REPLY"`, "REPLY", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			at := strings.Index(tc.src, `: "$`+tc.varName+`"`)
			if at < 0 {
				t.Fatalf("marker missing from %q", tc.src)
			}
			assigns, err := Assigns(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, a := range assigns {
				if a.Name == tc.varName && a.Value == "bad" {
					found = true
					if got := a.VisibleAt(at); got != tc.wantVisible {
						t.Errorf("%+v visible at marker = %v, want %v", a, got, tc.wantVisible)
					}
				}
			}
			if !found && tc.wantVisible {
				t.Errorf("no bad assignment for %s in %+v", tc.varName, assigns)
			}
		})
	}
	for _, src := range []string{
		`S=good; x=${ declare S=bad; : "$S"; }`,
		`S=good; x=${ typeset S=bad; : "$S"; }`,
		`S=good; x=${ local S=bad; : "$S"; }`,
		`REPLY=good; x=${| REPLY=bad; : "$REPLY"; }`,
	} {
		at := strings.Index(src, `: "$`)
		assigns, err := Assigns(src)
		if err != nil {
			t.Fatal(err)
		}
		visible := false
		for _, a := range assigns {
			if a.Value == "bad" && a.VisibleAt(at) {
				visible = true
			}
		}
		if !visible {
			t.Errorf("funsub local is not visible inside its body at %d: %+v", at, assigns)
		}
	}

	const called = `f() { x=${ local S; S=bad; }; }; S=good; f; : "$S"`
	call := strings.LastIndex(called, "f;")
	assigns, err := Assigns(called)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range assigns {
		if a.Name == "S" && a.Unknown && a.Offset == call {
			t.Errorf("funsub local leaked as unknown function write: %+v", a)
		}
	}
	const afterFunsub = `f() { x=${ local S; }; S=bad; }; S=good; f; : "$S"`
	call = strings.LastIndex(afterFunsub, "f;")
	assigns, err = Assigns(afterFunsub)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range assigns {
		if a.Name == "S" && a.Unknown && a.Offset == call && a.VisibleAt(len(afterFunsub)+1) {
			return
		}
	}
	t.Errorf("write after funsub-local binding was lost: %+v", assigns)
	const branchLocal = `f() { x=${ if true; then local S; fi; S=bad; }; }; S=good; f; : "$S"`
	call = strings.LastIndex(branchLocal, "f;")
	assigns, err = Assigns(branchLocal)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range assigns {
		if a.Name == "S" && a.Unknown && a.Offset == call && a.VisibleAt(len(branchLocal)+1) {
			return
		}
	}
	t.Errorf("branch funsub local was not used to suppress function write: %+v", assigns)
}

func TestFunsubLocalBindsToNearestShell(t *testing.T) {
	for _, tc := range []struct {
		src  string
		kind ScopeKind
	}{
		{`S=good; x=${ (declare S=bad); }; : "$S"`, ScopeSubshell},
		{`S=good; x=${ declare S=bad | cat; }; : "$S"`, ScopePipePart},
	} {
		off := strings.Index(tc.src, "S=bad")
		scopes, err := Scopes(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		var want Scope
		for _, scope := range scopes {
			if scope.Kind == tc.kind && scope.Start <= off && off < scope.End &&
				(scope.End-scope.Start < want.End-want.Start || want.End == 0) {
				want = scope
			}
		}
		if want.End == 0 {
			t.Fatalf("no %s scope contains declaration: %+v", tc.kind, scopes)
		}
		assigns, err := Assigns(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, a := range assigns {
			if a.Name == "S" && a.Value == "bad" && a.Offset == off {
				found = true
				if a.ScopeStart != want.Start || a.ScopeEnd != want.End {
					t.Errorf("nearest shell scope for declaration = [%d,%d), want %+v", a.ScopeStart, a.ScopeEnd, want)
				}
			}
		}
		if !found {
			t.Errorf("declaration assignment missing: %+v", assigns)
		}
	}
}

func TestFunsubNestedVisibilityAndReplyRange(t *testing.T) {
	for _, tc := range []struct {
		name, variable, src, want string
	}{
		{"nested local", "S", `S=good; x=${ local S=good; y=${ local S; S=bad; }; printf '%s\n' "$S" >&2; }`, "good"},
		{"reply outer local inner", "REPLY", `REPLY=good; x=${| REPLY=good; y=${ local REPLY=bad; }; printf '%s\n' "$REPLY" >&2; }`, "good"},
		{"local outer reply inner", "REPLY", `x=${ local REPLY=good; y=${| REPLY=bad; }; printf '%s\n' "$REPLY" >&2; }`, "good"},
		{"reply assignment before", "REPLY", `REPLY=bad; x=${| :; }; printf '%s\n' "$REPLY"`, "bad"},
	} {
		at := strings.LastIndex(tc.src, `printf '%s\n' "$`+tc.variable+`"`)
		if at < 0 {
			at = strings.LastIndex(tc.src, `: "$`+tc.variable+`"`)
		}
		assigns, err := Assigns(tc.src)
		if err != nil {
			t.Fatalf("%s: Assigns: %v", tc.name, err)
		}
		got, gotOff := "none", -1
		for _, a := range assigns {
			if a.Name == tc.variable && a.Offset < at && a.Offset > gotOff && a.VisibleAt(at) {
				got, gotOff = a.Value, a.Offset
			}
		}
		if got != tc.want {
			t.Errorf("%s: got %q want %q; assigns=%+v", tc.name, got, tc.want, assigns)
		}
	}
}

func TestHereDocScopesEndAtCommandSyntax(t *testing.T) {
	tests := []struct {
		src          string
		visible      bool
		bodyIsScoped bool
	}{
		{"S=good; cat <<EOF | cat >/dev/null; S=bad\nx\nEOF\n: \"$S\"", true, false},
		{"S=good; cat <<EOF | cat >/dev/null && S=bad\nx\nEOF\n: \"$S\"", true, false},
		{"S=good; coproc cat <<EOF; S=bad\nx\nEOF\n: \"$S\"", true, false},
		{"S=good; cat <<EOF & S=bad\nx\nEOF\nwait; : \"$S\"", true, false},
		{"S=good; cat <<EOF | cat >/dev/null\n${ S=bad; }\nEOF\n: \"$S\"", false, true},
		{"S=good; true | cat <<EOF >/dev/null\n${ S=bad; }\nEOF\n: \"$S\"", false, true},
		{"S=good; cat <<EOF >/dev/null | cat\n${ S=bad; }\nEOF\n: \"$S\"", false, true},
		{"S=good; cat <<EOF >/dev/null &\n${ S=bad; }\nEOF\nwait; : \"$S\"", false, true},
		{"S=good; coproc cat <<EOF >/dev/null\n${ S=bad; }\nEOF\nwait; : \"$S\"", false, true},
		{"S=good; : <<EOF | cat\n${ S=bad; }\nEOF\n: \"$S\"", false, true},
		{"S=good; : <<EOF &\n${ S=bad; }\nEOF\nwait; : \"$S\"", false, true},
		{"S=good; : <<EOF\n${ S=bad; }\nEOF\n: \"$S\"", true, false},
	}
	for _, tc := range tests {
		src := tc.src
		bad := strings.Index(src, "S=bad")
		scopes, err := Scopes(src)
		if err != nil {
			t.Fatalf("Scopes(%q): %v", src, err)
		}
		for _, scope := range scopes {
			if !tc.bodyIsScoped && (scope.Kind == ScopePipePart || scope.Kind == ScopeCoproc || scope.Kind == ScopeBackground) && scope.Start <= bad && bad < scope.End {
				t.Errorf("%q: scope extends over following assignment at %d: %+v", src, bad, scope)
			}
		}
		if tc.bodyIsScoped {
			bodyScope := false
			for _, scope := range scopes {
				if scope.Start <= bad && bad < scope.End &&
					(scope.Kind == ScopePipePart || scope.Kind == ScopeCoproc || scope.Kind == ScopeBackground) {
					bodyStart := strings.Index(src, "\n${") + 1
					bodyEnd := strings.Index(src[bodyStart:], "\nEOF") + bodyStart
					if scope.Start == bodyStart && scope.End >= bodyEnd {
						bodyScope = true
					}
				}
			}
			if !bodyScope {
				t.Errorf("%q: no child-process scope covers here-doc expansion at %d: %+v", src, bad, scopes)
			}
		}
		assigns, err := Assigns(src)
		if err != nil {
			t.Fatalf("Assigns(%q): %v", src, err)
		}
		visible := false
		for _, a := range assigns {
			if a.Name == "S" && a.Offset == bad && a.VisibleAt(len(src)) {
				visible = true
			}
		}
		if visible != tc.visible {
			t.Errorf("%q: following assignment is not visible: %+v", src, assigns)
		}
	}
}

func TestHereDocScopeIncludesCommandWord(t *testing.T) {
	const src = "<<EOF cat | cat\nx\nEOF\n"
	scopes, err := Scopes(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) < 3 {
		t.Fatalf("Scopes(%q) = %+v, want pipeline and here-doc ranges", src, scopes)
	}
	if got := src[scopes[0].Start:scopes[0].End]; got != "<<EOF cat" {
		t.Errorf("first pipeline scope = %q, want command including cat", got)
	}
}

func TestHereDocScopesForSubshellAndProcessSubstitution(t *testing.T) {
	tests := []struct {
		name string
		src  string
		kind ScopeKind
	}{
		{"subshell", "S=good; ( : <<EOF )\n${ S=bad; }\nEOF\n: \"$S\"", ScopeSubshell},
		{"subshell before following command", "S=good; ( : <<EOF ); T=1\n${ S=bad; }\nEOF\n: \"$S\"", ScopeSubshell},
		{"process substitution", "S=good; cat <( : <<EOF ) >/dev/null\n${ S=bad; }\nEOF\n: \"$S\"", ScopeProcSubst},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bad := strings.Index(tc.src, "S=bad")
			scopes, err := Scopes(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			covered := false
			for _, scope := range scopes {
				if scope.Kind == tc.kind && scope.Start <= bad && bad < scope.End {
					covered = true
				}
				if tc.name == "subshell before following command" && scope.Kind == tc.kind &&
					scope.Start <= strings.Index(tc.src, "T=1") && strings.Index(tc.src, "T=1") < scope.End {
					t.Errorf("subshell scope captured following command: %+v", scope)
				}
			}
			if !covered {
				t.Errorf("%s scope does not cover here-doc body assignment: %+v", tc.name, scopes)
			}
			assigns, err := Assigns(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range assigns {
				if a.Name == "S" && a.Offset == bad && a.VisibleAt(len(tc.src)) {
					t.Errorf("here-doc assignment escaped %s scope: %+v", tc.name, a)
				}
			}
		})
	}
}

func TestHereDocScopePreservesPipelineLast(t *testing.T) {
	const src = "echo x | bash /dev/fd/4 4<&0 <<'E'\necho ok\nE\n"
	body := strings.Index(src, "echo ok")
	scopes, err := Scopes(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range scopes {
		if scope.Kind == ScopePipePart && scope.Start == body && scope.End > body {
			if !scope.Last {
				t.Errorf("last pipeline here-doc scope lost Last metadata: %+v", scope)
			}
			return
		}
	}
	t.Errorf("no pipeline scope covers here-doc body at %d: %+v", body, scopes)
}

func TestFunsubLocalPreservesNestedAssignmentScopes(t *testing.T) {
	tests := []struct {
		name, variable, src string
	}{
		{"subshell", "S", `S=good; x=${ local S=good; ( S=bad ); printf '%s\n' "$S" >&2; }`},
		{"command substitution", "S", `S=good; x=${ local S=good; y=$( S=bad ); printf '%s\n' "$S" >&2; }`},
		{"pipeline", "S", `S=good; x=${ local S=good; S=bad | cat; printf '%s\n' "$S" >&2; }`},
		{"background", "S", `S=good; x=${ local S=good; S=bad & wait; printf '%s\n' "$S" >&2; }`},
		{"process substitution", "S", `S=good; x=${ local S=good; cat <(S=bad) >/dev/null; printf '%s\n' "$S" >&2; }`},
		{"uninvoked function", "S", `S=good; x=${ local S=good; g() { S=bad; }; printf '%s\n' "$S" >&2; }`},
		{"reply subshell", "REPLY", `x=${| REPLY=good; ( REPLY=bad ); printf '%s\n' "$REPLY" >&2; }`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			marker := strings.LastIndex(tc.src, `printf '%s\n' "$`+tc.variable+`"`)
			bad := strings.Index(tc.src, tc.variable+"=bad")
			assigns, err := Assigns(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range assigns {
				if a.Name == tc.variable && a.Offset == bad && a.VisibleAt(marker) {
					t.Errorf("nested assignment escaped its isolated scope: %+v", a)
				}
			}
		})
	}
}

func TestHereDocScopesCoverEveryExecutionOwner(t *testing.T) {
	tests := []struct {
		name string
		src  string
		kind ScopeKind
	}{
		{"command substitution", "S=good; x=$( : <<EOF )\n${ S=bad; }\nEOF\n: \"$S\"", ScopeCmdSubst},
		{"function body", "S=good; f() { : <<EOF; }\n${ S=bad; }\nEOF\n: \"$S\"", ScopeFunctionBody},
		{"function redirection", "S=good; f() { :; } <<EOF\n${ S=bad; }\nEOF\n: \"$S\"", ScopeFunctionBody},
		{"subshell redirection", "S=good; ( : ) <<EOF\n${ S=bad; }\nEOF\n: \"$S\"", ScopeSubshell},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bad := strings.Index(tc.src, "S=bad")
			scopes, err := Scopes(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			covered := false
			for _, scope := range scopes {
				if scope.Kind == tc.kind && scope.Start <= bad && bad < scope.End {
					covered = true
				}
			}
			if !covered {
				t.Errorf("no %s scope covers here-doc at %d: %+v", tc.kind, bad, scopes)
			}
			assigns, err := Assigns(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range assigns {
				if a.Name == "S" && a.Offset == bad && a.VisibleAt(len(tc.src)) {
					t.Errorf("here-doc assignment escaped its execution owner: %+v", a)
				}
			}
		})
	}
}

func TestHereDocsWithoutACommandAreIsolated(t *testing.T) {
	tests := []struct {
		name, src string
		isolated  bool
	}{
		{"assignment with funsub", "S=good; x=${ :; } <<EOF\n${ S=bad; }\nEOF\n: \"$S\"", true},
		{"assignment with command substitution", "S=good; x=$( : ) <<EOF\n${ S=bad; }\nEOF\n: \"$S\"", true},
		{"redirection only", "S=good; <<EOF\n${ S=bad; }\nEOF\n: \"$S\"", true},
		{"assignment only", "S=good; T=x <<EOF\n${ S=bad; }\nEOF\n: \"$S\"", true},
		{"named command control", "S=good; : <<EOF\n${ S=bad; }\nEOF\n: \"$S\"", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bad := strings.Index(tc.src, "S=bad")
			scopes, err := Scopes(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			covered := false
			for _, scope := range scopes {
				if scope.Kind == ScopeSubshell && scope.Start <= bad && bad < scope.End {
					covered = true
					if scope.Name != "" || scope.DeclOffset != -1 {
						t.Errorf("non-function here-doc scope metadata = %+v", scope)
					}
				}
			}
			if covered != tc.isolated {
				t.Errorf("isolated scope coverage = %v, want %v: %+v", covered, tc.isolated, scopes)
			}
			assigns, err := Assigns(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			visible := false
			for _, a := range assigns {
				if a.Name == "S" && a.Offset == bad && a.VisibleAt(len(tc.src)) {
					visible = true
				}
			}
			if visible == tc.isolated {
				t.Errorf("assignment visibility = %v, isolated=%v; assigns=%+v", visible, tc.isolated, assigns)
			}
		})
	}
}

func TestFunctionHereDocFunsubsAffectFunctionChanges(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{"body redirect", "S=good; f() { : <<EOF; }\n${ S=bad; }\nEOF\nf; printf '%s\\n' \"$S\"", "unknown"},
		{"function redirect", "S=good; f() { :; } <<EOF\n${ S=bad; }\nEOF\nf; printf '%s\\n' \"$S\"", "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			marker := strings.LastIndex(tc.src, `printf '%s\n' "$S"`)
			call := strings.LastIndex(tc.src, "\nf;") + 1
			if marker < 0 || call == 0 {
				t.Fatalf("marker/call missing: marker=%d call=%d", marker, call)
			}
			assigns, err := Assigns(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			got := "good"
			for _, a := range assigns {
				if a.Name == "S" && a.Offset == call && a.VisibleAt(marker) {
					got = a.Value
					if a.Unknown {
						got = "unknown"
					}
				}
			}
			if got != tc.want {
				t.Errorf("function call value = %q, want %q: %+v", got, tc.want, assigns)
			}
		})
	}
}

func TestNestedFunctionHereDocsKeepTheirOwnScope(t *testing.T) {
	for _, src := range []string{
		"f() { g() { : <<EOF; }\nx\nEOF\n}",
		"f() ( g() { : <<EOF; }\nx\nEOF\n)",
	} {
		scopes, err := Scopes(src)
		if err != nil {
			t.Fatal(err)
		}
		doc := strings.Index(src, "x\nEOF")
		for _, scope := range scopes {
			if scope.Start == doc && scope.Name == "f" {
				t.Fatalf("nested g here-doc was attributed to f: %+v", scopes)
			}
		}
	}
}

func TestFunctionHereDocWritesUnionDefinitionsAtCall(t *testing.T) {
	const printf = "printf '%s\\n' \"$S\""
	for _, tc := range []struct{ name, src, want string }{
		{"call before redefine", "S=good; f() { : <<EOF; }\n${ S=bad; }\nEOF\nf; f() { :; }; " + printf, "unknown"},
		{"call after redefinitions", "S=good; f() { S=bad; }; f() { :; }; f; " + printf, "unknown"},
		{"call before first definition", "S=good; f; f() { : <<EOF; }\n${ S=bad; }\nEOF\n" + printf, "unknown"},
		{"doc then body redefine", "S=good; f() { : <<EOF; }\n${ S=bad; }\nEOF\nf() { S=bad2; }; f; " + printf, "unknown"},
		{"conditional redefine", "S=good; if true; then f() { : <<EOF; }\n${ S=bad; }\nEOF\nelse f() { :; }; fi; f; " + printf, "unknown"},
		{"subshell redefine", "S=good; f() { : <<EOF; }\n${ S=bad; }\nEOF\n( f() { :; } ); f; " + printf, "unknown"},
		{"command substitution redefine", "S=good; f() { : <<EOF; }\n${ S=bad; }\nEOF\nx=$( f() { :; } ); f; " + printf, "unknown"},
		{"redefinition inside uncalled function", "S=good; f() { : <<EOF; }\n${ S=bad; }\nEOF\ng() { f() { :; }; }; f; " + printf, "unknown"},
		{"nested function assignment", "f() { S=good; g() { S=bad; }; " + printf + "; }; f", "good"},
		{"nested function in subshell body", "f() ( S=good; g() { S=bad; }; " + printf + " ); f", "good"},
		{"function subshell here-doc body", "f() ( S=good; : <<EOF\n${ S=bad; }\nEOF\n" + printf + " ); f", "unknown"},
		{"call to definition below", "S=good; a() { b; " + printf + "; }; b() { S=bad; }; a", "unknown"},
		{"call to here-doc definition below", "S=good; a() { b; " + printf + "; }; b() { : <<EOF; }\n${ S=bad; }\nEOF\na", "unknown"},
		{"deferred call to definition below", "S=good; a() { b; }; b() { S=bad; }; a; " + printf, "unknown"},
		{"call before definition in loop", "S=good; for i in 1 2; do f; f() { S=bad; }; done; " + printf, "unknown"},
		{"call before definition in while", "S=good; n=0; while [ $n -lt 2 ]; do n=$((n+1)); f; f() { S=bad; }; done; " + printf, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := strings.LastIndex(tc.src, printf)
			assigns, err := Assigns(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			got, gotOff := "none", -1
			for _, assignment := range assigns {
				if assignment.Name == "S" && assignment.Offset < marker && assignment.Offset > gotOff && assignment.VisibleAt(marker) {
					got, gotOff = assignment.Value, assignment.Offset
					if assignment.Unknown {
						got = "unknown"
					}
				}
			}
			if got != tc.want {
				t.Errorf("S at marker = %q, want %q; assigns=%+v", got, tc.want, assigns)
			}
		})
	}
}

func TestFunctionDefinitionRedirectDefersHereDocCalls(t *testing.T) {
	for _, src := range []string{
		"S=good; f() { printf '%s\\n' \"$S\"; } <<EOF\n${ g; }\nEOF\ng() { S=bad; }; f",
		"S=good; g() { S=bad; }; f() { printf '%s\\n' \"$S\"; } <<EOF\n${ g; }\nEOF\nf",
	} {
		marker := strings.Index(src, `"$S"`)
		assigns, err := Assigns(src)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, assignment := range assigns {
			if assignment.Name == "S" && assignment.Unknown && assignment.VisibleAt(marker) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("call in function-definition here-doc was not visible in the body: %+v", assigns)
		}
	}
}

func TestFunsubLocalDeclarationsRemainOrdered(t *testing.T) {
	const src = "S=good; : ${ local S; }; : ${ local T; S=bad; }"
	bad := strings.LastIndex(src, "S=bad")
	assigns, err := Assigns(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, assignment := range assigns {
		if assignment.Name == "S" && assignment.Offset == bad && assignment.VisibleAt(len(src)+1) {
			return
		}
	}
	t.Fatalf("later funsub assignment was hidden by an earlier local: %+v", assigns)
}

func TestFunsubLocalInSubshellDoesNotBindOuterAssignment(t *testing.T) {
	const src = `S=good; x=${ ( local S; ); S=bad; }; : "$S"`
	marker := strings.Index(src, `: "$S"`)
	assigns, err := Assigns(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, assignment := range assigns {
		if assignment.Name == "S" && assignment.VisibleAt(marker) {
			return
		}
	}
	t.Fatalf("subshell local incorrectly bound a later funsub assignment: %+v", assigns)
}

func TestFunctionCallReportsEveryChangedVariable(t *testing.T) {
	const src = "S=good; f() { A=1; S=bad; }; f; : \"$S\""
	call := strings.Index(src, "f; :")
	assigns, err := Assigns(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, assignment := range assigns {
		if assignment.Name == "S" && assignment.Unknown && assignment.Offset == call && assignment.VisibleAt(len(src)+1) {
			return
		}
	}
	t.Fatalf("function call omitted its S write: %+v", assigns)
}

func TestRoundSixHereDocFunctionKillers(t *testing.T) {
	const marker = `printf '%s\n' "$S"`
	tests := []struct{ name, src, want string }{
		{"call before redefinition", "S=good; f() { : <<EOF; }\n${ S=bad; }\nEOF\nf; f() { :; }; " + marker, "unknown"},
		{"call before first definition", "S=good; f; f() { : <<EOF; }\n${ S=bad; }\nEOF\n" + marker, "unknown"},
		{"document then body redefinition", "S=good; f() { : <<EOF; }\n${ S=bad; }\nEOF\nf() { S=bad2; }; f; " + marker, "unknown"},
		{"function subshell preserves nested isolation", "f() { S=good; ( S=bad ); " + marker + "; }; f", "good"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			at := strings.LastIndex(tc.src, marker)
			assigns, err := Assigns(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			got, gotOff := "none", -1
			for _, assignment := range assigns {
				if assignment.Name == "S" && assignment.Offset < at && assignment.Offset > gotOff && assignment.VisibleAt(at) {
					got, gotOff = assignment.Value, assignment.Offset
					if assignment.Unknown {
						got = "unknown"
					}
				}
			}
			if got != tc.want {
				t.Errorf("S at marker = %q, want %q; assigns=%+v", got, tc.want, assigns)
			}
		})
	}
	const nested = "f() { g() { : <<EOF; }\nx\nEOF\n}"
	scopes, err := Scopes(nested)
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.Index(nested, "x\nEOF")
	for _, scope := range scopes {
		if scope.Start == doc && scope.Name == "f" {
			t.Fatalf("nested g here-doc attributed to f: %+v", scopes)
		}
	}
}

func TestRoundSevenHereDocKillers(t *testing.T) {
	const printf = "printf '%s\\n' \"$S\""
	tests := []struct{ name, src, want string }{
		{"k7-154", "S=good; f() { " + printf + "; : <<EOF; }\n${ S=bad; }\nEOF\nf", "unknown"},
		{"k7-205a", "S=good; f() { " + printf + "; } <<EOF\nx\nEOF\nf; S=bad", "good"},
		{"k7-205b", "S=good; ( S=bad ); f() { " + printf + "; } <<EOF\nx\nEOF\nf", "good"},
		{"k7-205c", "( S=bad ); S=good; f() { " + printf + "; } <<EOF\nx\nEOF\nf", "good"},
		{"k7-139", "f() { S=good; : <<EOF | cat\n${ S=bad; }\nEOF\n" + printf + "; }; f", "unknown"},
		{"k7-325-local-heredoc-unknown", "S=good; f() { local S; : <<EOF; }\n${ S=bad; }\nEOF\nf; " + printf, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			at := strings.LastIndex(tc.src, printf)
			assigns, err := Assigns(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			got, gotOff := "none", -1
			for _, assignment := range assigns {
				if assignment.Name == "S" && assignment.Offset < at && assignment.Offset > gotOff && assignment.VisibleAt(at) {
					got, gotOff = assignment.Value, assignment.Offset
					if assignment.Unknown {
						got = "unknown"
					}
				}
			}
			if got != tc.want {
				t.Errorf("S at marker = %q, want %q; assigns=%+v", got, tc.want, assigns)
			}
		})
	}
}

func TestFunctionHereDocWithLocalReportsUnknown(t *testing.T) {
	const src = "S=good; f() { local S; : <<EOF\n${ S=bad; }\nEOF\n}; f; printf '%s\\n' \"$S\""
	marker := strings.LastIndex(src, `printf '%s\n' "$S"`)
	assigns, err := Assigns(src)
	if err != nil {
		t.Fatal(err)
	}
	got, gotOff := "none", -1
	for _, assignment := range assigns {
		if assignment.Name == "S" && assignment.Offset < marker && assignment.Offset > gotOff && assignment.VisibleAt(marker) {
			got, gotOff = assignment.Value, assignment.Offset
			if assignment.Unknown {
				got = "unknown"
			}
		}
	}
	if got != "unknown" {
		t.Fatalf("S at caller marker = %q, want unknown; assigns=%+v", got, assigns)
	}
}

func TestRoundSixFunctionRedirectHereDocIsVisibleInFunctionBody(t *testing.T) {
	const src = "f() { printf '%s\\n' \"$S\"; } <<EOF\n${ S=bad; }\nEOF\nS=good; f"
	marker := strings.Index(src, `printf '%s\n' "$S"`)
	assigns, err := Assigns(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, assignment := range assigns {
		if assignment.Name == "S" && assignment.VisibleAt(marker) {
			return
		}
	}
	t.Fatalf("function redirect here-doc write is not visible in body: %+v", assigns)
}

func TestFunctionSubshellHereDocWriteIsVisibleInsideBody(t *testing.T) {
	const marker = "printf '%s\\n' \"$S\""
	src := "f() ( S=good; : <<EOF\n${ S=bad; }\nEOF\n" + marker + " ); f"
	at := strings.Index(src, marker)
	assigns, err := Assigns(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, assignment := range assigns {
		if assignment.Name == "S" && assignment.VisibleAt(at) {
			return
		}
	}
	t.Fatalf("here-doc write was not visible inside function subshell: %+v", assigns)
}

func TestFunctionLocalShadowsItsHereDocWritesAtCall(t *testing.T) {
	const marker = "printf '%s\\n' \"$S\""
	src := "S=good; f() { local S; : <<EOF\n${ S=bad; }\nEOF\n}; f; " + marker
	call := strings.Index(src, "f; ")
	at := strings.LastIndex(src, marker)
	assigns, err := Assigns(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, assignment := range assigns {
		if assignment.Name == "S" && assignment.Unknown && assignment.Offset == call && assignment.VisibleAt(at) {
			return
		}
	}
	t.Fatalf("local S plus here-doc did not produce an unknown caller write: %+v", assigns)
}

func TestFunctionHereDocsFollowRedirectExecutionShell(t *testing.T) {
	for _, tc := range []struct {
		name, src   string
		wantUnknown bool
	}{
		{"subshell", "S=good; f() { ( : <<EOF ); }\n${ S=bad; }\nEOF\nf; : \"$S\"", false},
		{"background", "S=good; f() { : <<EOF & wait; }\n${ S=bad; }\nEOF\nf; : \"$S\"", false},
		{"pipeline", "S=good; f() { : <<EOF | cat; }\n${ S=bad; }\nEOF\nf; : \"$S\"", false},
		{"no command", "S=good; f() { <<EOF; }\n${ S=bad; }\nEOF\nf; : \"$S\"", false},
		{"assignment only", "S=good; f() { T=x <<EOF; }\n${ S=bad; }\nEOF\nf; : \"$S\"", false},
		{"nested function", "S=good; f() { g() { : <<EOF; }\n${ S=bad; }\nEOF\n}; f; : \"$S\"", false},
		{"redefined", "S=good; f() { : <<EOF; }\n${ S=bad; }\nEOF\nf() { :; }; f; : \"$S\"", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := strings.LastIndex(tc.src, `: "$S"`)
			call := strings.LastIndex(tc.src, "f;")
			assigns, err := Assigns(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, a := range assigns {
				if a.Name == "S" && a.Unknown && a.Offset == call && a.VisibleAt(marker) {
					found = true
				}
			}
			if found != tc.wantUnknown {
				t.Errorf("function call unknown write = %v, want %v; assigns=%+v", found, tc.wantUnknown, assigns)
			}
		})
	}
}

func TestFunctionBuiltinHereDocAssignmentsAffectFunctionShell(t *testing.T) {
	for _, src := range []string{
		"f() { S=good; : <<EOF\n${ S=bad; }\nEOF\nprintf '%s\\n' \"$S\"; }; f",
		"f() { S=good; local T=x <<EOF\n${ S=bad; }\nEOF\nprintf '%s\\n' \"$S\"; }; f",
	} {
		marker := strings.LastIndex(src, `printf '%s\n' "$S"`)
		assigns, err := Assigns(src)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, a := range assigns {
			if a.Name == "S" && a.VisibleAt(marker) {
				found = true
			}
		}
		if !found {
			t.Errorf("builtin here-doc assignment is not visible in function: %q; assigns=%+v", src, assigns)
		}
	}
}

func TestFunsubLocalScopeAtPipelineStart(t *testing.T) {
	const src = `S=good; ${ local S=bad; echo printf; } "%s\n" "$S" | cat; : "$S"`
	marker := strings.Index(src, `"$S"`)
	assigns, err := Assigns(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range assigns {
		if a.Name == "S" && a.Value == "bad" {
			if a.VisibleAt(marker) {
				t.Errorf("funsub-local assignment escaped at pipeline start: %+v", a)
			}
			return
		}
	}
	t.Fatalf("bad assignment not found: %+v", assigns)
}

func TestParentHereDocSearchRequiresMatchingCommand(t *testing.T) {
	const src = "S=good; ( : ); : <<EOF\n${ S=bad; }\nEOF\n: \"$S\""
	bad := strings.Index(src, "S=bad")
	assigns, err := Assigns(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range assigns {
		if a.Name == "S" && a.Offset == bad {
			if a.VisibleAt(len(src)) {
				return
			}
			t.Errorf("builtin here-doc assignment became isolated: %+v", a)
			return
		}
	}
	t.Fatalf("here-doc assignment not found: %+v", assigns)
}

func TestFunctionSubshellHereDocPreservesScopeMetadata(t *testing.T) {
	const src = "f() ( : <<EOF )\n${ S=bad; }\nEOF\n"
	body := strings.Index(src, "${ S=bad")
	scopes, err := Scopes(src)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, scope := range scopes {
		if scope.Start == body {
			if scope.Kind == ScopeFunctionBody {
				found = true
				if scope.Name != "f" || scope.DeclOffset != 0 || !scope.Isolated {
					t.Errorf("function subshell here-doc metadata = %+v", scope)
				}
			} else if scope.Kind == ScopeSubshell {
				t.Errorf("duplicate function-subshell here-doc range: %+v", scope)
			}
		}
	}
	if found {
		whole := "S=good; " + src + `printf '%s\n' "$S"`
		assigns, err := Assigns(whole)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range assigns {
			if a.Name == "S" && a.Value == "bad" && a.VisibleAt(len(whole)-len(`printf '%s\n' "$S"`)) {
				t.Errorf("function-subshell here-doc assignment escaped its scope: %+v", a)
			}
		}
		return
	}
	t.Errorf("no function-body scope covers here-doc body: %+v", scopes)
}

func TestHereDocRangesFollowStatementRedirects(t *testing.T) {
	for _, src := range []string{
		"S=good; : <<A | cat; : <<B\nx\nA\n${ S=bad; }\nB\n: \"$S\"",
		"S=good; : <<A | cat && : <<B\nx\nA\n${ S=bad; }\nB\n: \"$S\"",
	} {
		marker := strings.LastIndex(src, `: "$S"`)
		assigns, err := Assigns(src)
		if err != nil {
			t.Fatal(err)
		}
		got, offset := "", -1
		for _, a := range assigns {
			if a.Name == "S" && a.Offset < marker && a.Offset > offset && a.VisibleAt(marker) {
				got, offset = a.Value, a.Offset
				if a.Unknown {
					got = "unknown"
				}
			}
		}
		if got != "unknown" {
			t.Errorf("value at marker = %q, want Unknown; assigns=%+v", got, assigns)
		}
	}

	const src = "cat <<A | cat <<B\na\nA\nb\nB\n"
	bBody := strings.Index(src, "b\nB")
	scopes, err := Scopes(src)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, scope := range scopes {
		if scope.Kind == ScopePipePart && scope.Start == bBody {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("second pipeline here-doc has %d matching scopes, want 1: %+v", count, scopes)
	}
}

func TestHereDocRangesCoverNestedStatementSubtrees(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		kind      ScopeKind
	}{
		{"brace in pipeline", "{ cat <<EOF; } | cat\n${ S=bad; }\nEOF\n", ScopePipePart},
		{"brace in background", "{ : <<EOF; } &\n${ S=bad; }\nEOF\n", ScopeBackground},
		{"if in pipeline", "if true; then cat <<EOF; fi | cat\n${ S=bad; }\nEOF\n", ScopePipePart},
		{"funsub in pipeline", "x=${ : <<EOF; } | cat\n${ S=bad; }\nEOF\n", ScopePipePart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := strings.Index(tc.src, "S=bad")
			scopes, err := Scopes(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			for _, scope := range scopes {
				if scope.Kind == tc.kind && scope.Start <= bad && bad < scope.End {
					return
				}
			}
			t.Fatalf("no %s scope covers nested here-doc assignment at %d: %+v", tc.kind, bad, scopes)
		})
	}

	for _, src := range []string{
		"S=good; { cat <<EOF; } | cat >/dev/null\n${ S=bad; }\nEOF\n",
		"S=good; ( { cat <<EOF; } | cat >/dev/null )\n${ S=bad; }\nEOF\n",
	} {
		t.Run("no duplicate scopes", func(t *testing.T) {
			body := strings.Index(src, "${ S=bad; }")
			scopes, err := Scopes(src)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			seen := make(map[Scope]bool, len(scopes))
			for _, scope := range scopes {
				if seen[scope] {
					t.Fatalf("duplicate scope for brace pipeline here-doc: %+v", scope)
				}
				seen[scope] = true
				if scope.Kind == ScopePipePart && scope.Start == body {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("brace pipeline here-doc has %d matching PipePart scopes, want one: %+v", count, scopes)
			}
		})
	}
}

func TestTopLevelHereDocAfterFunctionIsNotInFunctionScope(t *testing.T) {
	const src = "f() { :; }\n: <<EOF\nx\nEOF\n"
	docBody := strings.Index(src, "x\nEOF")
	scopes, err := Scopes(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range scopes {
		if scope.Kind == ScopeFunctionBody && scope.Start == docBody {
			t.Fatalf("top-level here-doc was assigned to the preceding function: %+v", scopes)
		}
	}
}

func TestNestedFunctionDocAssignmentAfterInnerDocument(t *testing.T) {
	for _, src := range []string{
		"S=good; f() { printf '%s\\n' \"$S\"; } <<EOF\n${ g() { :; } <<EOG\nx\nEOG\nS=bad; }\nEOF\nf",
		"S=good; f() { printf '%s\\n' \"$S\"; } <<EOF\n${ S=bad; g() { :; } <<EOG\nx\nEOG\n}\nEOF\nf",
	} {
		marker := strings.Index(src, `"$S"`)
		assigns, err := Assigns(src)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, a := range assigns {
			if a.Name == "S" && a.VisibleAt(marker) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("outer function-definition here-doc assignment was not visible: %+v", assigns)
		}
	}
}

func TestFunctionScopeIndexUsesHalfOpenEnds(t *testing.T) {
	index := newSpanIndex([]span{{2, 5}})
	if index.contains(5) {
		t.Fatal("span contains its exclusive end")
	}
	if start, end := index.innermost(5); start != -1 || end != -1 {
		t.Fatalf("innermost at exclusive end = [%d,%d), want no span", start, end)
	}
}

func TestNestedBackgroundAssignmentRemainsVisibleAtSubshellEnd(t *testing.T) {
	const src = `S=good; ( :&S=bad; wait; printf '%s\n' "$S" )`
	marker := strings.LastIndex(src, `"$S"`)
	assigns, err := Assigns(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range assigns {
		if a.Name == "S" && a.Value == "bad" && a.VisibleAt(marker) {
			return
		}
	}
	t.Fatalf("background assignment was not visible before the subshell's exclusive end: %+v", assigns)
}

func TestScopesMatchAssignBoundsAndOrder(t *testing.T) {
	for _, src := range []string{
		`f() { S=1; }; f`, `f() ( S=1 ); f`, `(S=1)`, `echo $(S=1)`, "echo `S=1`", `cat <(S=1)`,
		`coproc { S=1; }`, `a | S=1 | b`, `S=1 &`, `a | b &`, `function g { S=1; }`, `f() { ( x=$(S=1) ) | S=2 & }`,
	} {
		assigns, err := Assigns(src)
		if err != nil {
			t.Fatal(err)
		}
		scopes, err := Scopes(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, assign := range assigns {
			if assign.ScopeStart < 0 {
				continue
			}
			if !slices.ContainsFunc(scopes, func(scope Scope) bool { return scope.Start == assign.ScopeStart && scope.End == assign.ScopeEnd }) {
				t.Errorf("%q: Assign %s bounds [%d,%d) absent in Scopes %+v", src, assign.Name, assign.ScopeStart, assign.ScopeEnd, scopes)
			}
		}
		for i := 1; i < len(scopes); i++ {
			prev, curr := scopes[i-1], scopes[i]
			if curr.Start < prev.Start || curr.Start == prev.Start && curr.End > prev.End {
				t.Errorf("%q: scopes not sorted outer first: %+v", src, scopes)
			}
		}
	}
	for kind, name := range map[ScopeKind]string{
		ScopeFunctionBody: "FunctionBody", ScopeSubshell: "Subshell", ScopeCmdSubst: "CmdSubst", ScopeProcSubst: "ProcSubst",
		ScopeCoproc: "Coproc", ScopePipePart: "PipePart", ScopeBackground: "Background",
	} {
		if got := fmt.Sprint(kind); got != name {
			t.Errorf("ScopeKind(%d).String() = %q, want %q", kind, got, name)
		}
	}
}

func TestScopesParseError(t *testing.T) {
	if scopes, err := Scopes(`f() {`); err == nil || scopes != nil {
		t.Errorf("Scopes of malformed command = %v, %v; want nil and a parse error", scopes, err)
	}
}

func TestFunctionScopesExcludeDefinitionRedirections(t *testing.T) {
	for _, tc := range []struct {
		src  string
		body string
	}{
		{`f() ( S=1 ) >/dev/null`, `( S=1 )`},
		{`f() ( S=1 ) 2>&1; f`, `( S=1 )`},
		{`f() { S=1; } >/dev/null`, `{ S=1; }`},
	} {
		start := strings.Index(tc.src, tc.body)
		end := start + len(tc.body)
		scopes, err := Scopes(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		if len(scopes) != 1 || scopes[0].Kind != ScopeFunctionBody || scopes[0].Start != start || scopes[0].End != end {
			t.Errorf("Scopes(%q) = %+v, want function body [%d,%d)", tc.src, scopes, start, end)
		}
		assigns, err := Assigns(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, assign := range assigns {
			if assign.Name == "S" && assign.Value == "1" {
				found = true
				if assign.ScopeStart != start || assign.ScopeEnd != end {
					t.Errorf("Assign in %q has scope [%d,%d), want [%d,%d)", tc.src, assign.ScopeStart, assign.ScopeEnd, start, end)
				}
			}
		}
		if !found {
			t.Errorf("Assigns(%q) has no S=1: %+v", tc.src, assigns)
		}
	}
}

func TestFunctionBodyAssignmentIsNotVisibleInDefinitionRedirection(t *testing.T) {
	for _, body := range []string{`( S=1; cat )`, `{ S=1; cat; }`} {
		src := `S=clean; f() ` + body + ` <<<"$S"; f`
		off := strings.Index(src, `"$S"`) + 1
		assigns, err := Assigns(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, assign := range assigns {
			if assign.Name == "S" && assign.Value == "1" && assign.VisibleAt(off) {
				t.Errorf("%q: body assignment is visible in the definition redirection: %+v", src, assign)
			}
		}
	}
}

func TestFunctionWritesRemainVisibleToCallWithDefinitionRedirection(t *testing.T) {
	src := `S=clean; f() { S=1; } <<<"$S"; f`
	assigns, err := Assigns(src)
	if err != nil {
		t.Fatal(err)
	}
	call := strings.LastIndex(src, "f")
	for _, assign := range assigns {
		if assign.Name == "S" && assign.Unknown && assign.Offset == call && assign.VisibleAt(len(src)+1) {
			return
		}
	}
	t.Errorf("Assigns(%q) = %+v, want visible Unknown S at the function call", src, assigns)
}
