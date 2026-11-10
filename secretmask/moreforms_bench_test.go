package secretmask

import (
	"strings"
	"testing"
)

// Transcript-like JSON lines: colons, hyphens, URLs and words that contain tool
// names as substrings, but no credentials. The context rules must stay cheap here.
func BenchmarkMaskTranscriptLike1MiB(b *testing.B) {
	line := `{"type":"assistant","timestamp":"2026-10-31T10:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"Ran go test -count=1 ./cmd/x through the high-level gate; see https://example.com/docs/page and the log-file at build/out.txt: ok, 12 passed, re-run not needed."}]}}` + "\n"
	text := strings.Repeat(line, (1<<20)/len(line)+1)[:1<<20]
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for range b.N {
		Mask(text)
	}
}
