package secretmask

import (
	"strings"
	"testing"
)

func testTagger(t *testing.T) *Tagger {
	t.Helper()
	tg, err := NewTagger([]byte("synthetic-tag-key-0001"))
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

// A marker tells the hidden value's length and a keyed tag: equal values get
// equal tags, different ones different, and the counts are those of Mask.
func TestTaggerMarksLengthAndTag(t *testing.T) {
	tg := testTagger(t)
	got, found := tg.Mask("PASSWORD=hunter22 x; export TOKEN=hunter22 y; PASSWORD=hunter23 z")
	same, diff := tg.Tag("hunter22"), tg.Tag("hunter23")
	want := "PASSWORD=[скрыто:password len=8 #" + same + "] x; export TOKEN=[скрыто:token len=8 #" + same +
		"] y; PASSWORD=[скрыто:password len=8 #" + diff + "] z"
	if got != want || found["password"] != 2 || found["token"] != 1 {
		t.Errorf("got %q %v\nwant %q", got, found, want)
	}
	if same == diff || len(same) != 2*tagBytes {
		t.Errorf("tags %q %q", same, diff)
	}
	plain, plainFound := Mask("PASSWORD=hunter22 x; export TOKEN=hunter22 y; PASSWORD=hunter23 z")
	if strings.Contains(plain, " len=") || len(plainFound) != len(found) {
		t.Errorf("Mask changed: %q %v", plain, plainFound)
	}
}

// The tail dropped after a sk_live key is part of the hidden value.
func TestTaggerCountsLiveKeyTail(t *testing.T) {
	tg := testTagger(t)
	key := "sk" + "_live_" + strings.Repeat("a1B2", 8)
	got, _ := tg.Mask(key + "_tail-x end")
	if want := "[скрыто:sk_live len=47 #" + tg.Tag(key+"_tail-x") + "] end"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Masking drawn text again keeps its markers: a rule that reads a marker as a
// value, as the Cookie one does, does not tag the marker instead of the value.
func TestTaggerSecondPassKeepsMarkers(t *testing.T) {
	tg := testTagger(t)
	for _, in := range []string{"curl -H 'Cookie: sid=Vvalue123' x", "PASSWORD=hunter22 x", "plain text"} {
		once, _ := tg.Mask(in)
		if twice, _ := tg.Mask(once); twice != once {
			t.Errorf("%q: %q then %q", in, once, twice)
		}
	}
}

// A placeholder-like text in the input shows no value: with nothing hidden it
// stays as it is.
func TestTaggerForeignPlaceholderStays(t *testing.T) {
	tg := testTagger(t)
	for _, in := range []string{"[скрыто:sk" + placeholderSep + "0] x", "[скрыто:sk" + keptSep + "3] x"} {
		if got, _ := tg.Mask(in); got != in {
			t.Errorf("%q -> %q", in, got)
		}
	}
}

func TestNewTaggerShortKey(t *testing.T) {
	if _, err := NewTagger(make([]byte, MinTagKeyLen-1)); err == nil {
		t.Error("short key accepted")
	}
	if _, err := NewTagger(make([]byte, MinTagKeyLen)); err != nil {
		t.Error(err)
	}
}

// A rule that hides a value holding a key hidden by an earlier pass measures
// and tags the whole original value, not the marker inside it.
func TestTaggerMeasuresSwallowedMarker(t *testing.T) {
	tg := testTagger(t)
	value := "Vv1 " + "sk" + "-proj-" + strings.Repeat("x9Y8", 6)
	got, found := tg.Mask("curl -H 'Cookie: sid=" + value + "' x")
	want := "curl -H 'Cookie: sid=[скрыто:token len=36 #" + tg.Tag(value) + "]' x"
	if got != want || found["sk"] != 1 || found["token"] != 1 {
		t.Errorf("got %q %v, want %q", got, found, want)
	}
}
