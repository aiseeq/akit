package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

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

type failedOutput struct{}

func (failedOutput) Write([]byte) (int, error) { return 0, errors.New("synthetic write failure") }

func TestWriteMaskedReturnsPieceWriteError(t *testing.T) {
	text := strings.Repeat("ordinary line\n", parallelMask/14+1)
	err := writeMasked(failedOutput{}, text)
	if err == nil || !strings.Contains(err.Error(), "synthetic write failure") {
		t.Fatalf("writeMasked error = %v", err)
	}
}

func TestWriteMaskedReturnsPieceMaskError(t *testing.T) {
	want := errors.New("synthetic mask write failure")
	text := strings.Repeat("ordinary line\n", parallelMask/14+1)
	err := writeMaskedWith(io.Discard, text, func(io.Writer, string) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("writeMasked error = %v, want %v", err, want)
	}
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

func TestReportKeepsDatabaseCutInParallelListing(t *testing.T) {
	const n = parallelHits
	cut := "database supplied cut"
	path := "/synthetic/session.jsonl"
	hits := make([]hit, n)
	for i := range hits {
		hits[i] = hit{src: source{Path: path, ID: "session"}, ev: transcript.Event{Line: i + 1, Text: fmt.Sprintf("full text %d", i)}}
	}
	hits[0].cut = &cut
	res := result{hits: hits, files: 1, linkIDs: map[string]string{path: "session"}}
	var out strings.Builder
	if code := report(res, options{width: 80}, env{now: time.Now()}, &out); code != 0 {
		t.Fatalf("report code = %d", code)
	}
	if !strings.Contains(out.String(), "database supplied cut") || strings.Contains(out.String(), "full text 0") {
		t.Fatalf("parallel report overwrote the database cut: %q", out.String()[:min(300, out.Len())])
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
