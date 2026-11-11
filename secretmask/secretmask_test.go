package secretmask

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/rand"
	"strings"
	"testing"
	"time"
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
		"task-example-secret-mask-judge",
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

func TestMaskBareCodeValueAfterShortAssignment(t *testing.T) {
	for _, in := range []string{"token := part[start:end]", "password := load", "token => next"} {
		if out, found := Mask(in); out != in || len(found) != 0 {
			t.Errorf("changed: %q -> %q %v", in, out, found)
		}
	}
	if out, _ := Mask(`password := "literal"`); strings.Contains(out, "literal") {
		t.Errorf("quoted literal kept: %q", out)
	}
}

func TestMaskRealTranscriptFalseMaskForms(t *testing.T) {
	cases := []struct {
		name, ordinary, secret string
	}{
		{"code type", `const auth: Record<Kind, string> = {}`, `auth: Summer<2024!`},
		{"equals generic", `auth=Record<Kind, string>`, `auth=Summer<2024!`},
		{"lowercase generic", `auth: record<T, string>`, `auth: Summer<2024!`},
		{"single-letter generic", `auth: T<Kind, string>`, `auth: Summer<2024!`},
		{"digit in generic type", `auth: Record<Kind, string>`, `auth: Summer<2024!`},

		{"function call", `auth: authority(x)`, `auth: Zq7xW9vK2m`},
		{"json object", `"https://api.example.com/auth": {"plan": "pro"}`, `"https://api.example.com/auth": "8f3a9c2e7b1d4f60"`},
		{"go map literal", `"https://api.example.com/auth": map[string]string{"plan": "pro"}`, `auth: Zq7xW9vK2mPq8Rt{`},
		{"go slice literal", `auth: []T{}`, `auth: kQzXwPvR{`},
		{"pwd error", `pwd: error retrieving current directory: getcwd: cannot access parent directories: No such file or directory`, `pwd: Zq7xW9vK2m`},
		{"pwd cannot error", `pwd: cannot access parent directories`, `pwd: Zq7xW9vK2m`},
		{"short number list", `financial=30, auth=60, general=300`, `auth=8f3a9c2e7b1d`},
		{"go struct", `{Name:work Home:/tmp/x/001 Auth:/tmp/x/001/auth.json} <nil>`, `Auth:/tmp/` + strings.Repeat("A1b", 8)},
		{"long temp path", `Auth:/tmp/TestPickAccountShortTakesLongest141398969/001/auth.json`, `Auth:/tmp/` + strings.Repeat("A1b", 8)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, found := Mask(tc.ordinary); got != tc.ordinary || len(found) != 0 {
				t.Errorf("ordinary form changed: %q, %v", got, found)
			}
			got, found := Mask(tc.secret)
			if got == tc.secret || len(found) == 0 {
				t.Errorf("secret value remained: %q, %v", got, found)
			}
		})
	}
}

func TestTypeExpressionHeadAcceptsSingleCharacterTypeName(t *testing.T) {
	text := "T<Kind,"
	value := contextValue{start: 0, end: len("T<Kind")}
	if !typeExpressionHead(text, value) {
		t.Fatal("single-character type name with a generic argument was rejected")
	}
}

func TestSyntaxExceptionHelpersRejectSecretLikeForms(t *testing.T) {
	for _, value := range []string{"{", "map[Random123{", "Zq7xW9vK2mPq8Rt{", "T2{", "T{"} {
		if goCompositeLiteralHead(value) {
			t.Errorf("secret-like or incomplete composite accepted: %q", value)
		}
	}
	for _, value := range []string{"map[string]string{", "map[LongName]Thing{", "[]T{"} {
		if !goCompositeLiteralHead(value) {
			t.Errorf("Go composite literal rejected: %q", value)
		}
	}
	if simpleGoType("") || simpleGoType("T2") || !simpleGoType("Thing") || !simpleGoType("LongName") || simpleGoType("LongerName") {
		t.Fatal("simple Go type identifier boundary changed")
	}
	if shortNumericValue(",") || shortNumericValue("1234567,") || !shortNumericValue("123456,") || !shortNumericValue("300") {
		t.Fatal("short numeric value boundary changed")
	}
	if !isTempDirectoryComponent("TestPickAccountShortTakesLongest141398969") ||
		isTempDirectoryComponent(strings.Repeat("a", 23)+"1234") ||
		isTempDirectoryComponent(strings.Repeat("a", 24)+"123") {
		t.Fatal("temporary directory suffix boundary changed")
	}
	if isPlainPath("/tmp/"+strings.Repeat("A1b", 8)+"}") || isPlainPath("/tmp/"+strings.Repeat("a", 24)+"}1234") {
		t.Fatal("punctuation-bearing path component was accepted")
	}
}

func TestShortNumericExceptionNeedsNumericAssignmentList(t *testing.T) {
	for _, text := range []string{`auth=123456,`, `financial=30, auth=1234567, general=300`} {
		got, found := Mask(text)
		if got == text || len(found) == 0 {
			t.Errorf("standalone or long numeric secret stayed visible: %q", text)
		}
	}
}

func TestQuotedTypeLookalikeIsStillMasked(t *testing.T) {
	text := `auth: "Record<Kind>"`
	got, found := Mask(text)
	if got == text || len(found) == 0 {
		t.Fatalf("quoted token-like value stayed visible: %q", got)
	}
}

func TestShortNumericExceptionIsLimitedToAuthNames(t *testing.T) {
	for _, text := range []string{`password=123456,`, `DB_PASSWORD=123456,`, `secret=123456,`, `token=123456,`, `api_key=123456,`} {
		got, found := Mask(text)
		if got == text || len(found) == 0 {
			t.Errorf("numeric secret escaped masking: %q", text)
		}
	}
}

func TestMaskGoCompositeLiteralStillMasksNestedSecret(t *testing.T) {
	text := `auth: map[string]string{"password": "Zq7xW9vK2mPq"}`
	got, found := Mask(text)
	if !strings.Contains(got, `"password": "[скрыто:password]"`) || len(found) == 0 {
		t.Errorf("nested secret was not masked: %q %v", got, found)
	}
}

func TestNonSecretValueFormsDoNotApplyToOtherSecretNames(t *testing.T) {
	for _, text := range []string{
		`password=123456,`, `secret=304290,`, `token=123456,`,
		`api_key=Zq7xW9vK2mPq8Rt{`, `DB_PASSWORD=Welcome<1`,
		`password=Summer<2024!`, `pwd: error retrieving current directory: getcwd: cannot access parent directories: Zq7xW9vK2mPq`,
		`auth=123456,`, `credentials=985866, user=bob`,
	} {
		got, found := Mask(text)
		if got == text || len(found) == 0 {
			t.Errorf("secret name was exempted from masking: %q", text)
		}
	}
}

func TestGenericHeadAndTailRejectTokenLikeValues(t *testing.T) {
	for _, text := range []string{
		"auth=Zq7xW9vK2m<Pq8Rt>",
		"credentials=Pa55w0rd<Xy>",
		"auth: Kx7Qz9<pR,",
		"auth: H3Un<E>_w$HBz8Sk",
		"auth=Record<Kind>suffix",
		"auth=Record<Kind>>",
		"auth=Record<Kind>_w$HBz8Sk",
		"auth=Record<Kind>)",
		"auth=Abc<def,ghi",
		"auth=Record<,",
		"auth: Record<Kind2, string>",
		"auth=Record<Kind!>",
		"auth=Record<2Kind,",
		"auth=Record<Kind",
		"auth=fooBar<Kind>",
		"auth=fooBarBaz<Kind>",
		"auth=Zq7xW9vK2m<Kind>",
		"auth=map[string]Zq7xW9vK2m{",
		"auth=pa55w0rd<x>",
		"auth=P123<x>",
		"auth=Foo_bar<x>",
		"auth=KxQzWp<abc>",
		"auth=Kxqzwprtmnbvcxzlk<abc>",
		"auth=abcdefghijklmnopqr<abc>",
		"credentials=PWSp<UAxVRVXI2n>",
		"auth=PE<QFCa,",
		"auth: NH<BA8GJ,CRgcjdLTh+a4McX",
	} {
		got, found := Mask(text)
		if got == text || len(found) == 0 {
			t.Errorf("token-like generic head or tail stayed visible: %q", text)
		}
	}
	for _, text := range []string{
		"auth=Record<string>",
		"auth=HashMap<Kind>",
		"auth=Record<Kind, string>",
		"auth: Record<Kind, string> = {}",
		"credentials=Map<Kind>",
		"auth=Record<string, string>",
		"auth=Promise<void>;",
		"auth=HashMap<String, Vec>",
		"auth=Map<string, number> = new Map()",
		"auth=Map<UUID>",
		"auth: Record<Kind>\tx",
		"auth=Record<Kind>\r\n",
	} {
		got, found := Mask(text)
		if got != text || len(found) != 0 {
			t.Errorf("ordinary type expression changed: %q -> %q %v", text, got, found)
		}
	}
}

func TestPlainTypeNameBoundaries(t *testing.T) {
	for _, name := range []string{"", "_", "1Type", "P123", "Foo_bar", "QFCa", "KxQzWp", "Kxqzwprtmnbvcxzlk"} {
		if plainTypeName(name) {
			t.Errorf("token-like type name accepted: %q", name)
		}
	}
	for _, name := range []string{"string", "Record", "HashMap", "UUID"} {
		if !plainTypeName(name) {
			t.Errorf("plain type name rejected: %q", name)
		}
	}
}

func TestTypeTailBoundaries(t *testing.T) {
	base := "Record<Kind>"
	value := contextValue{start: 0, end: len(base)}
	for _, suffix := range []string{"=", " ", "\n", ";", ""} {
		text := base + suffix
		if !typeExpressionHead(text, value) {
			t.Errorf("valid type tail boundary %q was rejected", suffix)
		}
	}
	if typeExpressionHead(base+")", value) {
		t.Fatal("closing parenthesis accepted after a generic type")
	}
}

func TestPwdDiagnosticIsNameAndLineScoped(t *testing.T) {
	message := "error retrieving current directory: getcwd: cannot access parent directories"
	value := contextValue{start: 0, end: len(message)}
	if !nonSecretValueForm(message, value, "pwd") {
		t.Fatal("pwd diagnostic form was not accepted")
	}
	for _, name := range []string{"auth", "password"} {
		if nonSecretValueForm(message, value, name) {
			t.Errorf("pwd diagnostic exception applied to %q", name)
		}
	}
	message = "cannot access parent directories\n"
	value = contextValue{start: 0, end: len("cannot access parent directories")}
	if !pwdErrorDiagnostic(message, value) {
		t.Fatal("newline-terminated shell diagnostic was rejected")
	}
}

func TestNonSecretSyntaxExceptionsAreLimitedToAuthNames(t *testing.T) {
	for _, text := range []string{
		"password=Record<string, string>",
		"secret=map[string]string{",
		"token=map[string]string{",
		"password=map[string]string{",
		"a=1, b=2, password=3",
	} {
		got, found := Mask(text)
		if got == text || len(found) == 0 {
			t.Errorf("syntax exception escaped its allowed name: %q", text)
		}
	}
}

func TestShortNumericExceptionIsDelimitedAndLineLocal(t *testing.T) {
	for _, text := range []string{
		"auth=30, general=300",
		"auth=30, general: 300, retry: 3",
		"a=1, b=2\nfinancial=30, auth=60,",
	} {
		got, found := Mask(text)
		if got == text || len(found) == 0 {
			t.Errorf("short number was exempted without a same-line numeric assignment list: %q", text)
		}
	}
}

func TestMaskPlainTextUnchanged(t *testing.T) {
	text := "go test ./... — ok; AKIA short; sk-1; ghp_x\n"
	got, found := Mask(text)
	if got != text || found == nil || len(found) != 0 {
		t.Fatalf("plain text changed or counts not empty: %v", found)
	}
}

func TestMaskCommandContext(t *testing.T) {
	tests := []struct{ in, want, kind string }{
		{"Authorization: Bearer synthetic-pass", "Authorization: Bearer [скрыто:authorization]", "authorization"},
		{"Proxy-Authorization=basic abc", "Proxy-Authorization=basic [скрыто:authorization]", "authorization"},
		{"PGPASSWORD=secret-value", "PGPASSWORD=[скрыто:password]", "password"},
		{"curl -u user:secret-value x", "curl -u user:[скрыто:password] x", "password"},
		{"curl --user='user:secret-value' x", "curl --user='user:[скрыто:password]' x", "password"},
		{"tool --client-secret secret-value", "tool --client-secret [скрыто:token]", "token"},
		{"mysql -psecret-value db", "mysql -p[скрыто:password] db", "password"},
		{"sshpass -p secret-value ssh host", "sshpass -p [скрыто:password] ssh host", "password"},
		{"postgres://user:secret-value@db/x", "postgres://user:[скрыто:password]@db/x", "password"},
		{`{"api_key":"secret-value"}`, `{"api_key":"[скрыто:token]"}`, "token"},
		{"token: meaningful", "token: [скрыто:token]", "token"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, found := Mask(tt.in)
			if got != tt.want || found[tt.kind] != 1 {
				t.Errorf("got %q, counts %v", got, found)
			}
		})
	}
}

func TestMaskCommandContextLeavesNonValues(t *testing.T) {
	for _, in := range []string{"TOKEN_COUNT=5", "TOKEN_FILE=/run/t", "PWD=/tmp", "OLDPWD=/tmp", "max_tokens=100", "max_token=100", "api_key=api_key", "token=cfg.Token", "password=getpass()", "Authorization: Bearer $TOKEN", "password: string", "token: the value", "curl -u \"$U:$P\" x", "mysql -p db", "curl https://host:8080/@foo"} {
		got, found := Mask(in)
		if got != in || len(found) != 0 {
			t.Errorf("%q changed to %q (%v)", in, got, found)
		}
	}
}

func TestMaskContextReviewForms(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"curl absolute path", "/usr/bin/curl -u admin:secret-value x", "/usr/bin/curl -u admin:[скрыто:password] x"},
		{"curl substitution", "$(curl -u admin:secret-value x)", "$(curl -u admin:[скрыто:password] x)"},
		{"mysql subshell", "(mysql -psecret-value)", "(mysql -p[скрыто:password])"},
		{"mysql absolute path", "/usr/bin/mysql -psecret-value", "/usr/bin/mysql -p[скрыто:password]"},
		{"sshpass after flag", "sshpass -v -p secret-value ssh host", "sshpass -v -p [скрыто:password] ssh host"},
		{"curl grouped short flags", "curl -su admin:secret-value x", "curl -su admin:[скрыто:password] x"},
		{"curl uppercase group", "curl -sSu admin:secret-value x", "curl -sSu admin:[скрыто:password] x"},
		{"curl continued line", "curl -sS \\\n  -u admin:secret-value x", "curl -sS \\\n  -u admin:[скрыто:password] x"},
		{"mysql continued line", "mysql -h db \\\n  -psecret-value", "mysql -h db \\\n  -p[скрыто:password]"},
		{"sshpass continued line", "sshpass \\\n  -p secret-value ssh host", "sshpass \\\n  -p [скрыто:password] ssh host"},
		{"flag continued line", "tool --token \\\n  secret-value", "tool --token \\\n  [скрыто:token]"},
		{"assignment spaces", "token = secret-value", "token = [скрыто:token]"},
		{"password spaces", "password = secret-value", "password = [скрыто:password]"},
		{"quoted code", `password: "cfg.Token"`, `password: "[скрыто:password]"`},
		{"quoted parentheses", `PASSWORD="ab(cd"`, `PASSWORD="[скрыто:password]"`},
		{"equals in value", "PASSWORD=ab=cd", "PASSWORD=[скрыто:password]"},
		{"comma in equals value", "PASSWORD=ab,cd", "PASSWORD=[скрыто:password]"},
		{"unicode value", "PASSWORD=паролька", "PASSWORD=[скрыто:password]"},
		{"short quoted token", `{"token":"12345"}`, `{"token":"[скрыто:token]"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := Mask(tt.input); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMaskContextReviewKeepsNonValues(t *testing.T) {
	for _, input := range []string{
		"curly -u a:b x", "xcurl -u a:b x", "notmysql -psecret", "docs/change-password now",
		"tool admin-token prune", "change-password is broken", "x-api-key is required",
		"password:\n  user: x", "token:\n  value: abc123456", "sshpass -e ssh -p 22 host",
		"sshpass -f /run/p ssh -p 22 host", "mysql -h db -P3306", "mysql -h db -P 3306",
		"PGPASSWORD= psql -h db", "TOKEN= ./run", "curl -d 'q=a:b' x", "curl -X -u x",
		"password: cfg.Token", "password=ab(cd)", "token: the value", "token=12345",
	} {
		if got, found := Mask(input); got != input || len(found) != 0 {
			t.Errorf("%q changed to %q (%v)", input, got, found)
		}
	}
}

func TestMaskContextIndependentReview(t *testing.T) {
	tests := []struct{ name, input, want string }{
		{"quoted ampersand", "curl -d 'a=1&b=2' -u admin:x y", "curl -d 'a=1&b=2' -u admin:[скрыто:password] y"},
		{"multiline quote", "curl -d '{\nbody\n}' -u a:b", "curl -d '{\nbody\n}' -u a:[скрыто:password]"},
		{"unclosed quote", "curl it's x; id -u a:b", "curl it's x; id -u a:b"},
		{"quoted mysql unicode", "mysql -p'пароль' db", "mysql -p'[скрыто:password]' db"},
		{"quoted sshpass dash", "sshpass -p '-hunter2pass' ssh host", "sshpass -p '[скрыто:password]' ssh host"},
		{"nested escaped quote", `{"password":"a\\\"hunter2pass"}`, `{"password":"[скрыто:password]"}`},
		{"empty url user", "https://:secret-value@host/", "https://:[скрыто:password]@host/"},
		{"header stays on line", "Authorization: Bearer\nContent-Type: application/json", "Authorization: Bearer\nContent-Type: application/json"},
		{"password option equals name", "--password=PASSWORD", "--password=PASSWORD"},
		{"token option equals name", "--token=TOKEN", "--token=TOKEN"},
		{"api key option equals normalized name", "--api-key=API_KEY", "--api-key=API_KEY"},
		{"api key hyphen equals name", "--api-key=api-key", "--api-key=api-key"},
		{"empty flag assignment", "--password= psql", "--password= psql"},
		{"marker stays intact", "s://token=/|@", "s://token=[скрыто:token]|@"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := Mask(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func BenchmarkMaskOrdinary1MiB(b *testing.B) {
	text := strings.Repeat("ordinary event message with no credentials or command names; ", (1<<20)/59+1)[:1<<20]
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for range b.N {
		Mask(text)
	}
}

func TestMaskOverlapFindsNestedGenericKey(t *testing.T) {
	body := strings.Repeat("a1B2", 5)
	key := "sk-" + body
	text := "ta" + "sk-" + strings.Repeat("letters", 4) + "." + key
	got, found := Mask(text)
	if !strings.HasSuffix(got, ".[скрыто:sk]") || found["sk"] != 1 {
		t.Fatalf("nested generic key was not masked: length %d, counts %v", len(got), found)
	}
}

func TestMaskNewEscapedContexts(t *testing.T) {
	contexts := []string{`\u{1b}[32m`, `\U0000003d`, `^[[32m`, "␛[32m", "\x1b(B", "[32m", "[0;32m"}
	for _, context := range contexts {
		got, found := Mask(context + fakes()["sk"])
		if got != context+"[скрыто:sk]" || found["sk"] != 1 {
			t.Errorf("escaped context %q was not preserved and masked: output length %d, counts %v", context, len(got), found)
		}
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

func TestRunIndexMatchesLinearRandomRun(t *testing.T) {
	random := rand.New(rand.NewSource(19))
	for range 1000 {
		var text strings.Builder
		for n := random.Intn(100); n > 0; n-- {
			text.WriteByte("ab09-_"[random.Intn(6)])
		}
		value := text.String()
		index := newRunIndex(value)
		for start := range value {
			for end := start; end <= len(value); end++ {
				for _, minimum := range []int{16, 20} {
					if got, want := index.has(start, end, minimum), hasRandomRun(value[start:end], minimum); got != want {
						t.Fatalf("run index differs for %q at range %d:%d, minimum %d: got %v want %v", value, start, end, minimum, got, want)
					}
				}
			}
		}
	}
}

func hasRandomRun(text string, n int) bool {
	start, letter, digit := 0, false, false
	for i := 0; i <= len(text); i++ {
		if i < len(text) && isAlphaNum(text[i]) {
			letter = letter || isLetter(text[i])
			digit = digit || text[i] >= '0' && text[i] <= '9'
			continue
		}
		if i-start >= n && letter && digit {
			return true
		}
		start, letter, digit = i+1, false, false
	}
	return false
}

func TestMaskPGPPrivateKeyBlock(t *testing.T) {
	begin, end := "-----BEGIN PGP PRIVATE KEY BLOCK-----", "-----END PGP PRIVATE KEY BLOCK-----"
	body := "\n\nlQOYBGabcdefgh\n=abcd\n"
	for name, text := range map[string]string{"closed": begin + body + end + "\nafter", "open": begin + body} {
		got, found := Mask(text)
		if found["private-key"] != 1 || strings.Contains(got, "lQOYB") {
			t.Errorf("%s block: counts %v, output length %d", name, found, len(got))
		}
	}
	got, found := Mask("before\n" + begin + body + end + "\nafter")
	if got != "before\n[скрыто:private-key]\nafter" || found["private-key"] != 1 {
		t.Errorf("text around a closed block changed: counts %v, output length %d", found, len(got))
	}
	public := "-----BEGIN PGP PUBLIC KEY BLOCK-----" + body + "-----END PGP PUBLIC KEY BLOCK-----"
	if got, found := Mask(public); got != public || len(found) != 0 {
		t.Errorf("public block changed: counts %v", found)
	}
}

// A generated key may hold no digit by chance: where the prefix stands on its
// own and the body has no separators, nothing else looks like this.
func TestMaskDigitlessKeyAtCleanEdge(t *testing.T) {
	letters := func(n int) string { return strings.Repeat("aBcDeFgHiJkL", n/12+1)[:n] }
	keys := map[string]string{
		"sk" + "_live_": "sk_live", "sk" + "_test_": "sk_live", "rk" + "_live_": "sk_live", "rk" + "_test_": "sk_live", "pk" + "_live_": "sk_live", "pk" + "_test_": "sk_live",
		"gh" + "p_": "github", "gh" + "o_": "github", "gh" + "u_": "github", "gh" + "s_": "github", "gh" + "r_": "github",
		"AI" + "za": "google",
	}
	for prefix, kind := range keys {
		key := prefix + letters(36)
		for _, before := range []string{"", " ", "KEY=", "\"", "\n", `\n`, "[32m"} {
			got, found := Mask(before + key + " tail")
			if got != before+"[скрыто:"+kind+"] tail" || found[kind] != 1 {
				t.Errorf("%s after %q: counts %v, output length %d", kind, before, found, len(got))
			}
		}
		for _, before := range []string{"x", "7", "x_", "x-", "_", "-"} {
			if got, found := Mask(before + key); got != before+key || len(found) != 0 {
				t.Errorf("%s glued after %q changed: counts %v", kind, before, found)
			}
		}
	}
}

// Names and kinds whose bodies can hold words keep needing a digit.
func TestMaskDigitlessNamesStay(t *testing.T) {
	for _, text := range []string{
		"network_test_ConnectionTimeoutCase",
		"framework_test_HandlesEmptyInput",
		"my_pk_test_ConnectionTimeoutCase",
		"task_ThisIsLongDescriptiveName",
		"xo" + "xo-hugs-and-kisses-from-me",
		"xo" + "xb-hugs-and-kisses-from-me",
		"sk-proj-migration-notes-final-version",
		"sk-ant-colony-optimisation-notes-draft",
		"github" + "_pat_some_very_long_descriptive_name_here",
		"gh" + "p_short",
		"sk" + "_live_short",
	} {
		if got, found := Mask("before " + text + " after"); got != "before "+text+" after" || len(found) != 0 {
			t.Errorf("text of %d bytes changed: counts %v", len(text), found)
		}
	}
}

// A personal token prefix inside a fine-grained token must not cut it apart.
func TestMaskGithubPatWithTokenPrefixInside(t *testing.T) {
	body := "aaaaa" + "gh" + "p_" + strings.Repeat("a1B2", 10) + "BBBB"
	got, found := Mask("key github" + "_pat_" + body + " end")
	if got != "key [скрыто:github] end" || found["github"] != 1 {
		t.Errorf("output length %d, counts %v", len(got), found)
	}
}

// A digitless match cut by _ is not a key: the real key after it must not lose its prefix.
func TestMaskDigitlessMatchBeforeRealKey(t *testing.T) {
	key := "sk" + "_live_" + strings.Repeat("a1B2", 8)
	got, found := Mask("rk" + "_live_" + strings.Repeat("aBcD", 8) + key + " end")
	if !strings.HasSuffix(got, "[скрыто:sk_live] end") || found["sk_live"] != 1 || strings.Contains(got, "a1B2") {
		t.Errorf("output length %d, counts %v", len(got), found)
	}
}

func TestMaskToolAfterWrittenLineBreak(t *testing.T) {
	const value = "Vvalue123"
	for _, sep := range []string{`\n`, `\r\n`, `\t`} {
		for _, text := range []string{
			`{"content":"#!/bin/sh` + sep + `curl -u admin:Vvalue123 https://x` + sep + `"}`,
			`model "x` + sep + `curl -u admin:Vvalue123 https://x" is not approved`,
			`["#!/bin/sh` + sep + `curl -u admin:Vvalue123 https://x"]`,
			`"{\"script\": \"#!/bin/sh\` + sep + `curl -u admin:Vvalue123 https://x\"}"`,
			`{"#!/bin/sh` + sep + `curl -u admin:Vvalue123 https://x":1}`,
			`"a` + sep + `curl` + sep + `-u admin:Vvalue123 x"`,
			`"a` + sep + `mysqldump` + sep + `-pVvalue123 db"`,
			`"a` + sep + `sshpass -p Vvalue123 ssh h"`,
		} {
			if out, found := Mask(text); strings.Contains(out, value) || len(found) == 0 {
				t.Errorf("value kept: %q -> %q %v", text, out, found)
			}
		}
	}
	for _, text := range []string{
		`printf 'a\nb'`,
		`echo "x\ncurl https://x"`,
		`C:\new\curl`,
		`C:\tmp\curl -u a:b`,
		`"a\ncurly -u a:b x"`,
	} {
		if out, found := Mask(text); out != text || len(found) != 0 {
			t.Errorf("changed: %q -> %q %v", text, out, found)
		}
	}
}

// A bare -p of mysql asks for the password, so the word after it is no value:
// a written line break behaves here as a real one and as a space.
func TestMaskBareMysqlFlagBeforeWrittenLineBreak(t *testing.T) {
	for _, sep := range []string{` `, "\n", `\n`, `\\n`, `\t`} {
		text := `mysql -p` + sep + `Vvalue123 db`
		if out, found := Mask(text); out != text || len(found) != 0 {
			t.Errorf("changed: %q -> %q %v", text, out, found)
		}
	}
}

func TestMaskLinearOnBackslashSeries(t *testing.T) {
	const size = 1 << 20
	series := strings.Repeat(`\`, size)
	for _, text := range []string{`curl ` + series + `x`, `curl ` + series + `n`, series, strings.Repeat(`curl \\\\x `, size/10), `curl ` + strings.Repeat(`\\\n`, size/4)} {
		start := time.Now()
		Mask(text)
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("%.12q: %v on 1 MiB", text, took)
		}
	}
}

// Keys of this form can carry _ and - inside the body: a mask that stops at the
// first separator would print the rest of the key.
func TestMaskLiveKeyBodyWithSeparators(t *testing.T) {
	r := func(s string, n int) string { return strings.Repeat(s, n) }
	tail := "_" + r("Ab", 2) + "_" + r("c1", 2) + "-" + r("Xy9", 3)
	for _, prefix := range []string{"sk" + "_live_", "sk" + "_test_", "rk" + "_live_", "pk" + "_test_"} {
		key := prefix + r("a1B2", 6) + tail
		got, found := Mask("before " + key + " after")
		if got != "before [скрыто:sk_live] after" || !maps.Equal(found, map[string]int{"sk_live": 1}) {
			t.Errorf("%s: output length %d, tail left %v, counts %v", prefix, len(got), strings.Contains(got, r("Xy9", 3)), found)
		}
	}
	for _, name := range []string{"pk" + "_test_returns_value_when_missing_key", "rk" + "_live_short-name"} {
		if got, found := Mask(name); got != name || len(found) != 0 {
			t.Errorf("plain name %q changed: %q, %v", name, got, found)
		}
	}
}

// A secret name glued to a key stays for the context rule, or the value after it
// would be printed.
func TestMaskLiveKeyGluedAssignmentKeepsValueHidden(t *testing.T) {
	key := "sk" + "_live_" + strings.Repeat("a1B2", 6)
	const value = "hunter2value"
	for _, name := range []string{"_password=", "-token: ", "_PASSWORD=", "_api_key="} {
		got, _ := Mask(key + name + value)
		if strings.Contains(got, value) || strings.Contains(got, key) {
			t.Errorf("%q: value or key left, output length %d", name, len(got))
		}
	}
	// With no delimiter after it the same word is the rest of the key.
	if got, _ := Mask(key + "_token rest"); got != "[скрыто:sk_live] rest" {
		t.Errorf("secret-looking tail without a delimiter left: output length %d", len(got))
	}
	// A body going on before a delimiter is still the key, not a name.
	tail := "_Ab12_cD34-eF56"
	if got, _ := Mask(key + tail + "=rest"); strings.Contains(got, tail) {
		t.Errorf("tail before a delimiter left: output length %d", len(got))
	}
}

func TestMaskLinearOnGluedKeyChains(t *testing.T) {
	key := "sk" + "_live_" + strings.Repeat("a1B2", 6)
	const size = 1 << 20
	for _, glue := range []string{"_.", "_a ", "-", "_token "} {
		text := strings.Repeat(key+glue, size/(len(key)+len(glue)))
		start := time.Now()
		Mask(text)
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("glue %q: %v on 1 MiB", glue, took)
		}
	}
}

// Output differs from the context rules' only by the removed rest of a key, so
// no construction glued to a key loses the masking of its value.
func TestMaskLiveKeyGluedConstructionsKeepValueHidden(t *testing.T) {
	key := "sk" + "_live_" + strings.Repeat("a1B2", 6)
	const value = "hunter2value"
	for _, glued := range []string{"_authorization: Bearer ", "-Proxy-Authorization: Basic ", "_authorization=", "-Authorization: ", "-https://user:", "_mysql://user:", "-ftp://u:"} {
		tail := "@host/x"
		if !strings.Contains(glued, "://") {
			tail = ""
		}
		got, _ := Mask(key + glued + value + tail)
		if strings.Contains(got, value) || strings.Contains(got, key) {
			t.Errorf("%q: value or key left, output length %d", glued, len(got))
		}
	}
	marker := "[скрыто:sk_live]"
	if got, found := Mask("x " + marker + "_rest y"); got != "x "+marker+"_rest y" || len(found) != 0 {
		t.Errorf("a marker already in the text changed: %q %v", got, found)
	}
	if got, found := Mask(key + "_" + key); got != marker+marker || found["sk_live"] != 2 {
		t.Errorf("two glued keys: output length %d, counts %v", len(got), found)
	}
}

func TestMaskLinearOnKeyRunEndingInName(t *testing.T) {
	key := "sk" + "_live_" + strings.Repeat("a1B2", 6)
	const size = 1 << 20
	for _, glue := range []string{"_", "-"} {
		for _, end := range []string{"token=x", "password:x"} {
			text := strings.Repeat(key+glue, size/(len(key)+1)) + end
			start := time.Now()
			Mask(text)
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("glue %q, end %q: %v on 1 MiB", glue, end, took)
			}
		}
	}
}

// A sed expression that reads a variable is not its value, at any escaping depth,
// and a real value inside the same JSON string is still hidden.
func TestMaskKeepsJSONValid(t *testing.T) {
	for _, tc := range []struct{ name, input, hidden string }{
		{"sed, one level", `{"cmd":"sudo sed -n \"s/^NAME_SECRET=\\(.*\\)$/\\1/p\" /opt/x/.env"}`, ""},
		{"sed, two levels", `{"cmd":"sudo sed -n \\\"s/^NAME_SECRET=\\\\(.*\\\\)$/\\\\1/p\\\" /opt/x/.env"}`, ""},
		{"real value in escaped quotes", `{"cmd":"x --password=\"hunter2pass\" y"}`, "hunter2pass"},
		{"real value behind an escaped quote", `{"password":"a\\\"hunter2pass"}`, "hunter2pass"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !json.Valid([]byte(tc.input)) {
				t.Fatalf("bad fixture: %s", tc.input)
			}
			got, _ := Mask(tc.input)
			if !json.Valid([]byte(got)) {
				t.Errorf("not valid after the mask: %s", got)
			}
			if tc.hidden == "" && got != tc.input {
				t.Errorf("a command with no value was changed: %s", got)
			}
			if tc.hidden != "" && strings.Contains(got, tc.hidden) {
				t.Errorf("value left: %s", got)
			}
		})
	}
}
