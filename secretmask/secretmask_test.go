package secretmask

import (
	"fmt"
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
		"sk":          "sk" + "-" + r("x9Y8", 6),
		"github":      "github" + "_pat_" + r("A1b", 7) + "A" + "_" + r("C2d", 19) + "C2",
		"slack":       "xo" + "xb-" + "123456789-987654321-" + r("A1b", 8),
		"aws":         "AK" + "IA" + r("Z9", 8),
		"google":      "AI" + "za" + r("A1b", 11) + "A1",
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
	contexts := []string{"\\n", "\\t", "\\r", "_", "a", "0", "%20", "\\u0041", "\\f", "\\e[31m", "\\033[0;1m", "\\x1b[32m", "\x1b[0m"}
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

	for _, prefix := range []string{"", " ", "_", "\\n", "\\t", "\\r", "%20", "\\u0041", "\\f", "\\e[31m", "\\033[0;1m", "\\x1b[32m", "\x1b[0m"} {
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
// last and recognizes the long random body even after a word character.
func TestMaskDistinctiveGluedToGenericSkIsIdempotent(t *testing.T) {
	generic := "sk-" + strings.Repeat("a1B", 11) + "a1"
	for kind, value := range fakes() {
		if kind == "sk" {
			continue
		}
		t.Run(kind, func(t *testing.T) {
			text := value + generic
			got, found := Mask(text)
			if strings.Contains(got, value) || strings.Contains(got, generic) {
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

func TestMaskDistinctiveSkPrefixes(t *testing.T) {
	body := strings.Repeat("a1bcdefgh", 3)
	for _, prefix := range []string{"sk" + "-proj-", "sk" + "-ant-"} {
		t.Run(prefix, func(t *testing.T) {
			text := "x9" + prefix + body
			got, found := Mask(text)
			if got != "x9[скрыто:sk]" || !maps.Equal(found, map[string]int{"sk": 1}) {
				t.Errorf("distinctive prefix was not masked: output length %d, counts %v", len(got), found)
			}
		})
	}
}

func TestMaskGenericSkAfterEncodedContexts(t *testing.T) {
	contexts := []string{
		`\u001b[32m`, `\u001B[32m`, `\x1B[32m`, `\E[32m`, `\33[32m`, "\x1b[2K", `\e[2K`,
		`\v`, `\b`, `\a`, `\0`, `\x20`, `\012`, `\N`, `%2520`, `=3D`, `&#61`, "x", "9",
	}
	for _, bodyLen := range []int{20, 24, 31, 35} {
		key := "sk-" + strings.Repeat("a1B", 12)[:bodyLen]
		for _, context := range contexts {
			t.Run(fmt.Sprintf("%d/%s", bodyLen, context), func(t *testing.T) {
				got, found := Mask(context + key)
				if got != context+"[скрыто:sk]" || !maps.Equal(found, map[string]int{"sk": 1}) {
					t.Errorf("encoded context was not preserved and masked: output length %d, counts %v", len(got), found)
				}
			})
		}
	}
}

func TestMaskShortRandomRunsAfterEscapedContexts(t *testing.T) {
	google := "AIza" + "Ab1Cd2Ef3Gh" + "-" + "Ij5Kl6Mn7Op" + "-" + "Qr9St0Uv1Wx"
	slack := "xoxb-" + "111111111111-" + "2222222222222-" + strings.Repeat("a", 24)
	sk := "sk-" + "Ab1Cd2Ef3Gh" + "-" + "Ij5Kl6Mn7Op"
	contexts := []string{
		`\u001b[32m`, `\x1B[32m`, `\E[32m`, `\33[32m`, "\x1b[2K", `\e[2K`,
		`\v`, `\b`, `\a`, `\0`, `\x20`, `\012`, `\N`, `%2520`, `%3D`, `%2f`,
		`Bearer%20`, `\e[32m`, `\033[1;32m`, `\x1b[0m`, `\f`, `=3D`, `&#61`,
		`\n`, `\t`, ` `, `&#61;`, `&#x3D;`,
	}
	for _, context := range contexts {
		for kind, value := range map[string]string{"google": google, "slack": slack, "sk": sk} {
			t.Run(kind+"/"+context, func(t *testing.T) {
				got, found := Mask(context + value)
				if got != context+"[скрыто:"+kind+"]" || !maps.Equal(found, map[string]int{kind: 1}) {
					t.Errorf("short random runs after escaped context were not masked: output length %d, counts %v", len(got), found)
				}
			})
		}
	}
}

func TestMaskDistinctivePrefixesRespectWordBoundary(t *testing.T) {
	key := func(prefix string) string { return prefix + strings.Repeat("a1bcdefgh", 3) }
	texts := []string{
		"network_test_ConnectionTimeoutCase",
		"framework_test_HandlesTimeoutCases",
		"xo" + "xo-hugs-and-kisses-from-me",
		"task-proj-migration-notes-2026-final",
		"xo" + "xb-hugs-and-kisses-from-me",
		"ask-ant-colony-optimisation-notes-draft",
		"task_ThisIsAVeryLongCamelCaseFunctionNameForTests",
		"risk_" + strings.Repeat("a", 40),
	}
	for _, text := range texts {
		got, found := Mask(text)
		if got != text || len(found) != 0 {
			t.Errorf("ordinary text changed: output length %d, counts %v", len(got), found)
		}
	}
	for _, prefix := range []string{"x", "2"} {
		text := prefix + key("sk-proj-")
		got, found := Mask(text)
		if got != prefix+"[скрыто:sk]" || !maps.Equal(found, map[string]int{"sk": 1}) {
			t.Errorf("distinctive token with random body was not masked: output length %d, counts %v", len(got), found)
		}
	}
}

func TestMaskSkDoesNotMatchInsideWords(t *testing.T) {
	texts := []string{
		"task-20261103-1604-HIGH-secret-mask-judge",
		"disk_usage_report_2026_11_03_final",
		"risk-assessment-2026-q4-final-report",
		"sk-learn==1.5.2",
		"mask-1234567890-abcdefghijkl",
		"desk-2026-11-03-abcdefghijklmn",
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
