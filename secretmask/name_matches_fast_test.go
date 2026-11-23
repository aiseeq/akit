package secretmask

import (
	"math/rand"
	"reflect"
	"testing"
)

func TestNameMatchesFastAgreesWithRegexp(t *testing.T) {
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
		checkFlags(t, text)
	}
	r := rand.New(rand.NewSource(71))
	alphabet := []byte("abcXYZ09_-. :='\"\\>\t[]")
	for range 5000 {
		text := make([]byte, r.Intn(120))
		for j := range text {
			text[j] = alphabet[r.Intn(len(alphabet))]
		}
		check(string(text))
		checkFlags(t, string(text))
	}
}

func checkFlags(t *testing.T, text string) {
	t.Helper()
	var want []contextMatch
	coveredUntil := 0
	for _, ix := range flagRE.FindAllStringSubmatchIndex(text, -1) {
		if ix[0] < coveredUntil {
			continue
		}
		name := text[ix[2]:ix[3]]
		kind, ok := optionSecretName(name)
		if !ok {
			continue
		}
		at := ix[1]
		assigned := at < len(text) && text[at] == '='
		if assigned {
			at++
		} else if at == len(text) || !hasFlagSpaceAt(text, at) {
			continue
		}
		if assigned && hasHorizontalSpaceAt(text, at) {
			continue
		}
		value, ok := scanValue(text, at, false)
		if !ok || valueIsStructure(text, value) {
			if ok {
				coveredUntil = value.end
			}
			continue
		}
		coveredUntil = value.end
		if !assigned && !flagValueAllowed(text[value.start:value.end]) || plainValueFor(name, text[value.start:value.end]) {
			continue
		}
		if valueIsValue(text, value, name, kind, false) {
			want = append(want, contextMatch{value.start, value.end, kind})
		}
	}
	if got := flagMatches(text); !reflect.DeepEqual(got, want) {
		t.Fatalf("flagMatches(%q) = %#v, regexp path = %#v", text, got, want)
	}
}
