package shellcmd

import (
	"reflect"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

func TestUnclosedHeredocDelimiterContainingCarriageReturn(t *testing.T) {
	for _, src := range []string{"zr <<E\r\nx\r\nE", "zk 0\r\nzr <<EOF\r\nx\r\nEOF"} {
		calls, err := Find(src, "zr")
		if err != nil || len(calls) != 1 {
			t.Errorf("Find(%q) = %+v, %v", src, calls, err)
		}
	}
}

func TestUnclosedEscapedCarriageReturnDelimiterIsCompleted(t *testing.T) {
	src := "zr <<E\\\r\nbody"
	_, full, err := parseShell(src)
	if err != nil || !strings.HasSuffix(full, "\nE\r\n") {
		t.Fatalf("parseShell = %q, %v", full, err)
	}
}

func TestCarriageReturnErrorRestoresLiteralAndQuotedMarkers(t *testing.T) {
	err := restoreCarriageReturnParseError(syntax.ParseError{Text: "literal \x01 and quoted \\x01"}, 1)
	if err.Text != "literal \r and quoted \\r" {
		t.Fatalf("error text = %q", err.Text)
	}
}

func TestUnclosedHeredocDelimiterRejectsMalformedParserMessage(t *testing.T) {
	for _, text := range []string{"other error", `unclosed here-document "unterminated`} {
		if _, ok := unclosedHeredocDelimiter(text); ok {
			t.Fatalf("accepted %q", text)
		}
	}
}

func TestCarriageReturnMarkerSkipsUnusedTab(t *testing.T) {
	controls := "\x01\x02\x03\x04\x05\x06\x07\x08"
	calls, err := Find("zz '"+controls+"' \\\r", "zz")
	if err != nil || len(calls) != 1 || !reflect.DeepEqual(calls[0].Args, []string{controls, "\r"}) {
		t.Fatalf("Find = %+v, %v", calls, err)
	}
}

func TestAppendedHeredocDelimiterPreservesOriginalCarriageReturn(t *testing.T) {
	src := "zr <<E\r\nx\r\nE"
	_, full, err := parseShell(src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(full, src) || strings.Contains(full, "\x01") {
		t.Fatalf("full = %q", full)
	}
	calls, err := Find(src, "zr")
	if err != nil || len(calls) != 1 || len(calls[0].Redirs) != 1 || calls[0].Redirs[0].Body != "x\r\n" {
		t.Fatalf("calls = %+v, err = %v", calls, err)
	}
}

func TestBackquotedCommentsEndBeforeFollowingCommands(t *testing.T) {
	for _, src := range []string{": `zz a # \\$( \\\\\nzk 1\n`\nzk 9", ": `zz a #\\\\ x \\\\\nzk 1\n`\nzk 9"} {
		calls, err := Find(src, "zk")
		var got [][]string
		for _, c := range calls {
			got = append(got, c.Args)
		}
		last := "9"
		if strings.Contains(src, "zk 2") {
			last = "2"
		}
		if err != nil || !reflect.DeepEqual(got, [][]string{{"1"}, {last}}) {
			t.Errorf("Find(%q) = %+v, %v", src, calls, err)
		}
	}
}

func TestCarriageReturnInFunctionNameDoesNotShadowOtherCommand(t *testing.T) {
	src := "zz\\\r() { :; }\nzz 1"
	calls, err := Find(src, "zz")
	if err != nil || len(calls) != 1 {
		t.Fatalf("Find = %+v, %v", calls, err)
	}
	shadowed, err := calls[0].ShadowedByFunction(src)
	if err != nil || shadowed {
		t.Fatalf("ShadowedByFunction = %v, %v", shadowed, err)
	}
}

func TestFunctionNameWithCarriageReturnShadowsMatchingCommand(t *testing.T) {
	src := "zz\\\r() { :; }\nzz\\\r 1"
	calls, err := Find(src, "zz\r")
	if err != nil || len(calls) != 1 {
		t.Fatalf("Find = %+v, %v", calls, err)
	}
	shadowed, err := calls[0].ShadowedByFunction(src)
	if err != nil || !shadowed {
		t.Fatalf("ShadowedByFunction = %v, %v", shadowed, err)
	}
}

func TestParseErrorDoesNotExposeCarriageReturnMarker(t *testing.T) {
	for _, src := range []string{"(( 1 \\\r 2 )); zk 1", "(( 1 \\\r 2 )); zk 1 #\x01"} {
		_, err := Calls(src)
		if err == nil {
			t.Fatal("expected parse error")
		}
		if strings.Contains(err.Error(), "\\x01") || strings.Contains(err.Error(), "\\x02") || strings.ContainsAny(err.Error(), "\x01\x02") || !strings.Contains(err.Error(), `\r`) {
			t.Fatalf("marker in error: %q", err)
		}
	}
}
