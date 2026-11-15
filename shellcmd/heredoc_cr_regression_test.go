package shellcmd

import (
	"reflect"
	"strings"
	"testing"
)

func TestCRLFScriptKeepsMainCalls(t *testing.T) {
	src := "if zz 1; then\nzk 1\nfi\nfor x in a; do\nzk 2\ndone\ncase a in\na) zk 3;;\nesac\n{ zk 4; }\nf() { zk 5; }\nf\na=(1 2)\nzr <<E\nbody\nE\nzr <<'E'\nbody\nE\nzk 6\n"
	src = strings.ReplaceAll(src, "\n", "\r\n")
	calls, err := Find(src, "zk")
	var got [][]string
	for _, call := range calls {
		got = append(got, call.Args)
	}
	want := [][]string{{"1"}, {"2"}, {"3"}, {"4"}, {"5"}, {"6"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Find = %+v, %v; want %v", calls, err, want)
	}
}

func TestEmptyQuotedHeredocDelimiterIsAccepted(t *testing.T) {
	calls, err := Find("zr <<\"\"\n\nzk 6", "zk")
	if err != nil || len(calls) != 1 {
		t.Fatalf("Find = %+v, %v", calls, err)
	}
}

func TestUnescapedCarriageReturnStillFindsCall(t *testing.T) {
	// The parser treats an unescaped CR as whitespace, unlike Bash; the call is retained.
	calls, err := Find("zz 1\r", "zz")
	if err != nil || len(calls) != 1 || !reflect.DeepEqual(calls[0].Args, []string{"1"}) {
		t.Fatalf("Find = %+v, %v", calls, err)
	}
}
