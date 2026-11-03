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

func TestMaskGluedKeysPreservesPrefix(t *testing.T) {
	contexts := []string{"\\n", "\\t", "\\r", "_", "a", "0", "%20"}
	for kind, value := range fakes() {
		for _, prefix := range contexts {
			if kind == "sk" && (prefix == "a" || prefix == "0" || prefix == "%20") {
				continue
			}
			t.Run(kind+"/"+prefix, func(t *testing.T) {
				text := prefix + value
				got, found := Mask(text)
				want := prefix + "[скрыто:" + kind + "]"
				if got != want || !maps.Equal(found, map[string]int{kind: 1}) {
					t.Errorf("masked text or count differs: got length %d, counts %v", len(got), found)
				}
			})
		}
	}

	for _, prefix := range []string{"", " ", "_", "\\n", "\\t", "\\r"} {
		t.Run("sk/allowed/"+prefix, func(t *testing.T) {
			text := prefix + fakes()["sk"]
			got, found := Mask(text)
			if got != prefix+"[скрыто:sk]" || !maps.Equal(found, map[string]int{"sk": 1}) {
				t.Errorf("masked text or count differs: got length %d, counts %v", len(got), found)
			}
		})
	}
}

func TestMaskKeysWithGluedSuffix(t *testing.T) {
	suffixes := []string{"_prod", "a", "7", "\\n", "-", "."}
	for kind, value := range fakes() {
		for _, suffix := range suffixes {
			t.Run(kind+"/"+suffix, func(t *testing.T) {
				got, found := Mask(value + suffix)
				if strings.Contains(got, value) || !maps.Equal(found, map[string]int{kind: 1}) {
					t.Errorf("secret remains or count differs: output length %d, counts %v", len(got), found)
				}
				if kind == "aws" && got != "[скрыто:aws]"+suffix {
					t.Errorf("AWS suffix changed or was consumed: output length %d", len(got))
				}
			})
		}
	}
}

// A key glued to a generic sk key with nothing between: the generic form goes
// last, so after the fixed-length aws key it still matches; a variable-length
// body takes the «sk» in, and the rest is not seen as a key — such input is not
// expected, only no full value and a stable result are asked.
func TestMaskDistinctiveGluedToGenericSkIsIdempotent(t *testing.T) {
	for kind, value := range fakes() {
		if kind == "sk" {
			continue
		}
		t.Run(kind, func(t *testing.T) {
			text := value + fakes()["sk"]
			got, found := Mask(text)
			if strings.Contains(got, value) || strings.Contains(got, fakes()["sk"]) {
				t.Errorf("a secret remains in output of length %d", len(got))
			}
			if kind == "aws" && !maps.Equal(found, map[string]int{kind: 1, "sk": 1}) {
				t.Errorf("counts %v, want one of each kind", found)
			}
			again, secondFound := Mask(got)
			if again != got || len(secondFound) != 0 {
				t.Errorf("second masking changed output or found secrets: length %d, counts %v", len(again), secondFound)
			}
		})
	}
}

func TestMaskSkDoesNotMatchInsideWords(t *testing.T) {
	texts := []string{
		"task-1234567890-abcdefghijkl",
		"disk_usage_2024_report_final_v2",
		"risk-proj-1234567890-abcdefghijkl",
	}
	for _, text := range texts {
		got, found := Mask(text)
		if got != text || len(found) != 0 {
			t.Errorf("word changed or counted: length %d, counts %v", len(got), found)
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
