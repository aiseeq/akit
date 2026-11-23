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
