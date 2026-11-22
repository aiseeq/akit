package secretmask

import (
	"strings"
	"testing"
)

func TestMaskCodeExpressionValuesAreReferences(t *testing.T) {
	for _, input := range []string{
		`c = Credentials(token=d.get("token"), refresh_token=d["refresh_token"], token_uri=d["token_uri"], client_id=d["client_id"], client_secret=d["client_secret"], scopes=d.get("scopes"))`,
		`c = Credentials(token=d.get("token"), refresh_token=d["refresh_token"], token_uri=d["token_uri"],` + "\n" +
			`                client_id=d["client_id"], client_secret=d["client_secret"], scopes=d.get("scopes"))`,
		`token=cfg.secrets["x"]`,
		`token=os.environ["TOKEN"]`,
		`token=os.getenv("TOKEN")`,
	} {
		t.Run(input, func(t *testing.T) {
			if got, found := Mask(input); got != input || len(found) != 0 {
				t.Fatalf("Mask changed code reference: %q -> %q (%v)", input, got, found)
			}
		})
	}
}

func TestMaskCodeReferenceNameThatLooksSecret(t *testing.T) {
	glpat := "glpat-" + "abcd1234Efgh5678Ijkl9012"
	for _, tc := range []struct{ input, want string }{
		{`token=Zq9xW8vT7sR6qP5mN4(expired)`, `token=Zq9xW8vT7sR6qP5mN4(expired)`},
		{`password=d["Zq9xW8vT7sR6qP5mN4abc"]`, `password=[скрыто:password]"Zq9xW8vT7sR6qP5mN4abc"]`},
		{`password=hunter2long(old)`, `password=hunter2long(old)`},
		{`API_KEY=Zq9xW8vT7sR6qP5mN4abcd[1]`, `API_KEY=[скрыто:token]`},
		{`token=d[Zq9xW8vT7sR6qP5mN4abc]`, `token=[скрыто:token]`},
		{`token=opts[` + glpat + `]`, `token=[скрыто:token]`},
		{`password=admin[123]`, `password=[скрыто:password]`},
		{`password=Summer[2024]`, `password=[скрыто:password]`},
		{`DB_PASSWORD=Welcome[1]!`, `DB_PASSWORD=[скрыто:password]`},
		{`**Password**: Summer[2024]`, `**Password**: [скрыто:password]`},
		{`credentials = master6x[];`, `credentials = [скрыто:token];`},
		{`secret: qwerty3["letmein6"]`, `secret: [скрыто:token]"letmein6"]`},
		{`token=Zq9xW8vT7sR6qP5mN4[Zq9xW8vT7sR6qP5mN4abc]`, `token=[скрыто:token]`},
		{`token=sha256("x")`, `token=sha256("x")`},
		{`password=getpass("Password: ")`, `password=getpass("Password: ")`},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if got, _ := Mask(tc.input); got != tc.want {
				t.Fatalf("Mask(%q) = %q, want base-compatible %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestMaskCodeReferenceKeepsShortCredentialName(t *testing.T) {
	const input = `credentials=c,`
	if got, found := Mask(input); got != input || len(found) != 0 {
		t.Fatalf("Mask(%q) = %q (%v), want unchanged", input, got, found)
	}
}

func TestMaskCodeReferenceR4RejectsMixedCaseDigitPathNames(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`- client_secret:self.fC97qKo['GETENV_KEY']`, `- client_secret:[скрыто:token]'GETENV_KEY']`},
		{`private_key:g4CggGh['LOAD_SECRET_KEY'])`, `private_key:[скрыто:token]'LOAD_SECRET_KEY'])`},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if got, _ := Mask(tc.input); got != tc.want {
				t.Fatalf("Mask(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestMaskCodeReferenceR4RequiresKnownSinglePartContainer(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`client_secret=opensesame["hunterlong"]`, `client_secret=[скрыто:token]"hunterlong"]`},
		{`client_secret=Secret["hunterlong"]`, `client_secret=[скрыто:token]"hunterlong"]`},
		{`token=opts["TOKEN"]`, `token=opts["TOKEN"]`},
		{`token=cfg2["x"]`, `token=cfg2["x"]`},
		{`token=v2["x"]`, `token=v2["x"]`},
		{`token=my_cfg7["x"]`, `token=my_cfg7["x"]`},
		{`token=cfg.camelCase["x"]`, `token=[скрыто:token]"x"]`},
		{`token=data.opensesame["x"]`, `token=[скрыто:token]"x"]`},
		{`--secret:getenv.Secret['load_secret']`, `--secret:[скрыто:token]'load_secret']`},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if got, _ := Mask(tc.input); got != tc.want {
				t.Fatalf("Mask(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestCodeReferenceScanStopsWithin256Bytes(t *testing.T) {
	expression := `cfg.get("x", ` + strings.Repeat(" ", 250) + `fallback)`
	value := contextValue{start: 0, end: len(expression)}
	if codeReferenceExpression(expression, value, 0, "token", "token", "=") {
		t.Fatalf("accepted expression whose closing delimiter is beyond 256 bytes")
	}
}

func TestCodeReferenceRequiresKnownSinglePartContainer(t *testing.T) {
	expression := `undefined("x")`
	value := contextValue{start: 0, end: len(expression)}
	if codeReferenceExpression(expression, value, 0, "token", "token", "=") {
		t.Fatalf("accepted unknown one-part call as a reference")
	}
}

func TestCodeReferenceRejectsMixedCaseDigitPathName(t *testing.T) {
	expression := `foo.aB3["KEY"]`
	value := contextValue{start: 0, end: len(expression)}
	if codeReferenceExpression(expression, value, 0, "token", "token", "=") {
		t.Fatalf("accepted mixed-case identifier containing digits")
	}
}

func TestCodeReferenceRejectsTrailingTextAndSecretGetFallback(t *testing.T) {
	for _, expression := range []string{
		`cfg.get("x")suffix`,
		`cfg.get("token", "Zq9xW8vT7sR6qP5mN4")`,
	} {
		value := contextValue{start: 0, end: len(expression)}
		if codeReferenceExpression(expression, value, 0, "token", "token", "=") {
			t.Errorf("accepted non-reference expression %q", expression)
		}
	}
}

func TestMaskCodeExpressionLiteralsAndSecretIndex(t *testing.T) {
	secretIndex := "sk_live_" + strings.Repeat("1", 24)
	for _, input := range []string{
		`client_secret="Zq9xW8vT7sR6qP5m"`,
		`token: Zq9xW8vT7sR6qP5m`,
		`password=hunter2long`,
		`refresh_token='Zq9xW8vT7sR6qP5m'`,
		`token=d["` + secretIndex + `"]`,
	} {
		t.Run(input, func(t *testing.T) {
			got, found := Mask(input)
			if got == input || len(found) == 0 || strings.Contains(got, "Zq9xW8vT7sR6qP5m") || strings.Contains(got, "hunter2long") || strings.Contains(got, secretIndex) {
				t.Fatalf("secret literal was not masked: %q -> %q (%v)", input, got, found)
			}
		})
	}
}
