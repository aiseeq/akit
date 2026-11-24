package secretmask

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func TestNameMatchesFastAcrossRandomText(t *testing.T) {
	check := func(text string) {
		t.Helper()
		var want []contextMatch
		for _, ix := range contextNameRE.FindAllStringSubmatchIndex(text, -1) {
			if match, ok := nameMatchAt(text, ix); ok {
				want = append(want, match)
			}
		}
		if got := nameMatches(text); !reflect.DeepEqual(got, want) {
			t.Fatalf("nameMatches(%q) = %#v, regexp path = %#v", text, got, want)
		}
	}
	for _, text := range []string{
		"password=hunter2",
		`API_KEY\" = value; other: token`,
		"auth:=value and password => next",
		"not-secret prose; timeout=3 and db-password: hunter2",
		"[скрыто:password] слово: пароль",
		"paſsword=hidden and KEY:another-value",
	} {
		check(text)
	}
	r := rand.New(rand.NewSource(71))
	alphabet := []byte("abcXYZ09_-. :='\"\\>\t[]")
	for range 5000 {
		text := make([]byte, r.Intn(120))
		for j := range text {
			text[j] = alphabet[r.Intn(len(alphabet))]
		}
		check(string(text))
	}
}

func TestNestedSecretFlagsInsideStructuredFlagValues(t *testing.T) {
	for _, in := range []string{
		`--token '{"a": "--password hunter2xyzQ9"}'`,
		`--token "{\"a\": \"--password hunter2xyzQ9\"}"`,
		`--token {"a":"--password=hunter2xyzQ9"}`,
		`--token {"a":1,"b":"--api-key sk9Hx7Lm2Qp4Zt8"}`,
		`run --config '{"x": 1}' --password hunter2xyzQ9`,
		`--secret '[a: 1, "--password Zx9QwErTy7"]'`,
		`cmd --opts "(k: v, '--token Zx9QwErTy7Ab')"`,
		`--token (name: x) --password Zx9QwErTy7`,
	} {
		got, _ := Mask(in)
		for _, secret := range []string{"hunter2xyzQ9", "Zx9QwErTy7", "sk9Hx7Lm2Qp4Zt8", "Zx9QwErTy7Ab"} {
			if strings.Contains(got, secret) {
				t.Errorf("nested flag value was not masked: %q -> %q", in, got)
			}
		}
	}
}
