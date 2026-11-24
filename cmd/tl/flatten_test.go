package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/akit/transcript"
)

// flattenCases are texts of words, spaces of every kind unicode.IsSpace knows, line breaks at the ends and
// inside runs of spaces, the line-break sign itself, and bytes that are not UTF-8.
func flattenCases() []string {
	cases := []string{"", " ", "\n", "\n\n", "a", " a ", "a\nb", "a \n b", "a\n\nb", "\n a \n", "a\u00a0b", "a\u2028b",
		"a\u3000\u3000b", "\u0085a\u0085", "⏎", "a ⏎ b", "\xff", "a\xffb c", "\xe2\x80", "слово\t\tслово\n", "x\r\ny"}
	parts := []string{"a", "bc", "слово", "⏎", " ", "  ", "\n", "\t", "\r", "\v", "\f", "\u0085", "\u00a0", "\u1680", "\u2000",
		"\u200a", "\u2028", "\u2029", "\u202f", "\u205f", "\u3000", "\u200b", "\xff", "\xc0", "\xe2\x80", "x\xe2\x8f", "\ufffd", "🙂"}
	rnd := uint64(7)
	next := func(n int) int {
		rnd = rnd*6364136223846793005 + 1442695040888963407
		return int(rnd >> 33 % uint64(n))
	}
	for range 5000 {
		var b strings.Builder
		for range next(40) {
			b.WriteString(parts[next(len(parts))])
		}
		cases = append(cases, b.String())
	}
	return cases
}

// flatten gives transcript.Short(text, 0) and its length, and its start for any limit.
func TestFlattenAsShort(t *testing.T) {
	for _, text := range flattenCases() {
		want := transcript.Short(text, 0)
		runes := []rune(want)
		got, total := flatten(text, -1)
		if got != want || total != len(runes) {
			t.Fatalf("%q: flatten %q (%d), Short %q (%d)", text, got, total, want, len(runes))
		}
		for _, limit := range []int{0, 1, 2, len(runes) / 3, len(runes) / 2, len(runes) - 2, len(runes) - 1, len(runes), len(runes) + 1} {
			limit = max(limit, 0)
			start, total := flatten(text, limit)
			if !slices.Equal([]rune(start), runes[:min(limit, len(runes))]) || total != len(runes) {
				t.Fatalf("%q limit %d: start %q (%d), Short %q", text, limit, start, total, want)
			}
		}
	}
}

// maskedShort cuts as it did over the whole flattened text: short and long texts, a secret before, across and
// after the cut, masking that shrinks the start below the cut.
func TestMaskedShortAsWholeFlatten(t *testing.T) {
	old := func(text string, width int) string {
		flat := transcript.Short(text, 0)
		r := []rune(flat)
		if width <= 0 || len(r) <= width {
			return flat
		}
		cut, _ := maskedCut(r, len(r), width)
		return cut
	}
	key := "sk" + "-proj-" + strings.Repeat("x9Y8", 6)
	texts := flattenCases()[:500]
	for _, n := range []int{10, 290, 300, 310, 800, 1700, 5000} {
		texts = append(texts, strings.Repeat("слово \n", n/7+1)[:n], strings.Repeat("a  ", n)+key+" tail",
			strings.Repeat("w ", n/2)+key+strings.Repeat(" z", n), "password="+strings.Repeat("x9", n)+" потом текст",
			strings.Repeat(key+" ", n/20+1))
	}
	for _, text := range texts {
		for _, width := range []int{0, 1, 30, 300} {
			if got, want := maskedShort(text, width), old(text, width); got != want {
				t.Fatalf("width %d, %q:\n got %q\nwant %q", width, text, got, want)
			}
		}
	}
}
