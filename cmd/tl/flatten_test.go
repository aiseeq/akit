package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

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
		cut, _ := runeMaskedCut(r, len(r), width)
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

// runeMaskedCut is maskedCut over a slice of runes, as it was written first.
func runeMaskedCut(r []rune, total, width int) (string, bool) {
	n := min(total, width+maskWindow)
	for {
		if n > len(r) {
			return "", false
		}
		masked, _ := hide(string(r[:n]))
		m := []rune(masked)
		if n == total && len(m) <= width {
			return masked, true
		}
		if n == total || len(m) >= width+maskWindow {
			return string(m[:width]) + fmt.Sprintf("…(+%d)", len(m)-width+total-n), true
		}
		n = min(total, 2*n)
	}
}

// maskedCut counting runes in the string cuts as over its runes: every start of a flattened text, short of the
// cut and past it, with secrets and bytes that are not UTF-8.
func TestMaskedCutAsRunes(t *testing.T) {
	key := "sk" + "-proj-" + strings.Repeat("x9Y8", 6)
	texts := flattenCases()[:300]
	for _, n := range []int{10, 300, 900, 1700} {
		texts = append(texts, strings.Repeat("слово ", n/6+1), strings.Repeat("a ", n)+key+" tail", "password="+strings.Repeat("x9", n)+" потом",
			strings.Repeat(key+" \xff", n/20+1), strings.Repeat("🙂\xe2\x80 ", n/4))
	}
	for _, text := range texts {
		flat, total := flatten(text, -1)
		r := []rune(flat)
		for _, width := range []int{1, 30, 300} {
			for _, size := range []int{0, width, width + maskWindow - 1, width + maskWindow, 2 * (width + maskWindow), len(r)} {
				size = min(size, len(r))
				got, gotOK := maskedCut(string(r[:size]), total, width)
				want, wantOK := runeMaskedCut(r[:size], total, width)
				if got != want || gotOK != wantOK {
					t.Fatalf("width %d, start of %d runes of %q:\n got %q %v\nwant %q %v", width, size, text, got, gotOK, want, wantOK)
				}
				if utf8.ValidString(flat) {
					continue
				}
				// the start as the bytes of the text: its bad bytes are runes too
				end, _ := runeEnd(flat, size)
				got, gotOK = maskedCut(flat[:end], total, width)
				if got != want || gotOK != wantOK {
					t.Fatalf("width %d, start of %d bytes of %q:\n got %q %v\nwant %q %v", width, end, text, got, gotOK, want, wantOK)
				}
			}
		}
	}
}
