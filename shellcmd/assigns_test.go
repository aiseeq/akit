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
	want := []Assign{top("S", "/a", 0), {Name: "T", Value: "~/b", Tilde: TildeHome, Offset: 13, ScopeStart: -1, ScopeEnd: -1}, top("S", "/z", 40)}
	if len(got) != len(want) {
		t.Fatalf("Assigns = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Assigns[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestAssignsArithmeticAndConditionalWrites(t *testing.T) {
	for _, src := range []string{
		`S=/clean; (( S=1 )); echo "$S"`,
		`S=/clean; ((S++)); echo "$S"`,
		`S=/clean; X=$(( S+=2 )); echo "$S"`,
		`S=/clean; if false; then S=/bad; fi; echo "$S"`,
		`S=/clean; false && S=/bad; echo "$S"`,
		`S=/clean; while false; do S=/bad; echo "$S"; done; echo "$S"`,
	} {
		assigns, err := Assigns(src)
		if err != nil {
			t.Fatalf("Assigns(%q): %v", src, err)
		}
		ref := strings.LastIndex(src, `"$S"`)
		value := ""
		for _, a := range assigns {
			if a.Name == "S" && a.VisibleAt(ref) {
				if a.Unknown {
					value = "<unknown>"
				} else {
					value = a.Value
				}
			}
		}
		if value != "<unknown>" {
			t.Errorf("Assigns(%q) resolves S=%q, want Unknown, assignments=%+v", src, value, assigns)
		}
	}
}

func TestAssignsImplicitWritesAndDeferredExpansions(t *testing.T) {
	tests := []struct{ name, src string }{
		{"nameref", `S=/clean; declare -n R=S; R=/bad; echo "$S"`},
		{"dynamic nameref", `S=/clean; T=S; declare -n R=$T; R=/bad; echo "$R"`},
		{"eval", `S=/clean; eval 'S=/bad'; echo "$S"`},
		{"alias", `S=/clean; alias g='S=/bad'; g; echo "$S"`},
		{"trap", `S=/clean; trap 'S=/bad' DEBUG; echo "$S"`},
		{"mapfile callback", `S=/clean; mapfile -C 'S=/bad;:' -c 1 A <<< x; echo "$S"`},
		{"bash -c scope", `S=/clean; bash -c 'echo "$S"'`},
		{"builtin here-doc", "S=/clean; : <<X; echo \"$S\"\n${ S=/bad; }\nX"},
		{"external here-doc", "S=/clean; cat >/dev/null <<X\n${ S=/bad; }\nX\necho \"$S\""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assigns, err := Assigns(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			ref := strings.LastIndex(tc.src, `"$S"`)
			if ref < 0 {
				ref = strings.LastIndex(tc.src, `"$R"`)
			}
			if ref < 0 {
				t.Fatalf("no reference in %q", tc.src)
			}
			value := ""
			for _, a := range assigns {
				if a.Name != "S" && a.Name != "R" || !a.VisibleAt(ref) {
					continue
				}
				if a.Unknown {
					value = "<unknown>"
				} else {
					value = a.Value
				}
			}
			if value != "<unknown>" {
				t.Errorf("value at reference = %q, want Unknown; assignments=%+v", value, assigns)
			}
		})
	}
}

func TestAssignsFailingRedirectsAndFunsubLocalPrecision(t *testing.T) {
	tests := []struct {
		name, src, want string
		unknown         bool
	}{
		{"export", `S=/clean; export S=/bad 2>/nonexistent/x`, "", true},
		{"readonly", `S=/clean; readonly S=/bad 2>/nonexistent/x`, "", true},
		{"brace group", `S=/clean; { S=/bad; } 2>/nonexistent/x`, "", true},
		{"if", `S=/clean; if true; then S=/bad; fi 2>/nonexistent/x`, "", true},
		{"case", `S=/clean; case x in x) S=/bad;; esac 2>/nonexistent/x`, "", true},
		{"successful redirect", `S=/clean; { S=/bad; } 2>/dev/null`, "/bad", false},
		{"duplicated descriptor", `S=/clean; { S=/bad; } 2>&1`, "/bad", false},
		{"successful function redirect", `S=/clean; f() { S=/bad; }; f 2>/dev/null`, "", true},
		{"funsub later failing local", `S=/clean; x=${ local S 2>/dev/null; local S 2>/nonexistent/x; S=/bad; }`, "/clean", false},
		{"funsub successful output redirect", `S=/clean; x=${ local S; local S >/tmp/w; S=/bad; }`, "/clean", false},
		{"nested funsub failing local", `S=/clean; x=${ local S; y=${ local S 2>/nonexistent/x; S=/bad; }; }`, "/clean", false},
		{"nested funsub successful output redirect", `S=/clean; x=${ local S; y=${ local S >/tmp/w; S=/bad; }; }`, "/clean", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Assigns(tt.src)
			if err != nil {
				t.Fatal(err)
			}
			value := "/clean"
			for _, assignment := range got {
				if assignment.Name != "S" || !assignment.VisibleAt(len(tt.src)+1) {
					continue
				}
				if assignment.Unknown {
					value = "<unknown>"
				} else if assignment.Append {
					if value != "<unknown>" {
						value += assignment.Value
					}
				} else {
					value = assignment.Value
				}
			}
			want := tt.want
			if tt.unknown {
				want = "<unknown>"
			}
			if value != want {
				t.Errorf("Assigns(%q) = %+v, resolved S=%q, want %q", tt.src, got, value, want)
			}
		})
	}
}

func TestAssignsUncertainFunsubBindingInsideOuterFunsub(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{"command substitution", `S=/clean; x=${ local S=/o; y=${ local S >/tmp/w; S=/bad; }; echo M@$S >&2; }`, "<unknown>"},
		{"reply substitution", `S=/clean; x=${ local S=/o; y=${| local S >/tmp/w; S=/bad; }; echo M@$S >&2; }`, "<unknown>"},
		{"append after inner substitution", `S=/clean; x=${ local S=/o; y=${ local S >/tmp/w; S=/bad; }; S+=/z; echo M@$S >&2; }`, "<unknown>"},
		{"three nested substitutions", `S=/clean; x=${ local S=/o; y=${ z=${ local S >/tmp/w; S=/bad; }; echo M@$S >&2; }; }`, "<unknown>"},
		// At equal scope a certain local declared earlier still holds: S is local whether or not the later one runs.
		{"certain local before uncertain one", `S=/clean; x=${ local S; local S >/tmp/w; S=/bad; echo M@$S >&2; }`, "/bad"},
		{"certain local with value before uncertain one", `S=/clean; x=${ local S=/o; local S >/tmp/w; S=/bad; echo M@$S >&2; }`, "/bad"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Assigns(tt.src)
			if err != nil {
				t.Fatal(err)
			}
			echoOff := strings.Index(tt.src, "echo M@$S")
			value := "<unset>"
			for _, assignment := range got {
				if assignment.Name != "S" || !assignment.VisibleAt(echoOff) {
					continue
				}
				if assignment.Unknown {
					value = "<unknown>"
				} else if assignment.Append {
					if value != "<unknown>" {
						value += assignment.Value
					}
				} else {
					value = assignment.Value
				}
			}
			if value != tt.want {
				t.Errorf("Assigns(%q) = %+v, resolved S at echo=%q, want %q", tt.src, got, value, tt.want)
			}
		})
	}
}

func TestAssignsAssignmentOnlyCommandsRunBeforeRedirects(t *testing.T) {
	for _, tc := range []struct{ src, want string }{
		{`S=/clean; S=/bad 2>/nonexistent/x; echo M@$S`, "/bad"},
		{`S=/clean; S=/bad >/tmp/w; echo M@$S`, "/bad"},
		{`S=/clean; S=/bad <file; echo M@$S`, "/bad"},
		{`S=/clean; [[ x ]] && S=/bad 2>/nonexistent/x; echo M@$S`, "<unknown>"},
		{`S=/clean; x=${ S=/bad; } 2>/nonexistent/x; echo M@$S`, "/bad"},
	} {
		got, err := Assigns(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		value := "/clean"
		for _, assignment := range got {
			if assignment.Name == "S" && assignment.VisibleAt(len(tc.src)+1) {
				if assignment.Unknown {
					value = "<unknown>"
				} else {
					value = assignment.Value
				}
			}
		}
		if value != tc.want {
			t.Errorf("Assigns(%q) = %+v, resolved S=%q, want %q", tc.src, got, value, tc.want)
		}
	}
}

func TestAssignsSkipsPrefixAndValuelessDeclarations(t *testing.T) {
	for _, src := range []string{`X=1 cmd`, `export T`, `bash -c 'S=1'`, `local L=1`} {
		got, err := Assigns(src)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("Assigns(%q) = %+v, want none", src, got)
		}
	}
}

func TestAssignsUnknownArraysAndLoopVariables(t *testing.T) {
	for _, tc := range []struct {
		src string
		off int
	}{
		{`S[0]=x`, 0},
		{`S=(x y)`, 0},
		{`S+=(x)`, 0},
		{`declare -a S=(x)`, 11},
		{`for S in x; do :; done`, 4},
	} {
		got, err := Assigns(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "S" || !got[0].Unknown || got[0].Value != "" || got[0].Append || got[0].Offset != tc.off {
			t.Errorf("Assigns(%q) = %+v, want one Unknown S at %d", tc.src, got, tc.off)
		}
	}
	got, err := Assigns(`(S=(x))`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Unknown || got[0].ScopeStart != 0 || got[0].ScopeEnd != len(`(S=(x))`) {
		t.Errorf("Assigns in subshell = %+v, want Unknown in the subshell scope", got)
	}
}

func TestAssignsUnknownFunctionCalls(t *testing.T) {
	for _, tc := range []struct {
		src  string
		name string
		want bool
	}{
		{`S=1; f() { S=2; }; f; :`, "S", true},
		{`f() { export S=2; }; f`, "S", true},
		{`f() { readonly S=2; }; f`, "S", true},
		{`f() { declare -g S=2; }; f`, "S", true},
		{`f() { for S in x; do :; done; }; f`, "S", true},
		{`f() { g; }; g() { S=2; }; f`, "S", true},
		{`f() { local S=2; }; f`, "S", false},
		{`f() { declare S=2; }; f`, "S", false},
		{`f() ( S=2 ); f`, "S", false},
		{`f() { (S=2); }; f`, "S", false},
		{`f() { (declare -g S=2); }; f`, "S", false},
		{`f() { S=2 | cat; }; f`, "S", false},
		{`f() { S=2; }; :`, "S", false},
		{`f() { S=2; }; command f`, "S", false},
		{`f() { S=2; }; builtin f`, "S", false},
		{`command() { S=2; }; command`, "S", true},
		{`builtin() { S=2; }; builtin`, "S", true},
		{`f() { S=2; }; eval f`, "S", true},
		{`f() { S=2; }; eval "f x"`, "S", true},
		{`f() { S=2; }; eval "true; f"`, "S", true},
		{`f() { S=2; }; c=f; eval "$c"`, "S", true},
		{`f() { S=2; }; eval "$(echo f)"`, "S", true},
		{`f() { S=2; }; eval "true;f"`, "S", true},
		{`f() { S=2; }; "f"`, "S", true},
		{`f() { S=2; }; \f`, "S", true},
		{`f() { S=2; }; fn=f; $fn`, "S", true},
		{`f() { S=2; }; eval echo hi`, "S", false},
		{`f() { S=2; }; eval foobar`, "S", false},
		{`f() { S=2; }; eval "xf"`, "S", false},
		{`f() { S=2; }; eval "fbar"`, "S", false},
		{`f() { S=2; }; eval "true;f"`, "S", true},
		{`f() { S=2; }; eval "true&f"`, "S", true},
		{`f() { S=2; }; eval "false||f"`, "S", true},
		{"f() { S=2; }; eval \"f\tx\"", "S", true},
		{`f() { S=2; }; eval "true
f"`, "S", true},
		{`f() { S=2; }; eval "'f'"`, "S", true},
		{`f() { S=2; }; eval '"f"'`, "S", true},
		{`f() { S=2; }; g() { eval "true; f"; }; g`, "S", true},
		{`f() { S=2; }; g() { $fn; }; g`, "S", true},
		{`f() { S=2; }; g() { g() { S=3; }; }; g`, "S", true},
		{`f() { T=2; }; fn=f; $fn`, "S", false},
		{`f() { S=2; local S=/l; }; f`, "S", true},
		{`f() { if false; then local S; fi; S=2; }; f`, "S", true},
		{`f() { declare -gx S=2; }; f`, "S", true},
		{`f() { declare -xg S=2; }; f`, "S", true},
		{`f() { S=2; }; f() { :; }; f`, "S", true},
		{`f() { :; }; f() { S=2; }; f`, "S", true},
		{`f() { g() { S=2; }; }; f`, "S", false},
		{`h() { S=2; }; f() { g() { h; }; }; f`, "S", false},
		{`f() { g() { S=2; }; g; }; f`, "S", true},
		{`f() { declare -g S; S=2; }; f`, "S", true},
		{`f() { declare S; S=2; }; f`, "S", false},
		{`f() { typeset S; S=2; }; f`, "S", false},
		{`f() { local S; for S in x; do :; done; }; f`, "S", false},
		{`f() { (for S in x; do :; done); }; f`, "S", false},
		{`f() { g() { local S; }; S=2; }; f`, "S", true},
		{`f() { export S; }; f`, "S", false},
		{`f() { local S; declare -g S=2; }; f`, "S", true},
		{`f() { S=2; }; g() { f; }; h() { g; }; h`, "S", true},
		{`h() { g; }; g() { f; }; f() { S=2; }; h`, "S", true},
		{`a() { b; }; b() { c; }; c() { S=2; }; a`, "S", true},
		{`c() { S=2; }; b() { c; }; a() { b; }; a`, "S", true},
	} {
		got, err := Assigns(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		unknown := 0
		for _, a := range got {
			if a.Name == tc.name && a.Unknown && a.VisibleAt(len(tc.src)+1) {
				unknown++
			}
		}
		if (unknown != 0) != tc.want {
			t.Errorf("Assigns(%q) = %+v, want function Unknown=%v", tc.src, got, tc.want)
		}
	}
	for _, tc := range []struct {
		src string
		cmd string
	}{
		{`f() { S=2; }; eval "true; f"`, "eval"},
		{`f() { S=2; }; "f"`, `"f"`},
		{`f() { S=2; }; \f`, `\f`},
		{`f() { S=2; }; fn=f; $fn`, `$fn`},
	} {
		got, err := Assigns(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		off := strings.Index(tc.src, tc.cmd)
		found := false
		for _, a := range got {
			if a.Name == "S" && a.Unknown && a.Offset == off {
				found = true
			}
		}
		if !found {
			t.Errorf("Assigns(%q) = %+v, want an Unknown S at %d", tc.src, got, off)
		}
	}
	got, err := Assigns(`f() { S=2; }; (f)`)
	if err != nil {
		t.Fatal(err)
	}
	var unknown []Assign
	for _, a := range got {
		if a.Unknown {
			unknown = append(unknown, a)
		}
	}
	start := strings.Index(`f() { S=2; }; (f)`, "(f)")
	if len(unknown) != 1 || unknown[0].ScopeStart != start || unknown[0].ScopeEnd != start+len("(f)") {
		t.Errorf("subshell call = %+v, want Unknown scoped to (f)", got)
	}
}

func TestFunctionChangesIgnoreCallsOutsideFunctionBody(t *testing.T) {
	const src = `S=good; f() { :; }; g() { S=bad; }; f; (g); : "$S"`
	call := strings.Index(src, "f;")
	assigns, err := Assigns(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, assignment := range assigns {
		if assignment.Name == "S" && assignment.Unknown && assignment.Offset == call {
			t.Fatalf("call outside f's body was attributed to f: %+v", assigns)
		}
	}
}

func TestAssignsLocalOnlyShadowsWhenDeclarationMustRun(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`false && local S; S=/bad`, true},
		{`[ -n "" ] && local S; S=/bad`, true},
		{`true || local S; S=/bad`, true},
		{`( local S ); S=/bad`, true},
		{`local S | cat; S=/bad`, true},
		{`echo "$(local S)"; S=/bad`, true},
		{`local S & wait; S=/bad`, true},
		{`if false; then local S; fi; S=/bad`, true},
		{`if local S; then :; fi; S=/bad`, false},
		{`while local S; false; do :; done; S=/bad`, false},
		{`while true; do local S; done; S=/bad`, true},
		{`for i in; do local S; done; S=/bad`, true},
		{`case x in y) local S;; esac; S=/bad`, true},
		{`{ local S; }; S=/bad`, false},
		{`local S; S=/bad`, false},
		{`local S=/local; S=/bad`, false},
		{`local S || true; S=/bad`, false},
		{`local S; export S=/x`, false},
		{`local T; S=/bad`, true},
		{`export S; S=/bad`, true},
	} {
		src := `S=/clean; f() { ` + tc.body + `; }; f; :`
		got, err := Assigns(src)
		if err != nil {
			t.Fatal(err)
		}
		unknown := false
		for _, a := range got {
			if a.Name == "S" && a.Unknown && a.VisibleAt(len(src)+1) {
				unknown = true
			}
		}
		if unknown != tc.want {
			t.Errorf("Assigns(%q) = %+v, want function Unknown=%v", src, got, tc.want)
		}
	}
}

func TestAssignsGlobalFlagAndEvalBoundaries(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want bool
	}{
		{`f() { local -g S=/bad; }; f`, true},
		{`f() { declare +g S=/bad; }; f`, false},
		{`f() { declare -- -g S=/bad; }; f`, false},
		{`f() { declare -g -- S=/bad; }; f`, true},
		{`opt=-g; f() { declare "$opt" S=/bad; }; f`, true},
		{`f() { S=/bad; }; eval "\$(echo f)"`, true},
		{`f() { S=/bad; }; builtin eval f`, true},
		{`f() { S=/bad; }; command eval f`, true},
		{`f() { S=/bad; }; command eval "$x f"`, true},
		{`f() { S=/bad; }; x=f; builtin eval $x`, true},
		{`f() { S=/bad; }; x=f; command eval "$x"`, true},
		{`f() { S=/bad; }; command echo f`, false},
		{`f() { S=/bad; }; builtin echo f`, false},
	} {
		got, err := Assigns(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		unknown := false
		for _, a := range got {
			if a.Name == "S" && a.Unknown && a.VisibleAt(len(tc.src)+1) {
				unknown = true
			}
		}
		if unknown != tc.want {
			t.Errorf("Assigns(%q) = %+v, want Unknown=%v", tc.src, got, tc.want)
		}
	}
}

func TestAssignsCommandEvalFormsAndRedirectedLocal(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want bool
	}{
		{src: `f() { S=/bad; }; command -p eval f`, want: true},
		{src: `f() { S=/bad; }; command -- eval f`, want: true},
		{src: `f() { S=/bad; }; command -p -- eval f`, want: true},
		{src: `f() { S=/bad; }; x=eval; command "$x" f`, want: true},
		{src: `f() { S=/bad; }; x=eval; command -p "$x" f`, want: true},
		{src: `f() { S=/bad; }; x=eval; command -- "$x" f`, want: true},
		{src: `f() { S=/bad; }; x=eval; builtin $x f`, want: true},
		{src: `f() { S=/bad; }; x=f; command -p eval "$x"`, want: true},
		{src: `f() { local S 2>/nonexistent/x; S=/bad; }; f`, want: true},
		{src: `f() { local S 2>/nonexistent/x || :; S=/bad; }; f`, want: true},
		{src: `f() { local S 2>/dev/null; S=/bad; }; f`, want: false},
		{src: `f() { local S 2>&1; S=/bad; }; f`, want: false},
		{src: `f() { local S; S=/bad; }; f`, want: false},
		{src: `f() { local S 2>/dev/null; S=/bad; }; f`, want: false},
		{src: `f() { S=/bad; }; command echo f`, want: false},
		{src: `f() { S=/bad; }; builtin echo f`, want: false},
		{src: `f() { S=/bad; }; command -p echo f`, want: false},
		{src: `f() { S=/bad; }; x=echo; command "$x" f`, want: true},
		{src: `f() { S=/bad; }; command -pp eval f`, want: true},
		{src: `f() { S=/bad; }; command command eval f`, want: true},
		{src: `f() { S=/bad; }; builtin command eval f`, want: true},
		{src: `f() { S=/bad; }; command -p command -p eval f`, want: true},
		{src: `command() { S=/bad; }; command ls`, want: true},
		{src: `builtin() { S=/bad; }; builtin cd .`, want: true},
		{src: `command() { S=/bad; }; command -v x`, want: true},
		{src: `f() { S=/bad; }; builtin -- eval f`, want: true},
		{src: `f() { declare S 2>/nonexistent/x; S=/bad; }; f`, want: true},
		{src: `f() { typeset S 2>/nonexistent/x; S=/bad; }; f`, want: true},
		{src: `f() { local S 2>&3; S=/bad; }; f`, want: true},
		{src: `f() { local S >&9; S=/bad; }; f`, want: true},
		{src: `f() { { local S; } 2>/nonexistent/x; S=/bad; }; f`, want: true},
		{src: `f() { local S 2>&-; S=/bad; }; f`, want: false},
		{src: `f() { local S >&/dev/null; S=/bad; }; f`, want: false},
		{src: `f() { local S >&2-; S=/bad; }; f`, want: false},
		{src: `f() { local S 0<&0; S=/bad; }; f`, want: false},
		{src: `f() { local S 1>&1; S=/bad; }; f`, want: false},
		{src: `f() { local S 2>&2; S=/bad; }; f`, want: false},
		{src: "f() { local S <<'E'\nx\nE\nS=/bad; }; f", want: false},
		{src: `f() { local S=/bad >/tmp/w; echo M@$S; }; f`, want: true},
		{src: `f() { local S=/bad 2>/nonexistent/x; echo M@$S; }; f`, want: true},
	} {
		got, err := Assigns(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		unknown := false
		for _, assignment := range got {
			if assignment.Name == "S" && assignment.Unknown && assignment.VisibleAt(len(tc.src)+1) {
				unknown = true
			}
		}
		if unknown != tc.want {
			t.Errorf("Assigns(%q) = %+v, want function Unknown=%v", tc.src, got, tc.want)
		}
	}

	for _, tc := range []struct {
		src  string
		want string
	}{
		{`S=/clean; x=${ local S 2>/nonexistent/x; S=/bad; }; echo M@$S`, "bad-or-unknown"},
		{`S=/clean; x=${ local S 2>/dev/null; S=/bad; }; echo M@$S`, "/clean"},
		{`S=/clean; f() { x=${ local S 2>/nonexistent/x; S=/bad; }; }; f; echo M@$S`, "bad-or-unknown"},
		{`S=/clean; x=${ local S >/tmp/w; S=/bad; }; echo M@$S`, "<unknown>"},
		{`S=/clean; x=${| local S >/tmp/w; S=/bad; REPLY=1; }; echo M@$S`, "<unknown>"},
		{`S=/clean; x=${ local S 2>&3; S=/bad; }; echo M@$S`, "<unknown>"},
		{`S=/clean; L=/tmp/l; x=${ local S 2>"$L"; S=/bad; }; echo M@$S`, "<unknown>"},
		{`S=/clean; x=${ local S </etc/hostname; S=/bad; }; echo M@$S`, "<unknown>"},
		{`S=/clean; x=${ declare S >/tmp/w; S=/bad; }; echo M@$S`, "<unknown>"},
	} {
		got, err := Assigns(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		value := "/clean"
		for _, assignment := range got {
			if assignment.Name == "S" && assignment.Unknown && assignment.VisibleAt(len(tc.src)+1) {
				value = "<unknown>"
			} else if assignment.Name == "S" && assignment.VisibleAt(len(tc.src)+1) {
				value = assignment.Value
			}
		}
		valid := value == tc.want || tc.want == "bad-or-unknown" && (value == "/bad" || value == "<unknown>")
		if !valid {
			t.Errorf("Assigns(%q) = %+v, resolved S=%q, want %q", tc.src, got, value, tc.want)
		}
	}

	for _, src := range []string{
		`f() { local S=/bad >/tmp/w; echo M@$S; }; f`,
		`f() { local S=/bad 2>/nonexistent/x; echo M@$S; }; f`,
	} {
		got, err := Assigns(src)
		if err != nil {
			t.Fatal(err)
		}
		echoOff := strings.Index(src, `echo M@$S`)
		found := false
		for _, assignment := range got {
			if assignment.Name == "S" && assignment.Unknown && assignment.VisibleAt(echoOff) {
				found = true
			}
		}
		if !found {
			t.Errorf("Assigns(%q) = %+v, want a visible Unknown initializer", src, got)
		}
	}
}

func TestAssignsUnknownFunctionCallFields(t *testing.T) {
	src := `f() { S=2; }; ( f )`
	got, err := Assigns(src)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(src, "( f )")
	want := Assign{Name: "S", Unknown: true, Offset: strings.LastIndex(src, "f"), ScopeStart: start, ScopeEnd: start + len("( f )")}
	for _, a := range got {
		if a.Unknown {
			if a != want {
				t.Errorf("Unknown function assignment = %+v, want %+v", a, want)
			}
			return
		}
	}
	t.Errorf("Assigns(%q) = %+v, want Unknown %+v", src, got, want)
}

func TestAssignsDynamicFunctionCallMayChangeEveryFunctionVariable(t *testing.T) {
	src := `f() { S=1; }; g() { T=1; }; $fn`
	got, err := Assigns(src)
	if err != nil {
		t.Fatal(err)
	}
	off := strings.Index(src, "$fn")
	var names []string
	for _, a := range got {
		if a.Unknown {
			if a.Offset != off || a.Value != "" || a.Append {
				t.Errorf("dynamic command Unknown = %+v, want empty value at %d", a, off)
			}
			names = append(names, a.Name)
		}
	}
	if strings.Join(names, ",") != "S,T" {
		t.Errorf("Unknown names = %v, want [S T]", names)
	}
}

func TestAssignsEvalAndDynamicCallsPropagateToCaller(t *testing.T) {
	for _, src := range []string{
		`f() { S=1; }; g() { eval "f"; }; g`,
		`f() { S=1; }; g() { $fn; }; g`,
	} {
		got, err := Assigns(src)
		if err != nil {
			t.Fatal(err)
		}
		callOffset := strings.LastIndex(src, "g")
		found := false
		for _, a := range got {
			if a.Name == "S" && a.Unknown && a.Offset == callOffset && a.ScopeStart == -1 && a.ScopeEnd == -1 {
				found = true
			}
		}
		if !found {
			t.Errorf("Assigns(%q) = %+v, want a top-level Unknown S at g call offset %d", src, got, callOffset)
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
	unknown := Assign{Offset: 5, Unknown: true, ScopeStart: 3, ScopeEnd: 10}
	for _, tc := range []struct {
		a    Assign
		off  int
		want bool
	}{
		{top, 4, false}, {top, 5, false}, {top, 6, true}, {top, 100, true},
		{inner, 5, false}, {inner, 6, true}, {inner, 9, true}, {inner, 10, false}, {inner, 11, false},
		{unknown, 4, false}, {unknown, 6, true}, {unknown, 9, true}, {unknown, 10, false},
	} {
		if got := tc.a.VisibleAt(tc.off); got != tc.want {
			t.Errorf("%+v.VisibleAt(%d) = %v, want %v", tc.a, tc.off, got, tc.want)
		}
	}
}
