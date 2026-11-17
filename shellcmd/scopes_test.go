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
