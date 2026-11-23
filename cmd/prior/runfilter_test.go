package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestFoldRuneFastMatchesBMP(t *testing.T) {
	for r := rune(0); r <= 0xFFFF; r++ {
		if got, want := foldRuneFast(r), foldRune(r); got != want {
			t.Fatalf("foldRuneFast(%U) = %U, want %U", r, got, want)
		}
	}
}

func TestRunCacheInvalidation(t *testing.T) {
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	q, err := newQuery([]string{"needle"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	day := filepath.Join(dir, "2026-01-01")
	if err := os.Mkdir(day, 0700); err != nil {
		t.Fatal(err)
	}
	one := filepath.Join(day, "one.md")
	writeRun := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeRun(one, "needle first\n")
	assertSame := func(query query) {
		t.Helper()
		files, errs := runMarkdownFiles(dir)
		if len(errs) != 0 {
			t.Fatal(errs)
		}
		cached := matchRunFilesCached(query, files)
		uncached := matchRunFiles(query, files, query.runs)
		if !reflect.DeepEqual(cached, uncached) {
			t.Fatalf("cached results %+v differ from uncached %+v", cached, uncached)
		}
	}
	assertSame(q) // create a valid cache
	otherQuery, err := newQuery([]string{"first"})
	if err != nil {
		t.Fatal(err)
	}
	assertSame(otherQuery) // queries share a content index

	// A size change invalidates the entry even if the old match line remains.
	writeRun(one, "needle changed and grew\n")
	assertSame(q)

	// Same-size edits are caught by mtime.
	info, err := os.Stat(one)
	if err != nil {
		t.Fatal(err)
	}
	writeRun(one, strings.Repeat("x", len("needle changed and grew\n")-1)+"\n")
	if err := os.Chtimes(one, info.ModTime().Add(2*time.Second), info.ModTime().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertSame(q)

	// New and removed reports update the inventory on the next call.
	two := filepath.Join(day, "two.md")
	writeRun(two, "needle added\n")
	assertSame(q)
	if err := os.Remove(one); err != nil {
		t.Fatal(err)
	}
	assertSame(q)

	// Unreadable cache data is a miss and is replaced by a valid cache.
	cachePath := runEntryPath(two)
	if err := os.WriteFile(cachePath, []byte("not a gob"), 0600); err != nil {
		t.Fatal(err)
	}
	assertSame(q)
	file, err := os.Open(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, runHeaderBytes)
	if _, err := file.Read(header); err != nil || !bytes.Equal(header[:4], runCacheMagic[:]) {
		_ = file.Close()
		t.Fatalf("cache was not rebuilt: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunBloomDoesNotRejectMatches(t *testing.T) {
	for _, tc := range []struct{ pattern, text string }{
		{"needle", "before\nNEEDLE on this line\nafter"},
		{"Kelvin", "the kelvin sign KELVIN is here"},
		{"σίσυφος", "ΣΊΣΥΦΟΣ"},
		{"foo|bar", "unrelated\nBAR appears"},
		{"a\\s+b", "a\nb\nA B"},
		{"x{0,2}y", "Y"},
	} {
		q, err := newQuery([]string{tc.pattern})
		if err != nil {
			t.Fatal(err)
		}
		if !q.any.MatchString(tc.text) {
			continue
		}
		if !runFilterMayMatchBloom(q.runs, runTextBloom([]byte(tc.text))) {
			t.Errorf("bloom rejected %q for pattern %q", tc.text, tc.pattern)
		}
	}
}

func TestFirstMatchStreamMatchesDataSearch(t *testing.T) {
	data := strings.Repeat("x", 5000) + "needle on a long line\nsecond needle\nlast"
	q, err := newQuery([]string{"needle"})
	if err != nil {
		t.Fatal(err)
	}
	want := firstMatchDataFiltered(q, []byte(data), q.runs)
	got, err := firstMatchStream(q, bytes.NewReader([]byte(data)), q.runs)
	if err != nil || got != want {
		t.Fatalf("streamed match = %q, %v; want %q", got, err, want)
	}
}

func TestFoldTextSpecialCaseFolds(t *testing.T) {
	for _, input := range []string{"ASCII ascii", "КИРИЛЛИЦА кириллица", "ёЁ", "İiI", "Σσς", "KK"} {
		var want strings.Builder
		for _, r := range input {
			want.WriteRune(foldRune(r))
		}
		if got := foldText(input); got != want.String() {
			t.Errorf("foldText(%q) = %q, want %q", input, got, want.String())
		}
	}
}

func TestRunFilterMatchesLineSearch(t *testing.T) {
	cases := []struct {
		pattern string
		data    string
	}{
		{`kelvin`, "before\nKELVIN\nKelvin\nKelvin\nafter"},
		{`secret`, "before\nſECRET\nSecret\nſecret\nafter"},
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
			want := ""
			for line := range strings.SplitSeq(tc.data, "\n") {
				if q.any.MatchString(line) {
					want = line
					break
				}
			}
			got, gotErr := firstMatchFiltered(q, path, q.runs)
			if got != want || gotErr != nil {
				t.Fatalf("got %q, %v; regexp lines want %q", got, gotErr, want)
			}
		})
	}
}

func TestFoldTextEquivalentAllRunes(t *testing.T) {
	for r := rune(0); r <= 0x10FFFF+1; r++ {
		input := string(r)
		var want strings.Builder
		for _, folded := range input {
			want.WriteRune(foldRune(folded))
		}
		if got := foldText(input); got != want.String() {
			t.Fatalf("rune %U: foldText = %q, want %q", r, got, want.String())
		}
	}
	for b := range 256 {
		for c := range 256 {
			input := string([]byte{byte(b), byte(c), 'a'})
			var want strings.Builder
			for _, folded := range input {
				want.WriteRune(foldRune(folded))
			}
			if got := foldText(input); got != want.String() {
				t.Fatalf("bytes %02x %02x: foldText = %q, want %q", b, c, got, want.String())
			}
		}
	}
}

func TestReviewRunsUnreadableOrder(t *testing.T) {
	dir := t.TempDir()
	for _, s := range []string{"a/b", "a-c"} {
		if err := os.MkdirAll(filepath.Join(dir, s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a-c", "x.md"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "a", "b"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "a-c"), 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(dir, "a", "b"), 0o755)
	defer os.Chmod(filepath.Join(dir, "a-c"), 0o755)
	_, errs := runMarkdownFiles(dir)
	if len(errs) == 0 {
		t.Skip("process can read directories after chmod 000")
	}
	if len(errs) != 2 {
		t.Fatalf("got %d directory errors, want 2: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), filepath.Join("a", "b")) {
		t.Fatalf("first directory error = %v, want a/b", errs[0])
	}
	if !strings.Contains(errs[1].Error(), "a-c") {
		t.Fatalf("second directory error = %v, want a-c", errs[1])
	}
}

func TestFilteredRegexpMatchesRegexpOnSameLines(t *testing.T) {
	cases := []struct {
		pattern string
		lines   []string
	}{
		{`(?i)простое решение`, []string{"ПРОСТОЕ РЕШЕНИЕ", "простая правка", "xпростое\tрешение"}},
		{`(?i)(костыль|механизм)`, []string{"КОСТЫЛЬ", "МЕХАНИЗМ", "решение"}},
		{`(?i)фриз[а-я]*`, []string{"ФРИЗ", "фризы", "диск"}},
		{`(?i)foo|бар[а-я]+`, []string{"FOO", "БАРЫ", "baz"}},
		{`(?i)foo|^`, []string{"любой текст", "FOO"}},
		{`(?i)\d{2,4}`, []string{"12", "12345", "нет цифр"}},
		{`(?i)(?:x{0,2})?`, []string{"", "anything", "xxx"}},
		{`(?i)σίσυφος`, []string{"ΣΊΣΥΦΟΣ", "другое"}},
		{`(?i)Kelvin`, []string{"KELVIN", "kelvin", "other"}},
	}
	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			re, err := regexp.Compile(tc.pattern)
			if err != nil {
				t.Fatal(err)
			}
			filter, err := newRunFilter(re)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range tc.lines {
				got, want := matchFiltered(re, filter, line), re.MatchString(line)
				if got != want {
					t.Errorf("matchFiltered(%q) = %v, regexp = %v", line, got, want)
				}
				if want && len(filter) > 0 && !runMayMatchASCII(line, filter) {
					t.Errorf("ASCII prefilter rejected matching line %q", line)
				}
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
