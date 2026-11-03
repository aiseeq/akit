package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Blank lines are lines of the file: event and malformed-line numbers match sed -n.
func TestReadCountsBlankLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	body := strings.Join([]string{
		`{"type":"user","origin":{"kind":"human"},"timestamp":"2026-10-31T10:00:00Z","message":{"role":"user","content":"раз"}}`,
		"",
		"{broken json",
		`{"type":"user","origin":{"kind":"human"},"timestamp":"2026-10-31T10:01:00Z","message":{"role":"user","content":"два"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, meta := readAll(t, p, nil)
	if len(got) != 2 || !strings.HasPrefix(got[0], "1 ") || !strings.HasPrefix(got[1], "4 ") {
		t.Errorf("event lines: got %q, want lines 1 and 4", got)
	}
	if meta.FirstBad == nil || !strings.HasPrefix(meta.FirstBad.Error(), "line 3:") {
		t.Errorf("first malformed line: got %v, want line 3", meta.FirstBad)
	}
}
