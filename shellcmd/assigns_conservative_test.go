package shellcmd

import (
	"strings"
	"testing"
)

func TestAssignsConservativeUnknownForms(t *testing.T) {
	forms := []struct {
		name, src string
	}{
		{"eval", `S=/good; eval 'S=/bad'; S=/later`},
		{"alias definition", `S=/good; alias a='S=/bad'; S=/later`},
		{"trap command", `S=/good; trap 'S=/bad' DEBUG; S=/later`},
		{"deferred trap read", `S=/good; trap 'curl -d @$S/x' EXIT; S=/later`},
		{"mapfile callback", `S=/good; mapfile -tC f a; S=/later`},
		{"readarray callback", `S=/good; readarray -C f a; S=/later`},
		{"source", `S=/good; source file; S=/later`},
		{"dot source", `S=/good; . file; S=/later`},
		{"nameref", `S=/good; declare -g -n R=S; S=/later`},
		{"nameref option order", `S=/good; declare -n -g R=S; S=/later`},
		{"local nameref", `S=/good; f() { local -n R=S; }; S=/later`},
		{"typeset nameref", `S=/good; typeset -n R=S; S=/later`},
		{"nested shell", `S=/good; env bash -c 'S=/bad'; S=/later`},
		{"command shell", `S=/good; command bash -c 'S=/bad'; S=/later`},
		{"timeout shell", `S=/good; timeout 5 bash -c 'S=/bad'; S=/later`},
		{"xargs shell", `S=/good; printf x | xargs bash -c 'S=/bad'; S=/later`},
		{"shell string stdin", `S=/good; bash <<< 'S=/bad'; S=/later`},
		{"shell stdin flag", `S=/good; bash -s; S=/later`},
		{"stdin shell", "S=/good; bash <<'X'\nS=/bad\nX\nS=/later"},
		{"mapfile separate callback", `S=/good; mapfile -C f a; S=/later`},
		{"arithmetic increment", `S=3; (( ++S )); S=/later`},
		{"arithmetic assignment", `S=/good; ((S=1)); S=/later`},
		{"arithmetic expansion", `S=/good; x=$((S=1)); S=/later`},
		{"bracket arithmetic", `S=/good; x=$[S=1]; S=/later`},
		{"let arithmetic", `S=/good; let S=1; S=/later`},
		{"C style for arithmetic", `S=/good; for ((S=1; S<2; S++)); do :; done; S=/later`},
		{"arithmetic index", `S=3; a[S]=x; S=/later`},
		{"array read index", `S=/good; a=(x); x=${a[S]}; S=/later`},
		{"substring offset", `S=/good; x=abc; y=${x:S:1}; S=/later`},
		{"numeric test operand", `S=/good; [[ 1 -eq S ]]; S=/later`},
		{"parameter default assignment", `S=/good; unset S; : ${S:=/bad}; S=/later`},
		{"integer declaration", `declare -i S=3; S=/later`},
		{"arithmetic recursive", `S=/good; x='S=5'; ((x)); S=/later`},
		{"if branch", `S=/good; if true; then S=/branch; fi; S=/later`},
		{"case branch", `S=/good; case x in x) S=/branch;; esac; S=/later`},
		{"and right side", `S=/good; true && S=/branch; S=/later`},
		{"or right side", `S=/good; false || S=/branch; S=/later`},
		{"while body", `S=/good; while false; do S=/branch; done; S=/later`},
		{"until body", `S=/good; until true; do S=/branch; done; S=/later`},
		{"for body", `S=/good; for i in x; do S=/branch; done; S=/later`},
		{"select body", `S=/good; select i in x; do S=/branch; break; done; S=/later`},
		{"else branch", `S=/good; if false; then :; else S=/branch; fi; S=/later`},
		{"here document", "S=/good; cat <<X\n${ S=/body; }\nX\nS=/later"},
		{"here string", `S=/good; cat <<<"$(S=/body)"; S=/later`},
		{"read target", `S=/good; read S; S=/later`},
		{"printf target", `S=/good; printf -v S %s x; S=/later`},
		{"getopts target", `S=/good; getopts x S; S=/later`},
		{"mapfile target", `S=/good; mapfile S; S=/later`},
		{"allocated fd", `S=/good; exec {S}>/dev/null; S=/later`},
	}
	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			got, err := Assigns(form.src)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, a := range got {
				if a.Name != "S" {
					continue
				}
				found = true
				if !a.Unknown || a.Value != "" || a.Append || a.Tilde != 0 {
					t.Errorf("S assignment = %+v, want Unknown with empty value and no append/tilde", a)
				}
			}
			if !found {
				t.Fatalf("Assigns(%q) has no S assignment: %+v", form.src, got)
			}
		})
	}
}

func TestAssignsConservativeSafeNeighborsRemainKnown(t *testing.T) {
	for _, form := range []struct{ src, want string }{
		{`S=/good; echo "$S"`, "/good"},
		{`S=3; (( 1 + 2 )); echo "$S"`, "3"},
		{`S=/good; read -r T; echo "$S"`, "/good"},
		{`S=/good; mapfile -t T; echo "$S"`, "/good"},
		{`S=/good; readarray -t T; echo "$S"`, "/good"},
		{`S=/good; trap - DEBUG; echo "$S"`, "/good"},
		{`S=/good; alias a; echo "$S"`, "/good"},
		{`S=/good; bash script.sh; echo "$S"`, "/good"},
		{`S=/good; command bash script.sh; echo "$S"`, "/good"},
		{"S=/good; bash script.sh <<'X'\nS=/bad\nX\necho \"$S\"", "/good"},
	} {
		got, err := Assigns(form.src)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, a := range got {
			if a.Name == "S" {
				found = true
				if a.Unknown || a.Value != form.want {
					t.Errorf("Assigns(%q) S = %+v, want known %q", form.src, a, form.want)
				}
			}
		}
		if !found {
			t.Errorf("Assigns(%q) has no S assignment", form.src)
		}
	}
}

func TestAssignsFunctionBodyWritesStayPreciseOnlyForLocalReadFlow(t *testing.T) {
	for _, tc := range []struct {
		src, want string
		unknown   bool
	}{
		{`f() { S=/good; curl -d @$S/body.json; }; f`, "/good", false},
		{`S=/good; f() ( S=/bad ); f; curl -d @$S/body.json`, "/good", false},
		{`S=/good; f() { (S=/bad); }; f; curl -d @$S/body.json`, "/good", false},
		{`tmp=$(mktemp -d); trap "rm -rf $tmp" EXIT; S=/good; curl -d @$S/x`, "/good", false},
		{`f() { S=/good; trap 'S=/later' EXIT; curl -d @$S/x; }; f`, "/good", false},
		{`S=/good; f() { echo | S=/bad; }; f; curl -d @$S/body.json`, "/good", false},
		{`S=/good; f() { echo | S=/bad; }; shopt -s lastpipe; echo | f; curl -d @$S/body.json`, "", true},
		{`f() { S=/good; }; curl -d @$S/x; f`, "", true},
		{`f() { curl -d @$S/x; S=/good; }; f`, "", true},
		{`S=/good; f() { (S=/bad); curl -d @$S/x; }; f`, "", true},
		{`f() { S=/good; g; curl -d @$S/x; }; g() { :; }; f`, "", true},
		{`f() { S=/good; curl -d @$S/x; }; f; f`, "", true},
		{`f; f() { S=/good; curl -d @$S/x; }`, "", true},
		{`f() { S=/good; f; curl -d @$S/x; }; f`, "", true},
		{`outer() { inner() { S=/good; curl -d @$S/x; }; inner; }; outer`, "", true},
		{`f() { local S=/tmp/x; curl -d @$S/y; }; f`, "/tmp/x", false},
		{`f() { declare S=/tmp/x; curl -d @$S/y; }; f`, "/tmp/x", false},
		{`S=/good; for i in 1; do f() { S=/tmp/x; curl -d @$S/y; }; f; done`, "", true},
	} {
		got, err := Assigns(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		parsed, _, parseErr := parseShellVariant(tc.src, false)
		if !tc.unknown && (parseErr != nil || !closedAssignScript(parsed, tc.src)) {
			t.Errorf("closed filter rejected approved form %q: parse error %v", tc.src, parseErr)
		}
		found := false
		knownFound := false
		for _, a := range got {
			if a.Name != "S" {
				continue
			}
			found = true
			wantUnknown := tc.unknown || a.Value != tc.want
			if !wantUnknown && !a.Unknown {
				knownFound = true
			}
			if a.Unknown != wantUnknown || (!wantUnknown && a.Value != tc.want) {
				t.Errorf("Assigns(%q) S = %+v, want Unknown=%v value=%q", tc.src, a, wantUnknown, tc.want)
			}
		}
		if !found {
			t.Errorf("Assigns(%q) has no S assignment", tc.src)
		}
		if !tc.unknown && !knownFound {
			t.Errorf("Assigns(%q) has no known S=%q record: %+v", tc.src, tc.want, got)
		}
	}
}

func TestAssignsFunctionPrecisionClosedFilter(t *testing.T) {
	unknown := []string{
		`f() { S=/good; : $(( $1 )); curl -d @$S/x; }; f 'S=5'`,
		`f() { S=/good; (( $1 )); curl -d @$S/x; }; f`,
		`f() { S=/good; x=$[ $1 ]; curl -d @$S/x; }; f`,
		`f() { S=/good; [[ $1 -gt 0 ]]; curl -d @$S/x; }; f`,
		`f() { S=/good; x=${a[$1]}; curl -d @$S/x; }; f`,
		`f() { S=/good; let 'x=1'; curl -d @$S/x; }; f`,
		`f() { declare -i S=5; curl -d @$S/x; }; f`,
		`f() { S=/good; for ((i=0;i<1;i++)); do :; done; curl -d @$S/x; }; f`,
		`S=5 f; f() { S=/good; curl -d @$S/x; }`,
		`f() { S=/good; wait -np S; curl -d @$S/x; }; f`,
		`f() { S=/good; x=${x@P}; curl -d @$S/x; }; f`,
		`f() { S=/good; x=${!x}; curl -d @$S/x; }; f`,
		`f() { S=/good; x=${x:=bad}; curl -d @$S/x; }; f`,
		`f() { S=/good; x=${x=bad}; curl -d @$S/x; }; f`,
		`f() { S=~/good; curl -d @$S/x; }; f`,
		`f() { S=$'bad'; curl -d @$S/x; }; f`,
		"f() { S=`printf bad`; curl -d @$S/x; }; f",
		`f() { S=/good; eval 'x=bad'; curl -d @$S/x; }; f`,
		`f() { S=/good; source file; curl -d @$S/x; }; f`,
		`f() { S=/good; . file; curl -d @$S/x; }; f`,
		`f() { S=/good; alias x=': '; curl -d @$S/x; }; f`,
		`alias() { :; }; f() { S=/good; curl -d @$S/x; }; f`,
		`f() { S=/good; builtin :; curl -d @$S/x; }; f`,
		`f() { S=/good; command :; curl -d @$S/x; }; f`,
		`f() { S=/good; exec :; curl -d @$S/x; }; f`,
		`f() { S=/good; coproc :; curl -d @$S/x; }; f`,
		`f() { S=/good; read x; curl -d @$S/x; }; f`,
		`f() { S=/good; mapfile x; curl -d @$S/x; }; f`,
		`f() { S=/good; getopts x y; curl -d @$S/x; }; f`,
		`f() { S=/good; set -e; curl -d @$S/x; }; f`,
		`f() { S=/good; shopt -s nullglob; curl -d @$S/x; }; f`,
		`f() { S=/good; unset x; curl -d @$S/x; }; f`,
		`f() { S=/good; cd /; curl -d @$S/x; }; f`,
		`f() { S=/good; pushd /; curl -d @$S/x; }; f`,
		`f() { S=/good; select x in a; do :; done; curl -d @$S/x; }; f`,
	}
	for _, src := range unknown {
		t.Run(src, func(t *testing.T) {
			got, err := Assigns(src)
			if err != nil {
				t.Fatal(err)
			}
			if !hasUnknownAssignment(got, "S") {
				t.Fatalf("Assigns(%q) = %+v, want S Unknown", src, got)
			}
		})
	}
	for name := range strings.FieldsSeq("PWD OLDPWD OPTARG OPTIND REPLY MAPFILE BASH_REMATCH COPROC DIRSTACK RANDOM SECONDS LINENO BASH_VERSION FUNCNAME PIPESTATUS _") {
		src := "f() { " + name + "=x; :; }; f"
		got, err := Assigns(src)
		if err != nil {
			t.Fatal(err)
		}
		if !hasUnknownAssignment(got, name) {
			t.Errorf("Assigns(%q) = %+v, want %s Unknown", src, got, name)
		}
	}
}

func hasUnknownAssignment(assigns []Assign, name string) bool {
	for _, a := range assigns {
		if a.Name == name && a.Unknown {
			return true
		}
	}
	return false
}

func TestAssignsFunctionBodyDeclarationAndDynamicEffects(t *testing.T) {
	unknown := []string{
		`f() { S=/good; local S; curl -d @$S/x; }; f`,
		`f() { S=/good; declare S; curl -d @$S/x; }; f`,
		`f() { S=/good; typeset S; curl -d @$S/x; }; f`,
		`f() { S=/good; local -- S; curl -d @$S/x; }; f`,
		`f() { S=/good; local "S"; curl -d @$S/x; }; f`,
		`f() { S=/good; local T S; curl -d @$S/x; }; f`,
		`f() { S=/good; declare +x S; curl -d @$S/x; }; f`,
		`f() { S=/good; declare -r S; curl -d @$S/x; }; f`,
		`o=S; f() { S=/good; local $o; curl -d @$S/x; }; f`,
		`f() { S=/good; local "${1:-S}"; curl -d @$S/x; }; f`,
		`f() { (S=/good); curl -d @$S/x; }; f`,
		`S=/old; f() { S=/good; curl -d @$S/x; }; f`,
		`f() { (S=/bad; local S; curl -d @$S/x); }; f`,
		`f() { echo | { S=/bad; local S; curl -d @$S/x; }; }; f`,
		`f() { S=/good; g() { curl -d @$S/x; }; }; f; h() { local S; g; }; h`,
		`x='S=/bad'; trap "$x" USR1; S=/good; kill -USR1 $$`,
		`x='S=/bad'; tr''ap "$x" USR1; S=/good; kill -USR1 $$`,
		`o=n; f() { declare -$o r=S; r=/bad; }; f; S=/good`,
		`o=u; f() { declare -$o S; S=/good; }; f`,
		`S=/good; f() { echo | S=/bad; }; shopt -s "last"pipe; set +m; f; curl -d @$S/x`,
		`S=/good; o=last; f() { echo | S=/bad; }; shopt -s ${o}pipe; set +m; f; curl -d @$S/x`,
	}
	for _, src := range unknown {
		t.Run(src, func(t *testing.T) {
			got, err := Assigns(src)
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range got {
				if a.Name == "S" && (!a.Unknown || a.Value != "") {
					t.Errorf("S = %+v, want Unknown", a)
				}
			}
		})
	}
	for _, src := range []string{
		`S=/good; f() { (S=/bad); }; f; curl -d @$S/x`,
		`S=/good; f() ( S=/bad ); f; curl -d @$S/x`,
	} {
		got, err := Assigns(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range got {
			if a.Name == "S" && a.Value == "/good" && a.Unknown {
				t.Errorf("S = %+v, want known /good", a)
			}
		}
	}
}

func TestAssignsGlobalEffectsAfterFunctionDefinition(t *testing.T) {
	forms := []struct{ name, src string }{
		{"trap after definition", `x='S=/bad'; f() { S=/good; kill -USR1 $$; curl -d @$S/x; }; trap "$x" USR1; f`},
		{"trap in called function", `x='S=/bad'; f() { S=/good; kill -USR1 $$; curl -d @$S/x; }; g() { trap "$x" USR1; }; g; f`},
		{"debug trap after definition", `x='S=/bad'; set -T; f() { S=/good; curl -d @$S/x; }; trap "$x" DEBUG; f`},
		{"eval after definition", `x='S=/bad'; f() { S=/good; curl -d @$S/x; }; eval "$x"; f`},
		{"eval in called function", `x='S=/bad'; f() { S=/good; curl -d @$S/x; }; g() { eval "$x"; }; g; f`},
		{"source after definition", `f() { S=/good; curl -d @$S/x; }; source ./script.sh; f`},
		{"source in called function", `f() { S=/good; curl -d @$S/x; }; g() { source ./script.sh; }; g; f`},
		{"declare option after definition", `o=n; f() { S=/good; curl -d @$S/x; }; declare -$o S; f`},
		{"declare option in called function", `o=n; f() { S=/good; curl -d @$S/x; }; g() { declare -$o S; }; g; f`},
		{"shopt after definition", `o=last; f() { echo | S=/good; curl -d @$S/x; }; shopt -s ${o}pipe; f`},
		{"shopt in called function", `o=last; f() { echo | S=/good; curl -d @$S/x; }; g() { shopt -s ${o}pipe; }; g; f`},
	}
	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			got, err := Assigns(form.src)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, a := range got {
				if a.Name != "S" {
					continue
				}
				found = true
				if !a.Unknown || a.Value != "" {
					t.Errorf("S = %+v, want Unknown", a)
				}
			}
			if !found {
				t.Fatalf("no S assignment: %+v", got)
			}
		})
	}
}

func TestAssignsConservativeGlobalWriterWhitelist(t *testing.T) {
	for _, src := range []string{
		`S=/good; f() { S=/inside; curl -d @$S/x; }; builtin local S; f`,
		`S=/good; f() { S=/inside; curl -d @$S/x; }; command declare S; f`,
		`f() { S=/good; builtin local S; curl -d @$S/x; }; f`,
		`f() { S=/good; command local S; curl -d @$S/x; }; f`,
		`f() { S=/good; builtin declare S; curl -d @$S/x; }; f`,
		`f() { S=/good; builtin typeset S; curl -d @$S/x; }; f`,
		`f() { S=/good; \local S; curl -d @$S/x; }; f`,
		`f() { S=/good; 'local' S; curl -d @$S/x; }; f`,
		`f() { S=/good; "declare" S; curl -d @$S/x; }; f`,
		`f() { S=/good; command -- declare S; curl -d @$S/x; }; f`,
		`S=/good; f() { readonly S; curl -d @$S/x; }; f`,
		`S=/good; f() { S=/inside; curl -d @$S/x; }; alias "$x"; f`,
		`S=/good; f() { S=/inside; curl -d @$S/x; }; builtin trap "$x" EXIT; f`,
		`S=/good; f() { S=/inside; curl -d @$S/x; }; declare -$o r=S; f`,
		`S=/good; f() { S=/inside; curl -d @$S/x; }; shopt -s "$opt"; f`,
		`S=/good; x='S=/bad'; trap "$x" USR1; f() { S=/inside; kill -USR1 $$; curl -d @$S/x; }; f`,
		`S=/good; x='S=/bad'; trap "$x" USR1; kill -USR1 $$; curl -d @$S/x`,
		`S=/good; declare -$o r=S; curl -d @$S/x`,
		`S=/good; eval "$x"; curl -d @$S/x`,
		`S=/good; source "$file"; curl -d @$S/x`,
		`S=/good; x='S=/bad'; ${!n:=x}; curl -d @$S/x`,
	} {
		got, err := Assigns(src)
		if err != nil {
			t.Fatalf("Assigns(%q): %v", src, err)
		}
		found := false
		for _, a := range got {
			if a.Name == "S" {
				found = true
				if !a.Unknown {
					t.Errorf("Assigns(%q) S = %+v, want Unknown", src, a)
				}
			}
		}
		if !found {
			t.Errorf("Assigns(%q) has no S assignment", src)
		}
	}
	for _, src := range []string{
		`tmp=$(mktemp -d); trap "rm -rf $tmp" EXIT; S=/good; curl -d @$S/x`,
		`export FOO="$1"; S=/good; curl -d @$S/x`,
	} {
		got, err := Assigns(src)
		if err != nil {
			t.Fatalf("Assigns(%q): %v", src, err)
		}
		for _, a := range got {
			if a.Name == "S" && (a.Unknown || a.Value != "/good") {
				t.Errorf("Assigns(%q) S = %+v, want known /good", src, a)
			}
		}
	}
}

func TestFunctionBodyDynamicEffectDisqualifiesKnownExemption(t *testing.T) {
	got, err := Assigns(`f() { S=/good; eval 'S=/other'; curl -d @$S/x; }; f`)
	if err != nil {
		t.Fatal(err)
	}
	if !hasUnknownAssignment(got, "S") {
		t.Fatalf("Assigns returned %+v, want function-body S Unknown after eval", got)
	}
}

func TestFunctionBodyExemptionRejectsAmbiguousOwnerAndReferences(t *testing.T) {
	for _, src := range []string{
		`f() { S=/good; curl -d @$S/x; }; f; f`,
		`S=/prefix f; f() { S=/good; curl -d @$S/x; }`,
		`f() { S=/good; g; curl -d @$S/x; }; g() { :; }; f`,
		`outer() { inner() { S=/good; curl -d @$S/x; }; inner; }; outer`,
		`f() { (S=/good); curl -d @$S/x; }; f`,
		`S=/old; f() { S=/good; curl -d @$S/x; }; f`,
		`g() { echo "$S"; }; f() { S=/good; curl -d @$S/x; }; g; f`,
	} {
		got, err := Assigns(src)
		if err != nil {
			t.Fatalf("Assigns(%q): %v", src, err)
		}
		if !hasUnknownAssignment(got, "S") {
			t.Errorf("Assigns(%q) = %+v, want S Unknown", src, got)
		}
	}
}

func TestAssignsWholeScriptTriggerMarksEveryAssignmentName(t *testing.T) {
	got, err := Assigns(`S=/good; T=/clean; alias a='S=/bad'; S=/later; T=/later`)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if (a.Name == "S" || a.Name == "T") && (!a.Unknown || a.Value != "" || a.Append) {
			t.Errorf("assignment = %+v, want Unknown for whole-script trigger", a)
		}
	}
}

func TestAssignsFlowTriggerOnlyMarksWrittenName(t *testing.T) {
	const src = `S=/good; T=/clean; if true; then S=/branch; fi; T=/later`
	got, err := Assigns(src)
	if err != nil {
		t.Fatal(err)
	}
	seenS, seenT := false, false
	wantT := map[int]string{strings.Index(src, "T=/clean"): "/clean", strings.Index(src, "T=/later"): "/later"}
	for _, a := range got {
		switch a.Name {
		case "S":
			seenS = true
			if !a.Unknown || a.Value != "" {
				t.Errorf("S assignment = %+v, want Unknown", a)
			}
		case "T":
			seenT = true
			if a.Unknown || a.Value != wantT[a.Offset] {
				t.Errorf("T assignment = %+v, want known value %q", a, wantT[a.Offset])
			}
		}
	}
	if !seenS || !seenT {
		t.Fatalf("Assigns omitted assignments: %+v", got)
	}
}

// A prefix assignment is omitted from the result, yet a function called with it sees the prefix value.
func TestAssignsPrefixBeforeFunctionCall(t *testing.T) {
	for _, src := range []string{
		`S=/good; post() { echo "SENT $S/ref"; }; S=/bad post`,
		`S=/tmp/a; post() { curl -d @$S/x; }; S=/tmp/b post`,
	} {
		as, err := Assigns(src)
		if err != nil {
			t.Fatal(err)
		}
		if len(as) != 1 || as[0].Name != "S" || !as[0].Unknown {
			t.Errorf("%q: got %+v, want one Unknown S", src, as)
		}
	}
	as, err := Assigns(`S=/tmp/a; post() { curl -d @$S/x; }; post`)
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 1 || as[0].Unknown || as[0].Value != "/tmp/a" {
		t.Errorf("without a prefix: got %+v, want known /tmp/a", as)
	}
}

// Any spelling that may enable posix mode makes a prefix before a special builtin persist.
func TestAssignsPosixModeSpellings(t *testing.T) {
	for _, src := range []string{
		"shopt -so posix\nS=/good\nS=/bad :",
		"set -o  posix\nS=/good\nS=/bad :",
	} {
		as, err := Assigns(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range as {
			if a.Name == "S" && !a.Unknown {
				t.Errorf("%q: S known %q", src, a.Value)
			}
		}
	}
}
