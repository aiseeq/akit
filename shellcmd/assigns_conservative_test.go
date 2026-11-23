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

func TestFunctionBodyDynamicEffectDisqualifiesKnownExemption(t *testing.T) {
	body := span{start: 10, end: 30}
	scan := suspiciousAssignScan{
		functionCounts: map[string]int{"S": 1}, functionRefs: map[string][]int{"S": {20}},
		functionCalls: map[string][]int{"f": {40}}, functionNames: map[span]string{body: "f"},
		functionBodies: []span{body}, bodyIndex: newSpanIndex([]span{body}), isolatedIndex: newSpanIndex(nil),
		assignByOffset: map[int]Assign{15: {Name: "S", Offset: 15, ScopeStart: 10, ScopeEnd: 30}},
		ownerCache:     map[int]functionOwner{15: {body: body, name: "f", count: 1}},
		nestedBodies:   map[span]bool{}, effectBodies: map[span]bool{body: true},
	}
	if scan.safeFunctionBodyName("S", 15, 1, []span{body}) {
		t.Fatal("dynamic effect must disqualify the known-value exemption")
	}
}

func TestFunctionBodyExemptionRejectsAmbiguousOwnerAndReferences(t *testing.T) {
	body := span{start: 10, end: 30}
	makeScan := func() suspiciousAssignScan {
		return suspiciousAssignScan{
			functionCounts: map[string]int{"S": 1}, functionRefs: map[string][]int{"S": {20}},
			functionCalls: map[string][]int{"f": {40}}, functionNames: map[span]string{body: "f"},
			functionBodies: []span{body}, bodyIndex: newSpanIndex([]span{body}), isolatedIndex: newSpanIndex(nil),
			assignByOffset: map[int]Assign{15: {Name: "S", Offset: 15, ScopeStart: 10, ScopeEnd: 30}},
			ownerCache:     map[int]functionOwner{15: {body: body, name: "f", count: 1}},
			nestedBodies:   map[span]bool{}, effectBodies: map[span]bool{},
		}
	}
	t.Run("owner name", func(t *testing.T) {
		scan := makeScan()
		scan.functionNames[body] = ""
		info := scan.ownerCache[15]
		info.name = ""
		scan.ownerCache[15] = info
		scan.functionCalls[""] = []int{40}
		if scan.safeFunctionBodyName("S", 15, 1, []span{body}) {
			t.Fatal("unnamed owner must not qualify")
		}
	})
	t.Run("owner count", func(t *testing.T) {
		scan := makeScan()
		info := scan.ownerCache[15]
		info.count = 2
		scan.ownerCache[15] = info
		if scan.safeFunctionBodyName("S", 15, 1, []span{body}) {
			t.Fatal("ambiguous owner must not qualify")
		}
	})
	t.Run("other textual write", func(t *testing.T) {
		scan := makeScan()
		scan.functionCounts["S"] = 2
		if scan.safeFunctionBodyName("S", 15, 1, []span{body}) {
			t.Fatal("a second textual write must disqualify the exemption")
		}
	})
	t.Run("isolated scope", func(t *testing.T) {
		scan := makeScan()
		scan.isolatedIndex = newSpanIndex([]span{{start: 14, end: 18}})
		if scan.safeFunctionBodyName("S", 15, 1, []span{body}) {
			t.Fatal("isolated assignment must not qualify for the exemption")
		}
	})
	t.Run("assignment scope", func(t *testing.T) {
		scan := makeScan()
		a := scan.assignByOffset[15]
		a.ScopeStart, a.ScopeEnd = 14, 18
		scan.assignByOffset[15] = a
		if scan.safeFunctionBodyName("S", 15, 1, []span{body}) {
			t.Fatal("assignment from a nested scope must not qualify")
		}
	})
	t.Run("other function reference", func(t *testing.T) {
		owner, other := span{start: 10, end: 20}, span{start: 30, end: 40}
		scan := suspiciousAssignScan{
			functionRefs: map[string][]int{"S": {35}}, functionNames: map[span]string{owner: "f", other: "g"},
			bodyIndex: newSpanIndex([]span{owner, other}), ownerCache: map[int]functionOwner{},
		}
		if scan.referencesFollowBodyAssignment("S", 15, owner, []span{owner, other}) {
			t.Fatal("reference in a different body must not follow the assignment")
		}
	})
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
