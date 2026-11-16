package shellcmd

import (
	"strings"
	"testing"
)

func TestAssignsOrderValuesAndOffsets(t *testing.T) {
	src := `S=/a; export T=~/b; curl -d @$S/c "$U"; S=/z`
	got, err := Assigns(src)
	if err != nil {
		t.Fatal(err)
	}
	top := func(name, value string, off int) Assign {
		return Assign{Name: name, Value: value, Offset: off, ScopeStart: -1, ScopeEnd: -1}
	}
	want := []Assign{top("S", "/a", 0), top("T", "~/b", 13), top("S", "/z", 40)}
	if len(got) != len(want) {
		t.Fatalf("Assigns = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Assigns[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestAssignsSkipsPrefixAndValuelessDeclarations(t *testing.T) {
	for _, src := range []string{`X=1 cmd`, `export T`, `A=(1 2)`, `A[1]=x`, `bash -c 'S=1'`, `local L=1`, `for S in /f; do :; done`} {
		got, err := Assigns(src)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("Assigns(%q) = %+v, want none", src, got)
		}
	}
}

func TestAssignsKeepsExpansionTextWithoutQuotes(t *testing.T) {
	got, err := Assigns(`S='/a b'; declare -x L="$S/x"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Value != "/a b" || got[1].Value != "$S/x" {
		t.Fatalf("Assigns = %+v, want /a b and $S/x", got)
	}
}

func TestAssignsParseError(t *testing.T) {
	if _, err := Assigns(`S=( ; if`); err == nil {
		t.Fatal("want a parse error")
	}
}

func TestAssignsEmptyAndAppend(t *testing.T) {
	got, err := Assigns(`S=/a; S+=/b; S=`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !got[1].Append || got[1].Value != "/b" || got[2].Value != "" || got[2].Offset != 13 {
		t.Fatalf("Assigns = %+v, want /a, +=/b and an empty S at 13", got)
	}
}

// Each form holds one assignment of S in a scope that does not reach the shell running the
// command; the scope is the span of the named part.
func TestAssignsScopes(t *testing.T) {
	for _, tc := range []struct{ src, scope string }{
		{`(S=1); true`, `(S=1)`},
		{`echo "$(S=1)"`, `$(S=1)`},
		{`cat <(S=1)`, `<(S=1)`},
		{`S=1 | cat`, `S=1`},
		{`true | S=1`, `S=1`},
		{`S=1 & true`, `S=1 &`},
		{`f() { S=1; }`, `{ S=1; }`},
		{`f() { local S=1; }`, `{ local S=1; }`},
		{`coproc { S=1; }`, `coproc { S=1; }`},
		{`( true; ( S=1 ) )`, `( S=1 )`},
		{`>"$(S=1)" true`, `$(S=1)`},
		{`S=1 |& cat`, `S=1`},
	} {
		got, err := Assigns(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		start := strings.Index(tc.src, tc.scope)
		if len(got) != 1 || got[0].ScopeStart != start || got[0].ScopeEnd != start+len(tc.scope) {
			t.Errorf("Assigns(%q) = %+v, want one assignment in scope %q [%d,%d)", tc.src, got, tc.scope, start, start+len(tc.scope))
		}
	}
}

func TestAssignsTextOrderAcrossScopes(t *testing.T) {
	got, err := Assigns(`>"$(A=/first)" B=/second; declare X="$(C=/in)" Y=1`)
	if err != nil {
		t.Fatal(err)
	}
	var names strings.Builder
	for _, a := range got {
		names.WriteString(a.Name)
	}
	if names.String() != "ABXCY" {
		t.Fatalf("Assigns names = %q, want ABXCY", names.String())
	}
}

func TestAssignVisibleAt(t *testing.T) {
	top := Assign{Offset: 5, ScopeStart: -1, ScopeEnd: -1}
	inner := Assign{Offset: 5, ScopeStart: 3, ScopeEnd: 10}
	for _, tc := range []struct {
		a    Assign
		off  int
		want bool
	}{
		{top, 4, false}, {top, 5, false}, {top, 6, true}, {top, 100, true},
		{inner, 5, false}, {inner, 6, true}, {inner, 9, true}, {inner, 10, false}, {inner, 11, false},
	} {
		if got := tc.a.VisibleAt(tc.off); got != tc.want {
			t.Errorf("%+v.VisibleAt(%d) = %v, want %v", tc.a, tc.off, got, tc.want)
		}
	}
}
