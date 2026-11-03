package secretmask

import (
	"maps"
	"strings"
	"testing"
)

// Fake values are built at run time, so the source holds nothing that looks like a key. Failures name the
// kind and the case, never the value.
func fakes() map[string]string {
	r := func(s string, n int) string { return strings.Repeat(s, n) }
	return map[string]string{
		"private-key": "-----BEGIN RSA " + "PRIVATE KEY-----\n" + r("Ab1", 20) + "\n-----END RSA " + "PRIVATE KEY-----",
		"sk_live":     "sk" + "_live_" + r("a1B2", 5),
		"sk":          "sk" + "-proj-" + r("x9Y8", 6),
		"github":      "gh" + "p_" + r("Q7w", 12),
		"slack":       "xo" + "xb-" + r("12ab-", 3),
		"aws":         "AK" + "IA" + r("Z9", 8),
		"google":      "AI" + "za" + r("k_3-", 9),
	}
}

func TestMaskEachKind(t *testing.T) {
	for kind, value := range fakes() {
		text := "before " + value + " after"
		got, found := Mask(text)
		if strings.Contains(got, value) {
			t.Errorf("%s: the value is still in the text", kind)
		}
		if want := "before [скрыто:" + kind + "] after"; got != want {
			t.Errorf("%s: masked text differs from %q (length %d)", kind, want, len(got))
		}
		if !maps.Equal(found, map[string]int{kind: 1}) {
			t.Errorf("%s: counts %v, want one of its kind", kind, found)
		}
	}
}

func TestMaskCountsRepeats(t *testing.T) {
	f := fakes()
	text := f["aws"] + " and " + f["aws"] + "; " + f["github"]
	got, found := Mask(text)
	if !maps.Equal(found, map[string]int{"aws": 2, "github": 1}) {
		t.Fatalf("counts %v, want aws 2 and github 1", found)
	}
	if got != "[скрыто:aws] and [скрыто:aws]; [скрыто:github]" {
		t.Fatalf("masked text has %d bytes, not the three markers", len(got))
	}
}

// «sk-» opens ordinary hyphenated words; without a digit the match is left alone.
func TestMaskSkNeedsDigit(t *testing.T) {
	text := "the sk-" + "learn-preprocessing-pipeline-module docs"
	got, found := Mask(text)
	if got != text || len(found) != 0 {
		t.Fatalf("a word without digits was masked: counts %v", found)
	}
}

func TestMaskPlainTextUnchanged(t *testing.T) {
	text := "go test ./... — ok; AKIA short; sk-1; ghp_x\n"
	got, found := Mask(text)
	if got != text || found == nil || len(found) != 0 {
		t.Fatalf("plain text changed or counts not empty: %v", found)
	}
}

// An unterminated private key block is masked to the end of the text.
func TestMaskUnterminatedPrivateKey(t *testing.T) {
	text := "key:\n-----BEGIN " + "PRIVATE KEY-----\n" + strings.Repeat("Zz9", 30)
	got, found := Mask(text)
	if got != "key:\n[скрыто:private-key]" || found["private-key"] != 1 {
		t.Fatalf("unterminated block: counts %v, masked length %d", found, len(got))
	}
}
