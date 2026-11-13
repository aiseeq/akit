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

func TestShowIgnoresFencesInsideJournal(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fence := strings.Repeat(string(rune(96)), 3)
	long1 := "- 2026-01-02 10:00 note — tester: " + strings.Repeat("alpha ", 30) + "end"
	long2 := "- 2026-01-03 10:00 note — tester: " + strings.Repeat("beta ", 30) + "end"
	outside := strings.Repeat("outside ", 30) + "end"
	body := "# Task\n\n## Вход\n\n" + fence + "\n" + outside + "\n" + fence + "\n\n## Журнал\n- note: first\n      " + fence + "\n" + long1 + "\n- note: second\n" + long2 + "\n\n## После\n\n" + outside + "\n"
	path := filepath.Join(dir, "20260101-1000-LOW-fences.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ticket := task{path: path, slug: "fences", body: body}
	root := filepath.Dir(filepath.Dir(dir))
	for _, o := range []options{
		{cmd: "show", id: "fences"},
		{cmd: "show", id: "fences", sections: []string{"Журнал"}},
		{cmd: "show", id: "fences", tailSet: true, tail: 0},
	} {
		var out, errOut bytes.Buffer
		if code := showTask(root, ticket, o, &out, &errOut); code != 0 {
			t.Fatalf("show %+v: code=%d stderr=%q", o, code, errOut.String())
		}
		for _, long := range []string{long1, long2} {
			if !strings.Contains(out.String(), wrapLines(long, showWidth)) || strings.Contains(out.String(), long) {
				t.Errorf("journal record wasn't wrapped for %+v:\n%s", o, out.String())
			}
		}
		if o.sections == nil && !strings.Contains(out.String(), outside) {
			t.Errorf("outside code fence was changed for %+v:\n%s", o, out.String())
		}
		if o.sections == nil && !strings.Contains(out.String(), "## После\n\n"+outside) {
			t.Errorf("outside-journal fence state changed for %+v:\n%s", o, out.String())
		}
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
