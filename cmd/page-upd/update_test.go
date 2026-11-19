package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdateChangedAndNewBlocksAndPreservesBytes(t *testing.T) {
	dir := t.TempDir()
	page := filepath.Join(dir, "page.html")
	snapshot := filepath.Join(dir, "snapshot.html")
	old := []byte("<!doctype html>\n<main><section id='first'>before</section>\n<p data-upd='2026-01-02T03:04+01:00'>same</p></main>\n")
	newPage := []byte("<!doctype html>\n<main><section id='first' class='x'>after</section>\n<p data-upd='2026-02-03T04:05+02:00'>same</p><aside>new</aside></main>\n")
	writeFile(t, snapshot, old)
	writeFile(t, page, newPage)
	firstClock := func() time.Time { return time.Date(2026, 11, 15, 22, 13, 59, 0, time.FixedZone("CET", 3600)) }
	got, err := execute([]string{page, snapshot}, firstClock)
	if err != nil {
		t.Fatal(err)
	}
	if got != "page-upd: помечено блоков: 2 (#first, aside)" {
		t.Fatalf("summary: %q", got)
	}
	updated, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	want := "<!doctype html>\n<main><section id='first' class='x' data-upd=\"2026-11-15T22:13+01:00\">after</section>\n<p data-upd='2026-02-03T04:05+02:00'>same</p><aside data-upd=\"2026-11-15T22:13+01:00\">new</aside></main>\n"
	if string(updated) != want {
		t.Fatalf("updated bytes:\n%s\nwant:\n%s", updated, want)
	}
	secondClock := func() time.Time { return time.Date(2026, 11, 15, 22, 14, 59, 0, time.FixedZone("CET", 3600)) }
	secondSummary, err := execute([]string{page, snapshot}, secondClock)
	if err != nil {
		t.Fatal(err)
	}
	if secondSummary != "page-upd: помечено блоков: 0" {
		t.Fatalf("second summary has no-op suffix or changed count: %q", secondSummary)
	}
	again, _ := os.ReadFile(page)
	if !bytes.Equal(updated, again) {
		t.Fatal("repeat run with a later clock changed bytes")
	}
}

func TestUpdateColumnSelectorAndChangedOldStamp(t *testing.T) {
	dir := t.TempDir()
	page, snapshot := filepath.Join(dir, "page.html"), filepath.Join(dir, "snapshot.html")
	writeFile(t, snapshot, []byte(`<main><p>outside</p></main><div class="col"><article data-upd="old">old</article></div>`))
	writeFile(t, page, []byte(`<main><p>outside</p></main><div class="col"><article data-upd="old">changed</article></div>`))
	_, err := execute([]string{"--column", ".col", page, snapshot}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(page)
	if !strings.Contains(string(got), `data-upd="2026-`) || strings.Contains(string(got), `<p data-upd`) {
		t.Fatalf("column or existing stamp not handled: %s", got)
	}
}

func TestUpdateUsesFirstMatchingColumn(t *testing.T) {
	dir := t.TempDir()
	page, snapshot := filepath.Join(dir, "page.html"), filepath.Join(dir, "snapshot.html")
	writeFile(t, snapshot, []byte(`<div class="col"><p>one</p></div><div class="col"><p>two</p></div>`))
	writeFile(t, page, []byte(`<div class="col"><p>changed one</p></div><div class="col"><p>changed two</p></div>`))
	if _, err := execute([]string{"--column", ".col", page, snapshot}, func() time.Time {
		return time.Date(2026, 11, 15, 22, 13, 0, 0, time.UTC)
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(got), `data-upd="2026-11-15T22:13+00:00"`) != 1 || !strings.Contains(string(got), `<div class="col"><p>changed two</p></div>`) {
		t.Fatalf("updated more than the first matching column: %s", got)
	}
}

func TestUpdateLatestSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "2026-01-01", "index.html"), []byte("old"))
	writeFile(t, filepath.Join(dir, "2026-02-01", "index.html"), []byte("new"))
	got, err := latestSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "2026-02-01") {
		t.Fatalf("latest snapshot: %s", got)
	}
}

func TestSetDataUpdChangesOnlyOpeningTag(t *testing.T) {
	tests := []struct{ input, want string }{
		{`<div title="a>b"><span data-upd="nested">x</span></div>`, `<div title="a>b" data-upd="2026-11-15T22:13+01:00"><span data-upd="nested">x</span></div>`},
		{`<div title=' data-upd=x'>x</div>`, `<div title=' data-upd=x' data-upd="2026-11-15T22:13+01:00">x</div>`},
		{`<div data-upd>x</div>`, `<div data-upd="2026-11-15T22:13+01:00">x</div>`},
		{`<div DATA-UPD='2026-11-14T01:02+01:00'>x</div>`, `<div DATA-UPD="2026-11-15T22:13+01:00">x</div>`},
	}
	for _, tc := range tests {
		got, err := setDataUpd([]byte(tc.input), "2026-11-15T22:13+01:00")
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("got %s, want %s", got, tc.want)
		}
	}
	got, err := setDataUpd([]byte(`<img src=x/>`), "2026-11-15T22:13+01:00")
	if err != nil || string(got) != `<img src=x data-upd="2026-11-15T22:13+01:00"/>` {
		t.Fatalf("self-closing tag: got %s, err %v", got, err)
	}
}

func TestUpdateReplacesSnapshotTimestampButKeepsNewTimestamp(t *testing.T) {
	dir := t.TempDir()
	page, snapshot := filepath.Join(dir, "page.html"), filepath.Join(dir, "snapshot.html")
	oldStamp := "2026-11-14T01:02+01:00"
	newStamp := "2026-11-15T22:13+01:00"
	writeFile(t, snapshot, []byte(`<main><section><p>old</p></section><aside data-upd="`+oldStamp+`">other</aside></main>`))
	writeFile(t, page, []byte(`<main><section data-upd="`+oldStamp+`"><p>changed</p></section><aside data-upd="`+oldStamp+`">other</aside></main>`))
	got, err := execute([]string{page, snapshot}, func() time.Time {
		return time.Date(2026, 11, 15, 22, 13, 0, 0, time.FixedZone("CET", 3600))
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "page-upd: помечено блоков: 1 (section)" {
		t.Fatalf("old snapshot timestamp was not refreshed: %q", got)
	}
	content, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `section data-upd="`+newStamp+`"`) {
		t.Fatalf("updated block has wrong timestamp: %s", content)
	}

	current := []byte(`<main><section data-upd="2026-11-15T22:13+01:00"><p>new block</p></section></main>`)
	writeFile(t, page, current)
	writeFile(t, snapshot, []byte(`<main><section><p>old block</p></section></main>`))
	got, err = execute([]string{page, snapshot}, func() time.Time {
		return time.Date(2026, 11, 15, 22, 14, 0, 0, time.FixedZone("CET", 3600))
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	if got != "page-upd: помечено блоков: 0" || !bytes.Equal(current, after) {
		t.Fatalf("new timestamp was rewritten: summary %q, bytes equal %t", got, bytes.Equal(current, after))
	}

	writeFile(t, page, []byte(`<main><section data-upd="yesterday"><p>changed</p></section></main>`))
	writeFile(t, snapshot, []byte(`<main><section><p>old</p></section></main>`))
	got, err = execute([]string{page, snapshot}, func() time.Time {
		return time.Date(2026, 11, 15, 22, 15, 0, 0, time.FixedZone("CET", 3600))
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err = os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	if got != "page-upd: помечено блоков: 1 (section)" || !strings.Contains(string(after), `data-upd="2026-11-15T22:15+01:00"`) {
		t.Fatalf("invalid timestamp was not replaced: summary %q, bytes %s", got, after)
	}
}

func TestUpdateMatchesBlocksByNormalizedContent(t *testing.T) {
	cases := []struct {
		name, snapshot, page string
		want                 string
	}{
		{"insert-middle", `<main><section>A</section><section>B</section></main>`, `<main><section>A</section><section>new</section><section>B</section></main>`, "section"},
		{"delete-first", `<main><section>A</section><section>B</section></main>`, `<main><section>B</section></main>`, ""},
		{"reorder", `<main><section>A</section><section>B</section></main>`, `<main><section>B</section><section>A</section></main>`, ""},
		{"duplicate-count", `<main><section>A</section></main>`, `<main><section>A</section><section>A</section></main>`, "section"},
		{"attribute-order", `<main><section id=x class=y>A</section></main>`, `<main><section class=y id=x>A</section></main>`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			page, snapshot := filepath.Join(dir, "page.html"), filepath.Join(dir, "snapshot.html")
			writeFile(t, page, []byte(tc.page))
			writeFile(t, snapshot, []byte(tc.snapshot))
			got, err := execute([]string{page, snapshot}, func() time.Time { return time.Date(2026, 11, 15, 22, 13, 0, 0, time.UTC) })
			if err != nil {
				t.Fatal(err)
			}
			content, _ := os.ReadFile(page)
			wantCount := 0
			if tc.want != "" {
				wantCount = 1
				if !strings.Contains(string(content), `data-upd="2026-11-15T22:13+00:00"`) {
					t.Fatalf("new block has no timestamp: %s", content)
				}
			} else if strings.Contains(string(content), `data-upd=`) {
				t.Fatalf("unchanged block was stamped: %s", content)
			}
			if !strings.Contains(got, fmt.Sprintf("помечено блоков: %d", wantCount)) {
				t.Fatalf("summary: %q", got)
			}
		})
	}
}

func TestUpdateClosesHTMLImpliedTags(t *testing.T) {
	cases := []struct{ snapshot, page, want string }{
		{`<main><p>one<p>two</main>`, `<main><p>one<p>TWO</main>`, `<p data-upd="2026-11-15T22:13+00:00">TWO`},
		{`<main><section>x</main>`, `<main><section>y</main>`, `<section data-upd="2026-11-15T22:13+00:00">y`},
		{`<main><div/><p>a</p></main>`, `<main><div/><p>b</p></main>`, `<div data-upd="2026-11-15T22:13+00:00"/><p>b`},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		page, snapshot := filepath.Join(dir, "page.html"), filepath.Join(dir, "snapshot.html")
		writeFile(t, page, []byte(tc.page))
		writeFile(t, snapshot, []byte(tc.snapshot))
		if _, err := execute([]string{page, snapshot}, func() time.Time { return time.Date(2026, 11, 15, 22, 13, 0, 0, time.UTC) }); err != nil {
			t.Fatal(err)
		}
		content, _ := os.ReadFile(page)
		if !strings.Contains(string(content), tc.want) {
			t.Errorf("missing expected block marker %q in %s", tc.want, content)
		}
	}
}

func TestParseBlocksRejectsUnclosedColumn(t *testing.T) {
	if _, err := parseBlocks([]byte(`<main><p>unfinished`), "main"); err == nil || !strings.Contains(err.Error(), "не закрыт столбец") {
		t.Fatalf("unclosed column error: %v", err)
	}
}

func TestParseBlocksRejectsHTMLParserDepthLimit(t *testing.T) {
	page := `<main>` + strings.Repeat(`<dl>`, 511) + strings.Repeat(`</dl>`, 511) + `</main>`
	if _, err := parseBlocks([]byte(page), "main"); err == nil || !strings.Contains(err.Error(), "некорректный HTML") {
		t.Fatalf("depth limit error: %v", err)
	}
}

func TestRunReturnsErrorForMissingColumn(t *testing.T) {
	dir := t.TempDir()
	page, snapshot := filepath.Join(dir, "page.html"), filepath.Join(dir, "snapshot.html")
	writeFile(t, page, []byte(`<div>page</div>`))
	writeFile(t, snapshot, []byte(`<div>snapshot</div>`))
	var stdout, stderr bytes.Buffer
	code := run([]string{page, snapshot}, &stdout, &stderr, time.Now)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "столбец") {
		t.Fatalf("run returned code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}
