package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aiseeq/akit/transcript"
)

// parallelTexts are synthetic texts with and without secrets, short and past the cut, with line breaks.
func parallelTexts(n int) []string {
	key := "sk" + "-proj-" + strings.Repeat("x9Y8", 6)
	texts := make([]string, n)
	for i := range texts {
		switch i % 5 {
		case 0:
			texts[i] = fmt.Sprintf("plain text %d", i)
		case 1:
			texts[i] = strings.Repeat("слово ", i%400) + key + "\nдальше"
		case 2:
			texts[i] = "password=" + strings.Repeat("x9", i%300) + " tail\r"
		case 3:
			texts[i] = strings.Repeat(key+" \n", i%7+1)
		default:
			texts[i] = strings.Repeat("a b\n", i%900)
		}
	}
	return texts
}

// The listing cuts worked out on all processors are those of maskedShort, each in its place; a hit with its own
// cut keeps it.
func TestMaskedShortsAsOneByOne(t *testing.T) {
	texts := parallelTexts(3 * parallelHits)
	own := "own cut"
	hits := make([]hit, len(texts))
	for i, text := range texts {
		hits[i] = hit{ev: transcript.Event{Text: text}}
		if i%17 == 0 {
			hits[i].ev.Images = 1
		}
		if i%11 == 0 {
			hits[i].cut = &own
		}
	}
	if maskedShorts(hits[:parallelHits-1], 300) != nil {
		t.Fatal("a short listing is cut in parallel")
	}
	for _, width := range []int{0, 30, 300} {
		cuts := maskedShorts(hits, width)
		for i := range hits {
			want := ""
			if hits[i].cut == nil {
				want = maskedShort(hits[i].ev.Shown(), width)
			}
			if cuts[i] != want {
				t.Fatalf("width %d, hit %d: got %q, want %q", width, i, cuts[i], want)
			}
		}
	}
}

// Output masked in pieces is the output masked line by line: secrets near the piece edges, CRLF lines and a last
// line without a line break.
func TestWriteMaskedInPiecesAsLineByLine(t *testing.T) {
	text := strings.Join(parallelTexts(1500), "\n") + "\nlast password=" + strings.Repeat("x9", 40)
	if len(text) < 2*parallelMask {
		t.Fatalf("test text of %d bytes is too short", len(text))
	}
	for _, cut := range []int{len(text), parallelMask - 1, parallelMask + 7} {
		var got, want strings.Builder
		if err := writeMasked(&got, text[:cut]); err != nil {
			t.Fatal(err)
		}
		if err := writeMaskedLines(&want, text[:cut]); err != nil {
			t.Fatal(err)
		}
		if got.String() != want.String() {
			t.Fatalf("%d bytes: masked in pieces differs from line by line", cut)
		}
	}
}
