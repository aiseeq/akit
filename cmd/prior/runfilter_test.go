package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestFoldRuneFastMatchesBMP(t *testing.T) {
	for r := rune(0); r <= 0xFFFF; r++ {
		if got, want := foldRuneFast(r), foldRune(r); got != want {
			t.Fatalf("foldRuneFast(%U) = %U, want %U", r, got, want)
		}
	}
}

func firstMatchLine(q query, line []byte, filter []runNeedle) bool {
	match, _ := firstMatchLineFolded(q, line, filter, nil)
	return match
}

func TestFirstMatchLiteralFastPathMatchesRegexp(t *testing.T) {
	queries := [][]string{{"needle"}, {"обратная", "тень"}, {"Σίσυφος"}, {"Kelvin"}, {""}}
	lines := []string{
		"needle in a report", "NEEDLE in a report", "обратная тень", "ОБРАТНАЯ ТЕНЬ",
		"ΣΊΣΥΦΟΣ", "σίσυφος", "Kelvin", "Kelvin", "nothing here", "",
	}
	for _, words := range queries {
		q, err := newQuery(words)
		if err != nil {
			t.Fatal(err)
		}
		filter := q.runs
		q.literalOnly = false // force the regexp path for the same query
		for _, line := range lines {
			want := firstMatchLine(q, []byte(line), filter)
			q.literals = make([]string, len(words))
			for i, word := range words {
				if regexp.QuoteMeta(word) == word {
					q.literals[i] = foldText(word)
				}
			}
			q.literalOnly = true
			got := firstMatchLine(q, []byte(line), filter)
			if got != want {
				t.Errorf("firstMatchLine(%q, %q) fast = %t, regexp = %t", words, line, got, want)
			}
			q.literalOnly = false
			q.literals = nil
		}
	}
}

// The line scan with a reused fold buffer returns the same line as the regexp applied line by line.
func TestFirstMatchDataMatchesRegexpLineScan(t *testing.T) {
	queries := [][]string{
		{"needle"}, {"Kelvin"}, {"тень", "Σίσυφος"}, {"a\nb"}, {""}, {"ſecret"}, {"журнал", "откатов"},
		{"окн[оа] сжатия", "compact"}, {"ёлка"}, {"^$"},
	}
	texts := []string{
		"before\nKelvin match\nafter\n",
		"Kelvin on the first line\nkelvin later\n",
		"nothing\nОБРАТНАЯ ТЕНЬ\nΣΊΣΥΦΟΣ\n",
		"a\nb on separate lines\n",
		"first\n\nlast",
		"",
		"\n",
		"SECRET\nſecret\n",
		"Журнал\nЖУРНАЛ ОТКАТОВ\n",
		"\xff\xfe журнал \xd0\n\xd0\x96УРНАЛ\n",
		"Окно сжатия\nCOMPACT\n",
		"ЁЛКА и ёлка\n",
	}
	for _, words := range queries {
		q, err := newQuery(words)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range texts {
			var want string
			for line := range bytes.SplitSeq([]byte(text), []byte{'\n'}) {
				if q.any.Match(line) {
					want = string(line)
					break
				}
			}
			if got := firstMatchDataFiltered(q, []byte(text), q.runs); got != want {
				t.Errorf("query %q, text %q: got %q, want %q", words, text, got, want)
			}
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

	// An unreadable index is a miss and is replaced by a valid index.
	indexPath := cachePath(runIndexName)
	if err := os.WriteFile(indexPath, []byte("not an index"), 0600); err != nil {
		t.Fatal(err)
	}
	assertSame(q)
	if _, err := loadRunIndex(); err != nil {
		t.Fatalf("index was not rebuilt: %v", err)
	}

	// Replacing a report with the same size and mtime must still invalidate its cached content.
	replaced := filepath.Join(day, "replaced.md")
	writeRun(replaced, "needle")
	assertSame(q)
	oldFile, err := os.Open(replaced)
	if err != nil {
		t.Fatal(err)
	}
	defer oldFile.Close()
	oldInfo, err := oldFile.Stat()
	if err != nil {
		t.Fatal(err)
	}
	replacement := replaced + ".new"
	writeRun(replacement, "miss!!")
	if err := os.Chtimes(replacement, oldInfo.ModTime(), oldInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, replaced); err != nil {
		t.Fatal(err)
	}
	assertSame(q)
}

func TestRunCacheReadsIdenticalContentsOnce(t *testing.T) {
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	q, err := newQuery([]string{"needle"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, name := range []string{"one.md", "two.md"} {
		write(t, filepath.Join(dir, name), "needle shared report\n")
	}
	files, errs := runMarkdownFiles(dir)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	_ = matchRunFilesCached(q, files) // build the content cache and index
	var reads atomic.Int32
	runEntryReadHook = func(string) { reads.Add(1) }
	t.Cleanup(func() { runEntryReadHook = nil })
	got := matchRunFilesCached(q, files)
	if len(got) != 2 || got[0].line != "needle shared report" || got[1].line != got[0].line {
		t.Fatalf("shared report results = %+v", got)
	}
	if reads.Load() != 1 {
		t.Fatalf("identical reports read %d report bodies; want one", reads.Load())
	}
}

func TestQueryRunFilterMatchesUnfilteredRandomized(t *testing.T) {
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	dir := t.TempDir()
	corpus := []string{
		"build 12345 done", "decision 9876", "abcdefghi", "abcdefgh", "λβγδ", "foo12", "bar345",
		"xλ3", "nothing here", "9999 zebra", "ΑΒΓΔ", "hgfedcba",
	}
	for day := 1; day <= 3; day++ {
		var body strings.Builder
		for i, line := range corpus {
			fmt.Fprintf(&body, "entry %d: %s\n", i, line)
		}
		write(t, filepath.Join(dir, fmt.Sprintf("2026-01-%02d", day), "report.md"), body.String())
	}
	write(t, filepath.Join(dir, "2026-01-04", "alternative.md"), "bar only\n")
	rng := rand.New(rand.NewSource(0x51f17e))
	atoms := []string{`[a-h]`, `\d`, `[0-9]`, `λ`, `[\p{Greek}]`, `(?:foo|bar)`, `.`}
	patterns := []string{`\d{4}`, `[a-h]{12}`, `(?:)`, `(?:foo|bar)`, `(?:λ|β){1,3}`, `(?:[0-9]+|[\p{Greek}]{2})`}
	for range 48 {
		left := atoms[rng.Intn(len(atoms))]
		right := atoms[rng.Intn(len(atoms))]
		minRepeat := rng.Intn(3)
		maxRepeat := minRepeat + 1 + rng.Intn(3)
		patterns = append(patterns, fmt.Sprintf("(?:%s){%d,%d}|(?:%s)?", left, minRepeat, maxRepeat, right))
	}
	for _, pattern := range patterns {
		q, err := newQuery([]string{pattern})
		if err != nil {
			t.Fatalf("pattern %q: %v", pattern, err)
		}
		var filtered, unfiltered bytes.Buffer
		runs(&filtered, q, dir)
		q.runs = nil
		runs(&unfiltered, q, dir)
		if filtered.String() != unfiltered.String() {
			t.Errorf("filtered output differs for %q:\nfiltered:\n%s\nunfiltered:\n%s", pattern, filtered.String(), unfiltered.String())
		}
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

func TestQueryRunFilterIncludesEveryAlternative(t *testing.T) {
	q, err := newQuery([]string{"замок", "загрузчик", "tlfeed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(q.runs) != 3 {
		t.Fatalf("run filter has %d alternatives: %#v", len(q.runs), q.runs)
	}
	for _, word := range []string{"ЗАМОК", "ЗАГРУЗЧИК", "TLFEED"} {
		found := false
		for _, needle := range q.runs {
			if len(needle) == 1 && len(needle[0]) == 1 && needle[0][0] == word {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("run filter does not contain %q: %#v", word, q.runs)
		}
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

func TestAppendFoldMatchesFoldText(t *testing.T) {
	var buf []byte
	for r := rune(0); r <= 0x10FFFF+1; r++ {
		input := string(r)
		buf = appendFold(buf[:0], []byte(input))
		if string(buf) != foldText(input) {
			t.Fatalf("rune %U: appendFold = %q, foldText = %q", r, buf, foldText(input))
		}
	}
	for b := range 256 {
		for c := range 256 {
			input := []byte{byte(b), byte(c), 'a', 0xd0}
			buf = appendFold(buf[:0], input)
			if string(buf) != foldText(string(input)) {
				t.Fatalf("bytes % x: appendFold = %q, foldText = %q", input, buf, foldText(string(input)))
			}
		}
	}
}

// The index alone rejects reports by their bloom: on a warm index only reports that may match are opened
// for reading, and the cache holds no per-report files.
func TestRunIndexBloomRejectsWithoutReading(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	t.Setenv("AKIT_PRIOR_CACHE_DIR", cacheDir)
	dir := t.TempDir()
	var filler strings.Builder
	for i := range 200 {
		fmt.Fprintf(&filler, "строка отчёта %d про журнал и разное\n", i)
	}
	write(t, filepath.Join(dir, "2026-01-01", "match.md"), filler.String()+"обратная тень\n")
	write(t, filepath.Join(dir, "2026-01-02", "other.md"), filler.String())
	write(t, filepath.Join(dir, "2026-01-03", "small.md"), "nothing relevant\n")
	files, errs := runMarkdownFiles(dir)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	q, err := newQuery([]string{"обратная", "тень"})
	if err != nil {
		t.Fatal(err)
	}
	cold := matchRunFilesCached(q, files)
	var reads []string
	var mu sync.Mutex
	runEntryReadHook = func(path string) {
		mu.Lock()
		reads = append(reads, filepath.Base(path))
		mu.Unlock()
	}
	t.Cleanup(func() { runEntryReadHook = nil })
	warm := matchRunFilesCached(q, files)
	if !reflect.DeepEqual(cold, warm) || !reflect.DeepEqual(warm, matchRunFiles(q, files, q.runs)) {
		t.Fatalf("cold %+v, warm %+v differ from uncached", cold, warm)
	}
	if !reflect.DeepEqual(reads, []string{"match.md"}) {
		t.Fatalf("warm index read reports %v; want only match.md", reads)
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != runIndexName {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("cache holds %v; want only %s", names, runIndexName)
	}
}

// The anchor scanner skips only lines the filter rejects: on random text with case variants, multi-byte and
// invalid sequences, report, reply and line matching agree with the regexp applied to every line.
func TestLineScannerMatchesRegexpRandomized(t *testing.T) {
	alphabet := []string{
		"ж", "Ж", "у", "У", "р", "Р", "к", "К", "a", "A", "k", "K", "K", "s", "S", "ſ", "σ", "Σ", "ς",
		"ё", "Ё", " ", "\n", "\n", "—", "\xd0", "\xb6", "\xff", "\xe2\x84", "x", "ки",
	}
	words := []string{"жур", "к", "ks", "Kk", "ſs", "σς", "ёж", "ки", "a k", "—к", "xx", "ж\n", "zzz", "kи"}
	rng := rand.New(rand.NewSource(0x5ca4))
	randomText := func() string {
		var b strings.Builder
		for range rng.Intn(60) {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		return b.String()
	}
	for range 3000 {
		var query []string
		for range 1 + rng.Intn(3) {
			query = append(query, words[rng.Intn(len(words))])
		}
		if rng.Intn(5) == 0 {
			query = append(query, "к[иa]")
		}
		q, err := newQuery(query)
		if err != nil {
			t.Fatal(err)
		}
		text := randomText()
		var want string
		var wantLines []queryLine
		for i, line := range strings.Split(text, "\n") {
			if i == strings.Count(text, "\n") && line == "" && text != "" {
				break // no line after the final newline
			}
			if q.any.MatchString(line) {
				if wantLines == nil {
					want = line
				}
				wantLines = append(wantLines, queryLine{number: i + 1, text: line})
			}
		}
		if got := firstMatchDataFiltered(q, []byte(text), q.runs); got != want {
			t.Fatalf("query %q, text %q: first match %q, want %q", query, text, got, want)
		}
		if got := matchingLines(q, []byte(text), q.runs); !reflect.DeepEqual(got, wantLines) {
			t.Fatalf("query %q, text %q: lines %+v, want %+v", query, text, got, wantLines)
		}
		reply := "### 2026-01-01T00:00:00Z x · claude 0123abcd\n" + text
		hits := replyBlocks(reply, q)
		if (len(hits) > 0) != (len(wantLines) > 0) {
			t.Fatalf("query %q, text %q: reply hits %+v, want lines %+v", query, text, hits, wantLines)
		}
		if len(hits) > 0 {
			wantWords := 0
			for _, word := range q.words {
				for _, line := range wantLines {
					if word.MatchString(line.text) {
						wantWords++
						break
					}
				}
			}
			if hits[0].words != wantWords {
				t.Fatalf("query %q, text %q: reply words %d, want %d", query, text, hits[0].words, wantWords)
			}
		}
		if want != "" && !runFilterMayMatchBloom(q.runs, runTextBloom([]byte(text))) {
			t.Fatalf("query %q, text %q: the report bloom rejected a match", query, text)
		}
	}
}

func TestForEachRuneTrigram(t *testing.T) {
	for _, text := range []string{"", "a", "ab", "abc", "журнал", "aж—𝄞b", "ж\xffу"} {
		var got []string
		forEachRuneTrigram(text, func(gram string) { got = append(got, gram) })
		var want []string
		var starts []int
		for i := range text {
			starts = append(starts, i)
		}
		starts = append(starts, len(text))
		for i := 0; i+3 < len(starts); i++ {
			want = append(want, text[starts[i]:starts[i+3]])
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: trigrams %q, want %q", text, got, want)
		}
	}
}

// Rune trigrams reject a report whose letter pairs, but not letter triples, cover the word: byte trigrams of
// two-byte text are letter pairs and let such reports through.
func TestRunBloomRejectsWordFromLetterPairs(t *testing.T) {
	q, err := newQuery([]string{"журнал"})
	if err != nil {
		t.Fatal(err)
	}
	text := []byte("жуть\nурна\nрнк\nналог\nалый\n")
	if !byteTrigramsInBloom(foldText("журнал"), byteTrigramBloom(text)) {
		t.Fatal("the byte-trigram bloom is expected to pass this text")
	}
	if runFilterMayMatchBloom(q.runs, runTextBloom(text)) {
		t.Error("the report bloom passed a text without the word")
	}
	if !runFilterMayMatchBloom(q.runs, runTextBloom([]byte("старый ЖУРНАЛ\n"))) {
		t.Error("the report bloom rejected the word in upper case")
	}
}

func byteTrigramsInBloom(variant string, bloom []byte) bool {
	for i := 0; i+3 <= len(variant); i++ {
		if !bloomHas(bloom, variant[i:i+3]) {
			return false
		}
	}
	return true
}

func byteTrigramBloom(data []byte) []byte {
	bloom := make([]byte, runBloomBytes)
	for line := range strings.Lines(string(data)) {
		folded := foldText(strings.TrimSuffix(line, "\n"))
		for i := 0; i+3 <= len(folded); i++ {
			bloomAdd(bloom, folded[i:i+3])
		}
	}
	return bloom
}

func TestDecodeRuneMatchesUTF8(t *testing.T) {
	check := func(p []byte) {
		r, n := decodeRune(p)
		wr, wn := utf8.DecodeRune(p)
		if r != wr || n != wn {
			t.Fatalf("decodeRune(%x) = %U %d, want %U %d", p, r, n, wr, wn)
		}
		r, n = decodeLastRune(p)
		wr, wn = utf8.DecodeLastRune(p)
		if r != wr || n != wn {
			t.Fatalf("decodeLastRune(%x) = %U %d, want %U %d", p, r, n, wr, wn)
		}
	}
	check(nil)
	for a := range 256 {
		check([]byte{byte(a)})
		for b := range 256 {
			check([]byte{byte(a), byte(b)})
			check([]byte{0xe2, byte(a), byte(b)})
			check([]byte{byte(a), byte(b), 0x80})
		}
	}
}
