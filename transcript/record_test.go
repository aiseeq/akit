package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecordAtPrefixStopsAtRecordBoundary(t *testing.T) {
	data := []byte("{\"a\":1}\n{\"timestamp\":\"2026-01-05T00:00:00.000Z\"}\n")
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	pos, prefix, end, err := RecordAt(f, 0, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if pos != 0 || string(prefix) != `{"a":1}` || end != int64(len("{\"a\":1}\n")) {
		t.Fatalf("RecordAt = (%d, %q, %d), want first record only", pos, prefix, end)
	}
}
