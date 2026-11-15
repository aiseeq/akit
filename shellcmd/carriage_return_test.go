package shellcmd

import (
	"reflect"
	"strings"
	"testing"
)

func TestCarriageReturnWordsAndLineEndings(t *testing.T) {
	tests := []struct {
		name, src, target string
		args              [][]string
	}{
		{"escaped carriage return ends a command", "zz x\\\r\nzk 1", "zk", [][]string{{"1"}}},
		{"escaped carriage return remains in the first argument", "zz x\\\r\nzk 1", "zz", [][]string{{"x\r"}}},
		{"unescaped carriage return follows an argument", "kill 1\r", "kill", [][]string{{"1"}}},
		{"unescaped carriage return starts a comment", "zz x\r#y", "zz", [][]string{{"x"}}},
		{"unescaped carriage return separates the command", "zz\r 1", "zz", [][]string{{"1"}}},
		{"existing control byte is preserved", "zz x\x01\r", "zz", [][]string{{"x\x01"}}},
		{"carriage return in a shell command string", "bash -c \"kill 1\r\"", "kill", [][]string{{"1"}}},
		{"unquoted heredoc ends after carriage return", "zr <<E\nq \\\r\nE\nzk 2", "zk", [][]string{{"2"}}},
		{"quoted heredoc ends after carriage return", "zr <<\"E\"\nq \\\r\nE\nzk 2", "zk", [][]string{{"2"}}},
		{"single quoted carriage return and newline", "zz 'x\\\r\ny'", "zz", [][]string{{"x\\\r\ny"}}},
		{"double quoted carriage return and newline", "zz \"x\\\r\ny\"", "zz", [][]string{{"x\\\r\ny"}}},
		{"command substitution ends a command", "zz $(zz x\\\r\nzk 1)", "zk", [][]string{{"1"}}},
		{"backquote substitution ends a command", "zz `zz x\\\r\nzk 1`", "zk", [][]string{{"1"}}},
		{"shell command string ends a command", "bash -c 'zz x\\\r\nzk 1'", "zk", [][]string{{"1"}}},
		{"even backslashes retain carriage return", "zz x\\\\\r\nzk 1", "zz", [][]string{{"x\\"}}},
		{"three backslashes escape carriage return", "zz x\\\\\\\r\nzk 1", "zz", [][]string{{"x\\\r"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls, err := Find(tt.src, tt.target)
			if err != nil {
				t.Fatal(err)
			}
			var got [][]string
			for _, c := range calls {
				got = append(got, c.Args)
				if c.Offset >= 0 && !strings.HasPrefix(tt.src[c.Offset:], tt.target) {
					t.Errorf("offset %d in %q", c.Offset, tt.src)
				}
			}
			if !reflect.DeepEqual(got, tt.args) {
				t.Fatalf("got args %#v, want %#v; calls %+v", got, tt.args, calls)
			}
		})
	}
}

func TestCarriageReturnWhenControlMarkersAreUnavailable(t *testing.T) {
	var controls []byte
	for b := byte(1); b < 32; b++ {
		if b != '\n' && b != '\r' {
			controls = append(controls, b)
		}
	}
	_, err := Calls("zz '" + string(controls) + "' \\\r")
	if err == nil {
		t.Fatal("expected an error when no marker is available")
	}
	if _, err := Calls("zz '" + string(controls) + "'\r"); err != nil {
		t.Fatalf("unescaped CR does not need a marker: %v", err)
	}
}

func TestCarriageReturnSkipsShellWhitespaceMarkers(t *testing.T) {
	controls := "\x01\x02\x03\x04\x05\x06\x07\x08\t"
	calls, err := Find("zz '"+controls+"' \\\r", "zz")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].Args, []string{controls, "\r"}) {
		t.Fatalf("Find = %+v", calls)
	}
}

func TestEscapedCarriageReturnDoesNotHideNestedHeredocCommand(t *testing.T) {
	src := "/usr/bin/bash <<EOT\nzr <<E\\\\\\\\\r\n\\$(zk 1)\nE\\\r\nzk 2\nEOT\nzk 99"
	calls, err := Find(src, "zk")
	if err == nil && (len(calls) != 2 || !reflect.DeepEqual(calls[0].Args, []string{"99"}) || !reflect.DeepEqual(calls[1].Args, []string{"2"})) {
		t.Fatalf("Find = %+v without a parse error", calls)
	}
}
