package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestWrapLines(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"short", "- коротко\n", "- коротко\n"},
		{"word boundary", "- один два три четыре пять\n", "- один два три\n  четыре пять\n"},
		{"indent kept", "  - один два три четыре\n", "  - один два три\n    четыре\n"},
		{"long word", "- " + strings.Repeat("я", 30) + "\n", "- " + strings.Repeat("я", 18) + "\n  " + strings.Repeat("я", 12) + "\n"},
		{"fence", "```\n" + strings.Repeat("x ", 15) + "\n```\nодин два три четыре пять\n", "```\n" + strings.Repeat("x ", 15) + "\n```\nодин два три четыре\n  пять\n"},
		{"no trailing newline", "один два три четыре пять", "один два три четыре\n  пять"},
		{"deep indent", strings.Repeat(" ", 19) + "один два", strings.Repeat(" ", 19) + "один два"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wrapLines(tt.in, 20); got != tt.want {
				t.Fatalf("wrapLines:\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestWrapFenceIndentBoundariesAndTab(t *testing.T) {
	long := strings.Repeat("word ", 10) + "end"
	fence := strings.Repeat(string(rune(96)), 3)
	for _, tc := range []struct {
		name, prefix, suffix string
		wantWrapped          bool
	}{
		{name: "three spaces opens fence", prefix: "   " + fence + "\n", suffix: "\n   " + fence + "\n", wantWrapped: false},
		{name: "four spaces do not open fence", prefix: "    " + fence + "\n", suffix: "\n", wantWrapped: true},
		{name: "tab does not open fence", prefix: "\t" + fence + "\n", suffix: "\n", wantWrapped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapLines(tc.prefix+long+tc.suffix, 20)
			wrapped := wrapLines(long, 20)
			if strings.Contains(got, wrapped) != tc.wantWrapped {
				t.Fatalf("wrapLines(%q) = %q; wrapped=%v, want %v", tc.prefix, got, strings.Contains(got, wrapped), tc.wantWrapped)
			}
		})
	}
}

func TestWrapFenceIndentIsRelativeToListContent(t *testing.T) {
	long := "- 2026-01-02 10:00 note — tester: " + strings.Repeat("word ", 20) + "end"
	fence := strings.Repeat(string(rune(96)), 3)
	insideFence := "- item\n    " + fence + "\n" + long + "\n    " + fence + "\n"
	got := wrapLines(insideFence, 20)
	if !strings.Contains(got, long) {
		t.Fatalf("four-space fence inside list item did not protect its content:\n%s", got)
	}
	noteContinuation := "- note: first\n      " + fence + "\n- next: " + strings.Repeat("word ", 20) + "end\n"
	got = wrapLines(noteContinuation, 20)
	next := strings.SplitN(noteContinuation, "\n", 3)[2]
	if !strings.Contains(got, wrapLines(next, 20)) || strings.Contains(got, next) {
		t.Fatalf("six-space note continuation was treated as a fence:\n%s", got)
	}
}

func TestWrapLinesKeepsText(t *testing.T) {
	long := "- 2026-01-01 10:00 note — a: " + strings.Repeat("слово ", 60) + "конец"
	got := wrapLines(long+"\n", showWidth)
	for line := range strings.SplitSeq(strings.TrimSuffix(got, "\n"), "\n") {
		if n := utf8.RuneCountInString(line); n > showWidth {
			t.Errorf("%d runes: %q", n, line)
		}
	}
	if joined := strings.ReplaceAll(strings.TrimSuffix(got, "\n"), "\n  ", " "); joined != long {
		t.Fatalf("text changed:\n%q\nwant\n%q", joined, long)
	}
}

func TestShowWrapsJournalAfterIndentedNoteFence(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	long := "- 2026-01-02 10:00 note — tester: " + strings.Repeat("слово ", 40) + "конец"
	text := "# Task\n\nСтатус: открыт\n\n## Журнал\n- 2026-01-01 10:00 note — tester: first\n      ```\n" + long + "\n"
	if err := os.WriteFile(filepath.Join(dir, "20260101-1000-LOW-fence.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(home))
	t.Chdir(home)
	var out, errOut bytes.Buffer
	if code := run(options{cmd: "show", id: "fence"}, &out, &errOut); code != 0 {
		t.Fatalf("show %d: %s", code, errOut.String())
	}
	t.Logf("tk show output:\n%s", out.String())
	if !strings.Contains(out.String(), wrapLines(long, showWidth)) || strings.Contains(out.String(), long) {
		t.Fatalf("show did not wrap the journal after the indented fence:\n%s", out.String())
	}
}

func TestShowWrapsLongLines(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	long := "- 2026-01-01 10:00 note — a: " + strings.Repeat("слово ", 60) + "конец"
	text := "# Длинная строка\n\nСтатус: открыт\n\n## Журнал\n" + long + "\n"
	if err := os.WriteFile(filepath.Join(dir, "20260101-1000-LOW-long-line.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	// Временная папка может лежать внутри чужого git-репо: тикет ищется по HOME, не по его корню.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(home))
	t.Chdir(home)
	var out, errOut bytes.Buffer
	if code := run(options{cmd: "show", id: "long-line"}, &out, &errOut); code != 0 {
		t.Fatalf("show %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), wrapLines(long, showWidth)) || strings.Contains(out.String(), long) {
		t.Fatalf("show does not wrap the journal:\n%s", out.String())
	}
}
