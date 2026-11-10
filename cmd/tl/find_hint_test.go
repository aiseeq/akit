package main

import (
	"strings"
	"testing"
)

// A short unknown session name gets the same search hint as a long one.
func TestFindUnknownNameHintsSearch(t *testing.T) {
	home := t.TempDir()
	for _, arg := range []string{"lead", "luna", "abc*", "leadxy"} {
		_, err := find(arg, home)
		if err == nil || !strings.Contains(err.Error(), "--grep or --all --project") {
			t.Errorf("%s: error %v", arg, err)
		}
	}
}
