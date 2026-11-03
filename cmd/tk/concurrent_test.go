package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// Notes written at the same time must all land in the journal and leave a valid file: a plain
// truncate-and-write lets a longer earlier write leave its tail behind and a later one drop the earlier note.
func TestConcurrentNotesKeepEveryNote(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "20200101-1000-LOW-race.md")
	if err := os.WriteFile(path, []byte("# Race\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n- 2020-01-01 10:00 открыт — A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const writers = 40
	var wg sync.WaitGroup
	codes := make([]int, writers)
	errs := make([]string, writers)
	for i := range writers {
		wg.Go(func() {
			// Notes of different length, with multi-byte text, so a leftover tail would split a rune.
			text := fmt.Sprintf("заметка-%02d %s", i, strings.Repeat("ж", i*7))
			o, err := parseArgs([]string{"note", "race", text, "--by", "A"})
			if err != nil {
				errs[i] = err.Error()
				codes[i] = -1
				return
			}
			var out, errOut bytes.Buffer
			codes[i] = runAtBase(base, dir, o, &out, &errOut)
			errs[i] = errOut.String()
		})
	}
	wg.Wait()
	for i, code := range codes {
		if code != 0 {
			t.Errorf("writer %d: code %d: %s", i, code, errs[i])
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(b) {
		t.Fatalf("ticket is not valid UTF-8 after concurrent notes:\n%q", b)
	}
	for i := range writers {
		if !strings.Contains(string(b), fmt.Sprintf("заметка-%02d ", i)) {
			t.Errorf("note %d lost", i)
		}
	}
	if n := strings.Count(string(b), "## Журнал"); n != 1 {
		t.Errorf("journal heading %d times", n)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".*"))
	if err != nil || len(leftovers) != 0 {
		t.Errorf("temporary files left in the task folder: %v %v", leftovers, err)
	}
}
