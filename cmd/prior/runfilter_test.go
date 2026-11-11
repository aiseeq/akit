package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunFilterMatchesLineSearch(t *testing.T) {
	cases := []struct {
		pattern string
		data    string
	}{
		{`kelvin`, "before\nKELVIN\nafter"},
		{`secret`, "before\nſECRET\nafter"},
		{`σίσυφος`, "ΣΊΣΥΦΟΣ"},
		{`^start`, "no\nSTART here\n"},
		{`end$`, "no\nhere END"},
		{`\Astart\z`, "no\nSTART\nmore"},
		{`\bword\b`, "sword\nWORD!"},
		{`a\s+b`, "a\nb\nA B"},
		{`a[^x]+b`, "a\nb\nA-B"},
		{`(?-i:Case)`, "case\nCase"},
		{`foo|bar`, "none\nBAR"},
		{`foo|^`, "unrelated"},
		{`x{0,2}y`, "Y"},
		{`(foo)?bar`, "BAR"},
		{`missing`, ""},
		{`missing`, "no newline"},
		{`�`, string([]byte{0xff})},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"/"+tc.data, func(t *testing.T) {
			q, err := newQuery([]string{tc.pattern})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "run.md")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			want, wantErr := firstMatch(q, path)
			got, gotErr := firstMatchFiltered(q, path, q.runs)
			if got != want || (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("got %q, %v; want %q, %v", got, gotErr, want, wantErr)
			}
		})
	}
}

func TestRunFilterReadError(t *testing.T) {
	q, err := newQuery([]string{"missing"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "missing.md")
	_, want := firstMatch(q, path)
	_, got := firstMatchFiltered(q, path, q.runs)
	if want == nil || got == nil || got.Error() != want.Error() {
		t.Fatalf("got %v; want %v", got, want)
	}
}
