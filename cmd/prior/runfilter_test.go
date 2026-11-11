package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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
		{`(?:foo)*bar`, "BAR"},
		{`foo|bar`, "FOO"},
		{`a\x{FFFD}b`, "a\xffb"},
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

// Ошибка чтения одного отчёта видна в выводе на его месте и не мешает остальным.
func TestRunsReportsUnreadableFileInPlace(t *testing.T) {
	q, err := newQuery([]string{"needle"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, day := range []string{"2026-01-01", "2026-01-02", "2026-01-03"} {
		if err := os.Mkdir(filepath.Join(dir, day), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, data string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("2026-01-03/a.md", "x\nneedle three\n")
	write("2026-01-01/a.md", "needle one\n")
	if err := os.Symlink(filepath.Join(dir, "absent"), filepath.Join(dir, "2026-01-02", "a.md")); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runs(&out, q, dir)
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "2026-01-02/a.md") || !strings.HasPrefix(lines[0], "(") ||
		lines[1] != "2026-01-03/a.md: needle three" || lines[2] != "2026-01-01/a.md: needle one" {
		t.Fatalf("output:\n%s", out.String())
	}

	results := matchRunFiles(q, []string{
		filepath.Join(dir, "2026-01-03", "a.md"), filepath.Join(dir, "2026-01-02", "a.md"), filepath.Join(dir, "2026-01-01", "a.md"),
	}, q.runs)
	if results[0].err != nil || results[1].err == nil || results[2].err != nil ||
		results[0].line != "needle three" || results[2].line != "needle one" {
		t.Fatalf("results: %+v", results)
	}
}
