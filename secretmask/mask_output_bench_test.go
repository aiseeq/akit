package secretmask

import (
	"strings"
	"testing"
)

// BenchmarkMaskOutputLines matches tl's line boundary: each line is masked as
// a separate call, while the text resembles ordinary transcript output.
func BenchmarkMaskOutputLines(b *testing.B) {
	line := "assistant: the password appears in this explanation, but there is no assignment or token value " + strings.Repeat("ordinary transcript text ", 8)
	lines := strings.Split(strings.Repeat(line+"\n", 4096), "\n")
	b.SetBytes(int64(len(line) * len(lines)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, line := range lines[:len(lines)-1] {
			Mask(line)
		}
	}
}
