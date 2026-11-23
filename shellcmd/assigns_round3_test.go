package shellcmd

import (
	"strings"
	"testing"
)

func TestAssignsRound3UnknownTriggers(t *testing.T) {
	tests := []struct{ name, src, variable string }{
		{"trap stays set after reset", `S=/good; eval 'S=/bad'; trap - EXIT; echo "$S/ref"`, "S"},
		{"trap stays set after print", `S=/good; . ./env.sh; trap -p; echo "$S/ref"`, "S"},
		{"trap double dash action", `S=/good; trap -- 'S=/bad' DEBUG; echo "$S/ref"`, "S"},
		{"alias stays set after bare trap", "shopt -s expand_aliases; alias a='S=/bad'\nS=/good; a; trap; echo \"$S/ref\"", "S"},
		{"callback stays set after reset", `S=/good; mapfile -C 'S=/bad; :' -c 1 A <<<x; trap - EXIT; echo "$S/ref"`, "S"},
		{"elif condition", `S=/good; if true; then :; elif S=/bad; then :; fi; echo "$S/ref"`, "S"},
		{"elif condition after elif", `S=/good; if true; then :; elif S=/bad; true; then :; fi; echo "$S/ref"`, "S"},
		{"while condition", `S=/good; i=0; while S=/bad; [ $i -lt 1 ]; do i=1; done; echo "$S/ref"`, "S"},
		{"until condition", `S=/good; until S=/bad; false; do :; done; echo "$S/ref"`, "S"},
		{"mapfile n target", `S=/good; mapfile -n 1 S <<<'/bad'; echo "$S/ref"`, "S"},
		{"mapfile delimiter target", `S=/good; mapfile -t -d , S <<<'/bad'; echo "$S/ref"`, "S"},
		{"readarray fd target", `S=/good; readarray -u 0 S <<<'/bad'; echo "$S/ref"`, "S"},
		{"mapfile offset target", `S=/good; mapfile -O 0 -t S <<<x; echo "$S/ref"`, "S"},
		{"command declare nameref", `S=/good; command declare -n R=S; R=/bad; echo "$S/ref"`, "S"},
		{"command declare", `S=/good; command declare S=/bad; echo "$S/ref"`, "S"},
		{"builtin export", `S=/good; builtin export S=/bad; echo "$S/ref"`, "S"},
		{"quoted export", `S=/good; export "S=/bad"; echo "$S/ref"`, "S"},
		{"dynamic eval", `c=eval; S=/good; $c 'S=/bad'; echo "$S/ref"`, "S"},
		{"dynamic source", `c=source; S=/good; $c ./e.sh; echo "$S/ref"`, "S"},
		{"function reference before later write", `S=/tmp/a; post() { echo "$S/ref"; }; S=/tmp/b; post`, "S"},
		{"callee writes before function reference", `S=/good; f() { echo "$S/ref"; }; g() { S=/bad; }; g; f`, "S"},
		{"printf attached target", `S=/good; printf -vS %s /bad; echo "$S/ref"`, "S"},
		{"printf dynamic target", `v=S; S=/good; printf -v "$v" %s /bad; echo "$S/ref"`, "S"},
		{"read dynamic target", `v=S; S=/good; read -r "$v" <<<'/bad'; echo "$S/ref"`, "S"},
		{"case transform upper", `S=/good; declare -u S; S=/bad; echo "$S/ref"`, "S"},
		{"case transform lower", `declare -l S=/BAD; echo "$S/ref"`, "S"},
		{"readonly later assignment", `readonly S=/good; S=/bad 2>/dev/null; echo "$S/ref"`, "S"},
		{"unset", `S=/good; unset S; echo "$S/ref"`, "S"},
		{"wait p", `S=/good; true & wait -n -p S; echo "$S/ref"`, "S"},
		{"prefix export", `S=/good; S=/bad export S; echo "$S/ref"`, "S"},
		{"prefix declaration", `S=/good; S=/bad declare S; echo "$S/ref"`, "S"},
		{"prefix readonly", `S=/good; S=/bad readonly S; echo "$S/ref"`, "S"},
		{"prefix special builtin in POSIX", `set -o posix; S=/good; S=/bad :; echo "$S/ref"`, "S"},
		{"read target in arithmetic", `S=/good; read y <<<'S=7'; (( y )); echo "$S/ref"`, "S"},
		{"unset arithmetic index", `a=(1); S=/good; unset 'a[S=7]'; echo "$S/ref"`, "S"},
		{"printf arithmetic index", `S=/good; printf -v 'a[S=7]' x; echo "$S/ref"`, "S"},
		{"integer declaration after value", `S=/good; f() { n='S=7'; }; declare -i n; f; echo "$S/ref"`, "S"},
		{"conditional parameter funsub", `X=1; S=/good; : ${X:-${ S=/bad; }}; echo "$S/ref"`, "S"},
		{"alternate parameter funsub", `X=; S=/good; : ${X:+${ S=/bad; }}; echo "$S/ref"`, "S"},
		{"case pattern funsub", `S=/good; case a in a) ;; "${ S=/bad; }") ;; esac; echo "$S/ref"`, "S"},
		{"function body write", `S=/good; g() { S=/bad; }; g; echo "$S/ref"`, "S"},
		{"nested shell through sh", `S=/good; sh -ec 'S=/bad'; echo "$S/ref"`, "S"},
		{"nested shell parse error", `S=/good; bash -c 'if'; echo "$S/ref"`, "S"},
		{"nested shell heredoc parse error", "S=/good; bash <<'X'\nif\nX\necho \"$S/ref\"", "S"},
		{"coprocess name", `S=/good; coproc S { :; }; echo "$S/ref"`, "S"},
		{"exec fd input", `S=/good; exec {S}</dev/null; echo "$S/ref"`, "S"},
		{"fd redirect with command", `S=/good; : {S}>/dev/null; echo "$S/ref"`, "S"},
		{"same line after heredoc opener", "S=/good; cat <<X; S=/bad\nSENT $S/ref\nX", "S"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assertUnknownAtReference(t, tt.src, tt.variable) })
	}
}

func TestAssignsRound3NestedFunctionReadWithOneWriteStaysExact(t *testing.T) {
	assertKnownAtReference(t, `S=/good; f() { echo "$S/ref"; }; f`, "S", "/good")
}

func TestAssignsRound3SafeNeighborsStayExact(t *testing.T) {
	for _, src := range []string{
		`S=/good; trap -p; echo "$S/ref"`,
		`S=/good; trap -- - EXIT; echo "$S/ref"`,
		`S=/good; trap -l >/dev/null; echo "$S/ref"`,
		`S=/good; trap '' EXIT; echo "$S/ref"`,
		`S=/good; mapfile -n 1 T <<<'x'; echo "$S/ref"`,
		`S=/good; mapfile -d , T <<<'x'; echo "$S/ref"`,
		`S=/good; read -r T <<<'x'; echo "$S/ref"`,
		`S=/good; echo bash -c 'S=/bad'; echo "$S/ref"`,
		`S=/good; S=/bad :; echo "$S/ref"`,
		`S=/good; :; echo "$S/ref"`,
		"S=/good; bash script.sh <<'X'\nS=/bad\nX\necho \"$S/ref\"",
	} {
		assertKnownAtReference(t, src, "S", "/good")
	}
	assertKnownAtReference(t, "S=/good; cat <<X\nbody\nX\nS=/bad; echo \"$S/ref\"", "S", "/bad")
}

func assertUnknownAtReference(t *testing.T, src, name string) {
	t.Helper()
	assigns, err := Assigns(src)
	if err != nil {
		t.Fatalf("Assigns(%q): %v", src, err)
	}
	ref := strings.LastIndex(src, "$"+name+"/ref")
	if ref < 0 {
		t.Fatalf("reference to %s missing in %q", name, src)
	}
	var last *Assign
	for i := range assigns {
		if assigns[i].Name == name && assigns[i].VisibleAt(ref) {
			last = &assigns[i]
		}
	}
	if last == nil {
		t.Fatalf("no visible %s assignment at reference; got %+v", name, assigns)
	}
	if !last.Unknown || last.Value != "" || last.Append {
		t.Errorf("assignment at reference = %+v, want Unknown with empty value and no append", *last)
	}
}

func assertKnownAtReference(t *testing.T, src, name, want string) {
	t.Helper()
	assigns, err := Assigns(src)
	if err != nil {
		t.Fatalf("Assigns(%q): %v", src, err)
	}
	ref := strings.LastIndex(src, "$"+name+"/ref")
	if ref < 0 {
		t.Fatalf("reference to %s missing in %q", name, src)
	}
	var last *Assign
	for i := range assigns {
		if assigns[i].Name == name && assigns[i].VisibleAt(ref) {
			last = &assigns[i]
		}
	}
	if last == nil || last.Unknown || last.Value != want || last.Append {
		t.Errorf("assignment at reference = %+v, want known %s=%q", last, name, want)
	}
}
