package shellcmd

import (
	"slices"
	"testing"
)

func TestUnbufferIsWrapper(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want []string
	}{
		{`unbuffer kill -9 1`, []string{"unbuffer", "kill"}},
		{`unbuffer -p rm -rf /x`, []string{"unbuffer", "rm"}},
		{`unbuffer make test`, []string{"unbuffer", "make"}},
		{`printf x | unbuffer -p kill -9 1`, []string{"printf", "unbuffer", "kill"}},
	} {
		calls, err := Calls(tc.src)
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		var got []string
		for _, c := range calls {
			got = append(got, c.Name)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: calls %v, want %v", tc.src, got, tc.want)
		}
		if !wrapperOptionsKnown("unbuffer", []string{"-p", "kill"}) {
			t.Errorf("unbuffer -p: flag rejected")
		}
	}
}
