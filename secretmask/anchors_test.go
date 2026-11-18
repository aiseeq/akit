package secretmask

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Neighbouring forms of the tool, URL and JSON rules: the value goes, the JSON
// stays valid.
func TestMaskToolAndJSONNeighbourForms(t *testing.T) {
	for _, input := range []string{
		`{"content":"echo Vvalue123 | sudo -S ls"}`,
		`echo -n 'Vvalue123' | sudo -S ls`,
		`echo Vvalue123 | sudo -u root -S ls`,
		`echo Vvalue123 | sudo -kS ls`,
		`{"api_token":["Wvalue456","Vvalue123"]}`,
		`{"password":["Vvalue123"]}`,
		`{"db_password":{"value":"Vvalue123"}}`,
		`{"content":"sshpass -V; sshpass -p Vvalue123 ssh h"}`,
		`{"argv":["/usr/bin/curl","-su","admin:Vvalue123"]}`,
		`{"cmd":"redis-cli -a Vvalue123 ping"}`,
		`docker login -u x -p Vvalue123 reg`,
		`git clone https://Vvalue123-deploy@host/x`,
		"export DB_PASSWORD1=Vvalue123",
		"tool --db_password Vvalue123",
	} {
		t.Run(input, func(t *testing.T) {
			got, _ := Mask(input)
			if strings.Contains(got, formValue) {
				t.Fatalf("value left in %q", got)
			}
			if json.Valid([]byte(input)) && !json.Valid([]byte(got)) {
				t.Fatalf("JSON broken: %q", got)
			}
		})
	}
}

func TestMaskToolAndJSONNeighboursStay(t *testing.T) {
	for _, input := range []string{
		`printf Vvalue123 | sudo -S ls`,
		`echo "$PASS" | sudo -S ls`,
		`echo Vvalue123 || sudo -S ls`,
		`{"token":{"type":"bearer"}}`,
		`{"value":"x","name":"password"}`,
		`{"args":["--password","$PW"]}`,
		`{"args":["--password","--verbose"]}`,
		`git clone https://deploy@host/x`,
		`no credentials here, the token count, 12 passed`,
		`credentials.json`,
		`{"input_tokens":5,"output_tokens":7}`,
		`pwd`,
	} {
		t.Run(input, func(t *testing.T) {
			got, found := Mask(input)
			if got != input || len(found) != 0 {
				t.Fatalf("changed to %q, %v", got, found)
			}
		})
	}
}

// The filter takes a secret name only where it is used as one, and a command
// word only at a command edge.
func TestContextAnchorGroups(t *testing.T) {
	for _, tc := range []struct {
		text string
		want anchorGroup
	}{
		{"the tokens passed through the high gate", 0},
		{"no credentials or command names; pseudo curly", 0},
		{"token: x", anchorName},
		{`{"api_token1":"x"}`, anchorName},
		{"--db_password x", anchorName},
		{"Password: x", anchorName},
		{"password for admin: x", anchorEnCandidate},
		{`["--token","x"]`, anchorName},
		{`{"name":"auth","value":"x"}`, anchorName},
		{"see https://x", anchorURL},
		{"Authorization: x", anchorHeader},
		{`/usr/bin/curl -u a:b`, anchorCurl},
		{`"a\nsudo -S ls"`, anchorTool},
		{"mysqldump -px", anchorTool},
		{"gh auth login", anchorTool},
	} {
		if got := contextAnchorGroups(tc.text); got != tc.want {
			t.Errorf("%q: groups %b, want %b", tc.text, got, tc.want)
		}
	}
}

func TestMaskRussianLabels(t *testing.T) {
	secret := "Zq7xW9vK2mPq8Rt"
	quoted := `"токен": ` + secret
	if ix := contextRussianNameRE.FindStringIndex(quoted); ix == nil || ix[0] != 0 {
		t.Fatalf("Russian quoted label was not matched from opening quote: %v", ix)
	}
	for _, label := range []string{
		"пароль", "Пароль", "ПАРОЛЬ", "паРОЛЬ", "ПаРоЛь", "пароля", "паролем",
		"токен", "Токен", "ТОКЕН", "тОкЕн", "токена",
		"ключ", "Ключ", "КЛЮЧ", "КлЮч",
	} {
		input := label + ": " + secret
		got, _ := Mask(input)
		if strings.Contains(got, secret) {
			t.Errorf("secret after Russian label %q remained visible", label)
		}
	}
	for _, input := range []string{
		"sk-" + strings.Repeat("a1B", 12) + " пароль: " + secret,
		`["--password","Abc123Xyz789"] пароль: ` + secret,
		`["--password","Abc123Xyz789Abc123Xyz789"] пароль: ` + secret,
		`["--password","Abc123"] пароль: ` + secret,
		`["--password","Abc123Xyz789"] ["--password","Abc123"] пароль: ` + secret,
		`["--password","Abc123"] пароль: ` + secret + ` ["--password","` + strings.Repeat("Abc123Xyz789", 3) + `Abc1"]`,
		`пароль: ` + secret + ` ["--password","Abc123"]`,
		"автопароль пароль: " + secret,
		"Пароль учётки: " + secret,
		"тОкЕн= " + secret,
		"КЛЮЧ: " + secret,
		"пароль:токен: " + secret,
		"пароль для этой учётной записи: " + secret,
		"пароль для учётной записи— " + secret,
		"пароль— " + secret,
		"пароль– " + secret,
		"пароль - " + secret,
		"пароль= " + secret,
		"пароль:= " + secret,
		"пароль=> " + secret,
		"**Пароль:** " + secret,
		"__пароля__: " + secret,
		"`паролем`: " + secret,
		"«токен»: " + secret,
		`"токена": "` + secret + `"`,
		`'ключ': "` + secret + `"`,
		"ключ и пароль: " + secret,
		"ключ и токен: " + secret,
	} {
		got, _ := Mask(input)
		if strings.Contains(got, secret) {
			t.Errorf("secret value remained visible for Russian label form %q", input)
		}
	}
	for _, input := range []string{
		"пароль не подошёл",
		"пароль: нет",
		"токенов: 1200",
		"пароль: 1200",
		"токен: 1200",
		"токен: абвгдежз",
		"ключ: " + strings.Join([]string{"SI", "1234"}, "-"),
		"ключ: incident-reference",
		"ключ: abcdefghij123456",
		"ключ: Abcdefghijklmnop",
		"супертокен: " + secret,
		"_ключ: " + secret,
		"сверхпароль: " + secret,
		"электроключ: " + secret,
		"пароль для учётной записи\n" + secret,
	} {
		if got, found := Mask(input); got != input || len(found) != 0 {
			t.Errorf("non-secret Russian form changed %q to %q", input, got)
		}
	}
	if got, found := Mask("ключ: aB4cD9eF0gH2"); strings.Contains(got, "aB4cD9eF0gH2") || found["token"] == 0 {
		t.Errorf("token-shaped key value not masked: %q, %v", got, found)
	}
}

func TestMaskRussianLabelsKeepCyrillicProse(t *testing.T) {
	for _, input := range []string{
		"- Описание расхода токенов: показывать сумму по тарифу API.",
		"Токен: обновлять раз в сутки",
		"токен: обыкновенное слово без ё",
		"пароль: Замечательное",
		"пароль: ёжик в тумане",
		"пароль: ёжикзамечательный",
	} {
		if got, found := Mask(input); got != input || len(found) != 0 {
			t.Errorf("prose changed from %q to %q, counts %v", input, got, found)
		}
	}
	for _, input := range []string{
		"токен: a1B2c3D4e5F6g7H8",
		"пароль: Zq8wX2pLm",
		"пароль: КрасныйСлон",
		"токен: КрасныйSecret",
		"пароль: КрасныйSecret",
		"пароль: обычноеword",
	} {
		if got, found := Mask(input); got == input || len(found) == 0 {
			t.Errorf("secret value was not masked in %q", input)
		}
	}
}

func TestMaskCodeAssignmentsKeepExpressions(t *testing.T) {
	for _, input := range []string{
		"const pass = typeof want === 'string';",
		"let pass = await check(x);",
		"pass = new Checker(cfg);",
		"const token = await fetchToken();",
		"let secret = process.env.APP_SECRET;",
		"let pwd = await x.y();",
		"secret = await vault.read(path)",
		"api_key = env.API_KEY",
		"password = settings.SECRET_KEY",
		"hash = user.password_hash",
		`password = typeof want === "function";`,
		"let pwd = check(x);",
	} {
		if got, found := Mask(input); got != input || len(found) != 0 {
			t.Errorf("expression changed from %q to %q, counts %v", input, got, found)
		}
	}
	for _, input := range []string{
		`const pass = "Xk9#mQ2vL7";`,
		`const pass = "typeof";`,
		`token = "sk.eyJ1IjoiZXhhbXBsZSJ9"`,
		`token = "settings.SECRET_KEY"`,
		"pass = Xk9mQ2vL7",
		"password: hunter2",
		"DB password: Zq8wX2pLm",
		"export PASS=Xk9mQ2vL7",
		"token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"token=aB3_dE5fG7.hJ9kL2mN4_pQ6rS8",
		"secret=Xk9_mQ2.vL7",
		"api_key=a1b2c3d4e5f6_g7h8.i9",
		"password=my_secret.v2",
		"password=new",
		"password=typeof",
		"password = typeof;",
		"password=await",
		"password: process.env.APP_SECRET",
		"api_key=env.API2_KEY",
		"token=env.API2",
		"token=x.ABCDEF0123456789",
		"api_key=live.4F3A9B2C7D1E6F80",
		"secret=prod.QWERTYUIOP234567",
		"otp_secret=totp.JBSWY3DPEHPK3PXP",
		"password=Hello.WORLD1",
		"password=Summer.SALE2024",
		"VAULT_TOKEN=hvs.CAESIJxK9mQ2vL7wP3aB5dE7fG9hJ1kL3mN5pQ7rS9tU1vW3",
		"VAULT_TOKEN=s.Xk9mQ2vL7wP3aB5dE7fG9hJ1",
		"token=hvb.AAAAAQKx9mQ2vL7wP3aB5dE7fG9hJ1kL3",
		"MAPBOX_TOKEN=sk.eyJ1IjoiZXhhbXBsZSIsImEiOiJ4eXoifQ.Xk9mQ2vL7wP3aB5d",
		"mapbox_token=pk.eyJ1IjoiZXhhbXBsZSIsImEiOiJ4eXoifQ.Xk9mQ2vL7wP3aB5d",
		"DOPPLER_TOKEN=dp.st.dev.Xk9mQ2vL7wP3aB5dE7fG9hJ1kL3",
		"secret=abc.Xk9mQ2vL7wP3",
		"token=x.Xk9mQ2vL7wP3aB5d",
		"api_key=key.Xk9mQ2vL7wP3aB5d",
		"password=mysecret.v2",
		"secret=Xk9mQ2vL7.ABCDEF1234",
		"secret=foo.bar_..BARX",
		"secret=foo.bar-baz",
		"secret=settings.1234_",
		"secret=USER.password_hash",
		"password=new Pass2024",
		"password = await Xk9mQ2vL7",
		"password = new;",
		"password = typeof Xk9mQ2vL7",
		"password = typeof x",
		"password = typeof;",
		"password = typeof ;",
		"password = await 1X.foo",
		"password = new 1X.foo",
		"password = await x+secret",
		"password = new X+secret",
		"password: new",
	} {
		if got, found := Mask(input); got == input || len(found) == 0 {
			t.Errorf("secret value was not masked in %q", input)
		}
	}
}

func TestMaskInvalidKeywordAssignmentMasksWholeValue(t *testing.T) {
	for input, want := range map[string]string{
		"password = await Xk9mQ2vL7":  "password = [скрыто:password]",
		"password = new Pass2024":     "password = [скрыто:password]",
		"password = typeof Xk9mQ2vL7": "password = [скрыто:password]",
		"password = typeof x":         "password = [скрыто:password]",
		"password = new;":             "password = [скрыто:password];",
		"password = typeof;":          "password = [скрыто:password];",
		"password = typeof ;":         "password = [скрыто:password] ;",
		"password = await 1X.foo":     "password = [скрыто:password]",
		"password = new 1X.foo":       "password = [скрыто:password]",
		"password = await x+secret":   "password = [скрыто:password]",
		"password = new X+secret":     "password = [скрыто:password]",
	} {
		if got, found := Mask(input); got != want || found["password"] != 1 {
			t.Errorf("got %q, counts %v; want %q", got, found, want)
		}
	}
}

func TestCodeAssignmentContinuationRules(t *testing.T) {
	for input, want := range map[string]bool{
		"await check(x)":               true,
		"await vault.read(path)":       true,
		"await x.y()":                  true,
		"await 1X.foo":                 false,
		"await x+secret":               false,
		"await x":                      false,
		"new Checker(cfg)":             true,
		"new object.field":             true,
		"new 1X.foo":                   false,
		"new X+secret":                 false,
		"new X":                        false,
		"typeof want === \"function\"": true,
		"typeof object.field":          true,
		"typeof Xk9mQ2vL7":             false,
		"typeof x":                     false,
		"typeof ;":                     false,
	} {
		keyword, _, _ := strings.Cut(input, " ")
		if keyword == input {
			keyword = strings.TrimSuffix(input, ";")
		}
		value := contextValue{start: 0, end: len(keyword)}
		if got := codeAssignmentExpression(input, value, "="); got != want {
			t.Errorf("%q: code expression = %v, want %v", input, got, want)
		}
	}
}

func TestMaskEnglishPasswordLabelPhrases(t *testing.T) {
	secret := "Zq7xW9vK2mPq8Rt"
	inputs := []string{
		"**Password:** " + secret,
		"__Password__: " + secret,
		"`Password`: " + secret,
		"password for the account: " + secret,
		"password for the current user: " + secret,
		"password for the\naccount: " + secret,
		"password for = the account: " + secret,
		"password - the account: " + secret,
		"password for the current user account: " + secret,
		"password" + strings.Repeat(" ", 512) + ": " + secret,
	}
	for _, input := range inputs {
		t.Run(input[:min(len(input), 40)], func(t *testing.T) {
			got, found := Mask(input)
			if strings.Contains(got, secret) || found["password"] != 1 {
				t.Fatalf("English label did not hide its value: output %q, counts %v", got, found)
			}
		})
	}
}

func TestMaskEnglishPasswordLabelGrammar(t *testing.T) {
	secret := "Zq7xW9vK2mPq8Rt"
	inputs := []string{
		"password for the database: " + secret,
		"passwd for the account: " + secret,
		"pwd of the account: " + secret,
		"passphrase for this account: " + secret,
		"password for admin: " + secret,
		"password of the account: " + secret,
		"password\nfor the account: " + secret,
		"password for\nthe account: " + secret,
		"password for the \naccount: " + secret,
		"password for the\r\naccount: " + secret,
		"- password for admin: " + secret,
		"Password for the user's (admin) account: " + secret,
		"DB password: " + secret,
		"DB Root Password: " + secret,
		"Root Password: " + secret,
		"Admin password: " + secret,
		"db_password for the account: " + secret,
		"**Password for admin:** " + secret,
		"**Password for admin**: " + secret,
		"***Password:*** " + secret,
		"***Password***: " + secret,
		"*Password:* " + secret,
		"*Password*: " + secret,
		"_Password_: " + secret,
		"~~Password~~: " + secret,
		"~~Password:~~ " + secret,
		"[Password]: " + secret,
		"«Password»: " + secret,
		`"Password": ` + secret,
		"**DB password:** " + secret,
		"**Database Password:** " + secret,
		"**Root Password**: " + secret,
		"*DB password*: " + secret,
		`"DB password for admin": ` + secret,
		"password for the account\n: " + secret,
		"password for the\naccount: " + secret,
		"password : " + secret,
		"password \n: " + secret,
		"password\n: " + secret,
		"password for " + strings.Repeat("a", 80) + ": " + secret,
		"password for (superadministrator): " + secret,
		"**New admin password**: " + secret,
		"- **New admin password**: " + secret,
		"db_password - the account: " + secret,
		"- db_password\n: " + secret,
		"- __db_password for = the account__\n: " + secret,
		"__db_password for = the account__\n: " + secret,
		`"Password"` + "\n: " + secret,
		"**Password:** " + secret,
		"__Password__: " + secret,
		"`Password`: " + secret,
		"password" + strings.Repeat(" ", 512) + ": " + secret,
	}
	for _, input := range inputs {
		t.Run(input[:min(len(input), 48)], func(t *testing.T) {
			got, found := Mask(input)
			if strings.Contains(got, secret) || found["password"] != 1 {
				t.Fatalf("label value remained or count differed: output %q, counts %v", got, found)
			}
		})
	}
}

func TestEnglishPasswordLabelMatchesOrdinaryValueRules(t *testing.T) {
	for _, value := range []string{"hunter2", "changeme", `"literalValue123"`, "cfg.Token", "${PASSWORD}"} {
		plain, _ := Mask("Password: " + value)
		phrase, _ := Mask("password for admin: " + value)
		plainMasked := strings.Contains(plain, "[скрыто:password]")
		phraseMasked := strings.Contains(phrase, "[скрыто:password]")
		if phraseMasked != plainMasked {
			t.Errorf("phrase changed ordinary value behavior for %q: got %q, Password: gives %q", value, phrase, plain)
		}
	}
	for _, value := range []string{"see", "at", "required"} {
		input := "password for admin: " + value
		if got, found := Mask(input); got != input || len(found) != 0 {
			t.Errorf("nonsecret bare value changed: %q -> %q, counts %v", input, got, found)
		}
	}
	for _, input := range []string{
		"password for admin := load",
		"password for admin => load",
	} {
		if got, found := Mask(input); got != input || len(found) != 0 {
			t.Errorf("bare code expression changed: %q -> %q, counts %v", input, got, found)
		}
	}
	for _, input := range []string{
		`password for admin := "literalValue123"`,
		`password for admin => "literalValue123"`,
	} {
		got, found := Mask(input)
		if strings.Contains(got, "literalValue123") || found["password"] != 1 {
			t.Errorf("quoted code literal was not hidden: %q, counts %v", got, found)
		}
	}
}

func TestEnglishPasswordLabelsDoNotCaptureOtherSecretNames(t *testing.T) {
	input := "password for admin: Zq7xW9vK2mPq8Rt; token: Zq7xW9vK2mPq8Rt"
	got, found := Mask(input)
	if strings.Contains(got, "Zq7xW9vK2mPq8Rt") || found["password"] != 1 || found["token"] != 1 {
		t.Errorf("password phrase consumed a neighbouring token label: %q, counts %v", got, found)
	}
}

func TestEnglishPasswordLabelNeighboursStay(t *testing.T) {
	for _, input := range []string{
		"password for the account is managed by SSO",
		"Password reset link: https://example.com/reset",
	} {
		got, found := Mask(input)
		if got != input || len(found) != 0 {
			t.Errorf("ordinary text changed from %q to %q, counts %v", input, got, found)
		}
	}
}

func TestEnglishPasswordLabelKeepsMainNeighbourBehaviour(t *testing.T) {
	input := "the password: field is required"
	want := "the password: [скрыто:password] is required"
	if got, found := Mask(input); got != want || found["password"] != 1 {
		t.Errorf("main neighbour behaviour changed: got %q, counts %v", got, found)
	}
}

func TestEnglishPasswordQuotedLabels(t *testing.T) {
	secret := "Zq7xW9vK2mPq8Rt"
	for _, input := range []string{
		`{"text":"password for the account: ` + secret + `"}`,
		`{"text":"password for the account: ` + secret + `}`,
		`"password for the account: ` + secret + `"`,
		`echo "Password for admin: ` + secret,
		`echo "Password for admin: ` + secret + `"`,
		`msg = "password for the database: ` + secret + `"`,
	} {
		got, found := Mask(input)
		if strings.Contains(got, secret) || found["password"] != 1 {
			t.Errorf("quoted English label did not hide its value: %q, counts %v", input, found)
		}
	}
}

func TestEnglishPasswordPrefixesStayOnTheirLine(t *testing.T) {
	secret := "Zq7xW9vK2mPq8Rt"
	for _, input := range []string{
		"db_password - the account: " + secret,
		"`Password:` " + secret,
		`"Password:" ` + secret,
		"`DB password:` " + secret,
		"__Password:__ " + secret,
	} {
		got, found := Mask(input)
		if strings.Contains(got, secret) || found["password"] == 0 {
			t.Errorf("English wrapped label did not hide its value: %q -> %q, counts %v", input, got, found)
		}
	}

	for _, rows := range [][]string{
		{"x", "db_password - the account: " + secret},
		{"hello", "db_password - the account: " + secret},
		{"## Section", "db_password - the account: " + secret},
		{"Note: **important", "password for admin: " + secret},
		{"See **bold", "password for admin: " + secret},
		{"Use `make", "password for admin: " + secret},
		{"secret: **required", "hello", "password for admin: " + secret},
	} {
		joined, _ := Mask(strings.Join(rows, "\n"))
		separate := make([]string, len(rows))
		for i, row := range rows {
			separate[i], _ = Mask(row)
		}
		if joined != strings.Join(separate, "\n") || strings.Contains(joined, secret) {
			t.Errorf("prefix parsing crossed a line boundary for %q: joined %q, separate %q", rows, joined, strings.Join(separate, "\n"))
		}
	}
}

func TestMarkdownLabelsForOtherMainSecretNames(t *testing.T) {
	secret := "Zq7xW9vK2mPq8Rt"
	for _, input := range []string{
		"**Token:** " + secret,
		"**Secret:** " + secret,
		"`api_key`: " + secret,
		"__Token__: " + secret,
		"**Token**\n: " + secret,
		"`api_key`\n: " + secret,
	} {
		got, found := Mask(input)
		if strings.Contains(got, secret) || len(found) != 1 {
			t.Errorf("Markdown label value remained visible: %q -> %q, counts %v", input, got, found)
		}
	}
	wrapped, _ := Mask("**Token:** see the vault")
	if wrapped != "**Token:** see the vault" {
		t.Errorf("Markdown changed ordinary Token value handling: got %q", wrapped)
	}
}

func TestEnglishPasswordLabelMalformedWrappersAndValuesStay(t *testing.T) {
	secret := "Zq7xW9vK2mPq8Rt"
	for _, input := range []string{
		"**password for the account: " + secret,
		"password for the account**: " + secret,
		"password for the account: https://example.com/reset",
		"password for the account: **required",
		"password\nfor\nthe account: " + secret,
		"mypassword for the account: " + secret,
		"1password for the account: " + secret,
		"password\nfor the\naccount: " + secret,
		"password\nfor\nthe account: " + secret,
		"password for the `account`: " + secret,
		"**Password**\n\n: " + secret,
		"[скрыто:password] for the account: " + secret,
	} {
		got, found := Mask(input)
		if got != input || len(found) != 0 {
			t.Errorf("malformed or ordinary form changed: %q -> %q, counts %v", input, got, found)
		}
	}
}

func TestEnglishPasswordPhraseLimitDoesNotCaptureProse(t *testing.T) {
	for _, input := range []string{
		"password for a b c d e: see the vault",
		"password for the main admin user account: see the vault",
	} {
		got, found := Mask(input)
		if got != input || len(found) != 0 {
			t.Errorf("out-of-limit phrase changed prose: %q -> %q, counts %v", input, got, found)
		}
	}
}

func TestEnglishLabelsStayOnSameLineValue(t *testing.T) {
	for _, input := range []string{
		"`password`:\n  description: user password",
		"`password`:\n  minLength: 12",
		"password for the account:\nUsername: admin",
		"password\nfor the account:\nZq7xW9vK2mPq8Rt",
	} {
		got, found := Mask(input)
		if got != input || len(found) != 0 {
			t.Errorf("label consumed next-line value: %q -> %q, counts %v", input, got, found)
		}
	}
}

func TestEnglishDocumentationValuesStayUnchanged(t *testing.T) {
	for _, input := range []string{
		"**Password:** TBD",
		"**Password:** —",
		"**Password:** Updated",
		"**Password:** see the vault",
		"**Password:** N/A",
		"`password`:\n  description: user password",
		"`password`:\n  minLength: 12",
		"password for the account:\nUsername: admin",
		"**Password**: The password for the database user.",
		"`password`: the user's password",
		"- `password`: New password for the user.",
		"`password`: Updated",
		"`password`: Required. The password for the account.",
	} {
		got, found := Mask(input)
		if got != input || len(found) != 0 {
			t.Errorf("documentation value changed: %q -> %q, counts %v", input, got, found)
		}
	}
}

func TestEnglishPasswordDocumentationFastPathStaysUnchanged(t *testing.T) {
	for _, input := range []string{
		`"PASSWORD"` + "\n: at least 12 characters",
		`"PASSWORD"` + strings.Repeat(" ", 40) + "\n: see the vault",
		`"Password"` + "\n: Required. The password for the account.",
		`"PASSWORD"` + "\n: see the vault",
	} {
		got, found := Mask(input)
		if got != input || len(found) != 0 {
			t.Errorf("documentation value changed: %q -> %q, counts %v", input, got, found)
		}
	}
}

func TestEnglishPasswordPhraseWordLimitIsFour(t *testing.T) {
	for _, header := range []string{
		"password for a b c d e: ",
		"password for the main admin user account: ",
	} {
		if ix := contextEnglishPasswordAtRE.FindStringSubmatchIndex(header); ix != nil {
			t.Errorf("English password grammar accepted more than four words: %q, match %v", header, ix)
		}
		if _, accepted := englishFastHeaderLead(header, strings.Index(header, "password"), strings.Index(header, "password")+len("password")); accepted {
			t.Errorf("English fast header accepted more than four words: %q", header)
		}
	}
}

func TestEnglishPasswordDenseLabelsUseFastGrammar(t *testing.T) {
	var input strings.Builder
	for range 40 {
		input.WriteString("**DB password for the account**: Zq7xW9vK2mPq8Rt\n")
	}
	masked, _ := Mask(input.String())
	if strings.Contains(masked, "Zq7xW9vK2mPq8Rt") || strings.Count(masked, maskMarker+"password") != 40 {
		t.Fatalf("dense Markdown password labels were not all masked: %q", masked)
	}
}

func TestEnglishMarkdownPasswordUsesOrdinaryValueRules(t *testing.T) {
	for _, value := range []string{"hunter2", "changeme"} {
		plain, _ := Mask("Password: " + value)
		wrapped, found := Mask("**Password**: " + value)
		if !strings.Contains(plain, "[скрыто:password]") || wrapped != "**Password**: [скрыто:password]" || found["password"] != 1 {
			t.Errorf("Markdown password changed ordinary value behavior for %q: plain %q, wrapped %q, counts %v", value, plain, wrapped, found)
		}
	}
}

func TestEnglishMarkdownPasswordUsesPasswordValuePredicate(t *testing.T) {
	input := "**required"
	value := contextValue{start: 0, end: len(input)}
	want := englishPasswordValueIsSecret(input, 0, "password", "password", ":", value)
	if got := englishMarkdownValueIsSecret(input, 0, "password", "password", ":", value); got != want {
		t.Errorf("Markdown password value predicate differs: got %v, want %v", got, want)
	}
}

func TestEnglishValueScannerRejectsNextLineValue(t *testing.T) {
	input := "Password:\n  Zq7xW9vK2mPq8Rt"
	at := strings.Index(input, ":") + 1
	if value, ok := scanEnglishSameLineValue(input, at, true); ok {
		t.Errorf("same-line scanner accepted next-line value %q at [%d,%d)", input[value.start:value.end], value.start, value.end)
	}
}

func TestEnglishWrapperPairRequiresMatchingMarker(t *testing.T) {
	if englishWrapperPairIsValid("**Token: ", len("**Token"), "**", false) {
		t.Fatal("unclosed Markdown wrapper was accepted")
	}
	if !englishWrapperPairIsValid("**Token**: ", len("**Token"), "**", false) {
		t.Fatal("matching Markdown wrapper was rejected")
	}
}

func TestMarkdownMatcherRejectsMismatchedWrapper(t *testing.T) {
	input := "**Token: Zq7xW9vK2mPq8Rt"
	start := strings.Index(input, "Token")
	labels := []englishAnchorStart{{start: start, end: start + len("Token"), delimiter: -1}}
	if matches := markdownSecretLabelMatches(input, labels); len(matches) != 0 {
		t.Errorf("Markdown matcher accepted a missing closing wrapper: %v", matches)
	}
}

func TestEnglishPasswordMatcherIgnoresExistingMaskMarker(t *testing.T) {
	input := "[скрыто:password] for the account: Zq7xW9vK2mPq8Rt"
	got, found := Mask(input)
	if got != input || len(found) != 0 {
		t.Errorf("existing mask marker was reinterpreted: %q -> %q, counts %v", input, got, found)
	}
}

func TestEnglishPrefixCacheKeepsOpeningMarkerContext(t *testing.T) {
	secret := "Zq7xW9vK2mPq8Rt"
	for _, input := range []string{
		"DB password for prod: x\n**DB password**: " + secret,
		"db_password\n__db_password for account__: " + secret,
		"  db_Password**: x\n1 __root db_Password__: " + secret,
	} {
		got, found := Mask(input)
		if strings.Contains(got, secret) || found["password"] == 0 {
			t.Errorf("prefix cache lost Markdown opening marker: %q -> %q, counts %v", input, got, found)
		}
	}
}

func TestEnglishPasswordMarkerBeforeDelimiterKeepsValueHidden(t *testing.T) {
	input := `"PASSWORD":** "Zq7xW9vK2mPq8Rt"`
	got, found := Mask(input)
	if strings.Contains(got, "Zq7xW9vK2mPq8Rt") || found["password"] == 0 {
		t.Errorf("closing marker before delimiter left value visible: %q -> %q, counts %v", input, got, found)
	}
}

func TestEnglishPasswordThreeWordMarkdownPrefix(t *testing.T) {
	secret := "Zq7xW9vK2mPq8Rt"
	for _, input := range []string{
		"`Root DB password:` " + secret,
		`"Root DB password:" ` + secret,
	} {
		got, found := Mask(input)
		if strings.Contains(got, secret) || found["password"] == 0 {
			t.Errorf("quoted three-word password label remained visible: %q -> %q, counts %v", input, got, found)
		}
	}
	input := "**Root DB password:** TBD"
	if got, found := Mask(input); got != input || len(found) != 0 {
		t.Errorf("three-word Markdown documentation value changed: %q -> %q, counts %v", input, got, found)
	}
}

func TestEnglishLabelsDoNotSuppressMainNameMatches(t *testing.T) {
	secret := "Zq7xW9vK2mPq8Rt"
	for _, input := range []string{
		`"password":="` + secret + `"`,
		`"PASSWORD"=>"` + secret + `"`,
		`"password": **` + secret,
		`"DB_PASSWORD":="` + secret + `"`,
		`"PASSWD":="` + secret + `"`,
	} {
		got, found := Mask(input)
		if strings.Contains(got, secret) || found["password"] == 0 {
			t.Errorf("English candidate suppressed main name match: %q -> %q, counts %v", input, got, found)
		}
		groups, russianStarts, _ := contextAnchorScan(input)
		mainPath := &masking{found: make(map[string]int)}
		withoutEnglish := maskContext(input, mainPath, groups&^anchorEnCandidate, russianStarts, nil)
		if strings.Contains(withoutEnglish, secret) || mainPath.found["password"] == 0 {
			t.Errorf("main name group did not independently mask %q: %q, groups %v", input, withoutEnglish, groups)
		}
	}
}

func TestEnglishHyphenPasswordLabelVariants(t *testing.T) {
	secret := "Zq7xW9vK2mPq8Rt"
	for _, input := range []string{
		"db_pwd - the account: " + secret,
		`- passphrase - the account: "` + secret + `"`,
	} {
		got, found := Mask(input)
		if strings.Contains(got, secret) || found["password"] == 0 {
			t.Errorf("hyphen password label remained visible: %q -> %q, counts %v", input, got, found)
		}
	}
	for _, input := range []string{
		"- db_password" + strings.Repeat(" ", 41) + ": at least 12 characters",
		`"PASSWORD"` + strings.Repeat(" ", 41) + ": at least 12 characters",
	} {
		nameMasking := &masking{found: make(map[string]int)}
		want := replaceContextMatches(input, nameMatches(input), nameMasking)
		got, _ := Mask(input)
		if got != want {
			t.Errorf("long-space form diverged from ordinary name matching: %q -> %q, name path %q", input, got, want)
		}
	}
}

func TestEnglishMarkdownPasswordValueIsIndependentOfOtherAnchors(t *testing.T) {
	for _, doc := range []string{
		"**Password:** TBD",
		"**Password:** —",
		"**Password:** Updated",
		"**Password:** see the vault",
		"**Password:** N/A",
		"**Password**: The password for the database user.",
		"`password`: the user's password",
		"- `password`: New password for the user.",
		"`password`: Updated",
	} {
		alone, _ := Mask(doc)
		for _, neighbor := range []string{"token: Zq7xW9vK2mPq8Rt", "password:", "DB password: X"} {
			joined, _ := Mask(doc + "\n" + neighbor)
			separate, _ := Mask(neighbor)
			want := alone + "\n" + separate
			if joined != want {
				t.Errorf("neighbor changed markdown label result: doc %q, neighbor %q, alone %q, joined %q, want %q", doc, neighbor, alone, joined, want)
			}
		}
	}
}

func TestEnglishHeaderCachesAreIndependentOfNeighborContext(t *testing.T) {
	for _, pair := range [][2]string{
		{"/etc/password for admin: Zq7xW9vK2mPq8Rt", "a/password for admin: Zq7xW9vK2mPq8Rt"},
		{"x password**: y", "**DB password**: Zq7xW9vK2mPq8Rt"},
		{"see password**: note.", "**DB password**: Zq7xW9vK2mPq8Rt"},
	} {
		for _, rows := range [][]string{{pair[0], pair[1]}, {pair[1], pair[0]}} {
			joined, _ := Mask(strings.Join(rows, "\n"))
			separate := make([]string, len(rows))
			for i, row := range rows {
				separate[i], _ = Mask(row)
			}
			if joined != strings.Join(separate, "\n") {
				t.Errorf("neighbor-dependent cache result for %q: joined %q, separate %q", rows, joined, strings.Join(separate, "\n"))
			}
		}
	}
}

func TestEnglishHeaderBoundaryRejectsEmbeddedLabels(t *testing.T) {
	secret := "Zq7xW9vK2mPq8Rt"
	for _, input := range []string{
		"my**password**: " + secret,
	} {
		got, found := Mask(input)
		if got != input || len(found) != 0 {
			t.Errorf("invalid boundary was accepted: %q -> %q, counts %v", input, got, found)
		}
	}
}

func TestRussianOffsetsAfterTokenReplacement(t *testing.T) {
	token := "sk-" + strings.Repeat("a1B", 12)
	secret := "Zq7xW9vK2mPq8Rt"
	got, found := Mask(token + " пароль: " + secret)
	if len(token) == len("[скрыто:sk]") || found["sk"] != 1 || found["password"] != 1 || strings.Contains(got, secret) {
		t.Fatalf("token rewrite did not leave current Russian offsets: counts %v", found)
	}
}

func TestRussianAnchorByteBases(t *testing.T) {
	for _, label := range []string{"арол", "АРОЛ", "окен", "ОКЕН", "люч", "ЛЮЧ"} {
		if groups := contextAnchorGroups(label); groups&anchorRuName == 0 {
			t.Errorf("Russian byte anchor %q was not found", label)
		}
	}
}

func TestMaskRussianLongWhitespace(t *testing.T) {
	input := "пароль" + strings.Repeat(" ", 4<<20) + ": Qz7mK9xR2nV4bY6"
	got, _ := Mask(input)
	if strings.Contains(got, "Qz7mK9xR2nV4bY6") {
		t.Fatal("secret value after long Russian label whitespace remained visible")
	}
}

func TestMaskRussianRepeatedLabelsScaleLinearly(t *testing.T) {
	elapsed := make([][]time.Duration, 2)
	for i, n := range []int{1000, 4000} {
		input := strings.Repeat("пароль ", n) + ":"
		Mask(input)
		for range 5 {
			start := time.Now()
			for range 4 {
				Mask(input)
			}
			elapsed[i] = append(elapsed[i], time.Since(start)/4)
		}
	}
	for i := range elapsed {
		for j := i + 1; j < len(elapsed[i]); j++ {
			if elapsed[i][j] < elapsed[i][i] {
				elapsed[i][i], elapsed[i][j] = elapsed[i][j], elapsed[i][i]
			}
		}
	}
	if elapsed[1][4] > elapsed[0][4]*8 {
		t.Fatalf("Russian label scan scaled superlinearly: N=1000 %v, N=4000 %v", elapsed[0][4], elapsed[1][4])
	}
}

func TestMaskLinearOnJSONArrayLikeText(t *testing.T) {
	const size = 1 << 20
	for _, unit := range []string{`"[",`, `["password",`, `"value":"a",`, "sudo |"} {
		text := `{"token":` + strings.Repeat(unit, size/len(unit))
		start := time.Now()
		Mask(text)
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("unit %q: %v on 1 MiB", unit, took)
		}
	}
}

// A command word right after ; | or & is an anchor, as it starts a segment for
// the rules; a secret name before a structure leaves the names inside it to
// their own rules.
func TestMaskAfterSeparatorAndInsideStructures(t *testing.T) {
	for _, input := range []string{
		"true;mysql -uroot -pVvalue123 db",
		"a|sshpass -p Vvalue123 ssh h",
		"true&&curl -u admin:Vvalue123 x",
		"ok||mysql -pVvalue123",
		"x;curl --user='user:Vvalue123' x",
		"cat f >/dev/null;mysql -pVvalue123 db",
		"make 2>&1;curl -u admin:Vvalue123 x",
		"auth: {password: Vvalue123}",
		"auth: password: Vvalue123",
		"credentials: {token: Vvalue123}",
		"auth: [password: Vvalue123]",
		"auth: (password: Vvalue123)",
		`auth='{"password":"Vvalue123"}'`,
		`credentials: "{\"password\":\"Vvalue123\"}"`,
		"token: {password: Vvalue123}",
		"basic_auth: {password: Vvalue123, user: u}",
		"auth=::X-API-KEY: Vvalue123",
		`note:\nauth=Vvalue123`,
	} {
		t.Run(input, func(t *testing.T) {
			if got, _ := Mask(input); strings.Contains(got, formValue) {
				t.Fatalf("value left in %q", got)
			}
		})
	}
	// The structure stays; only the inner value goes.
	for input, want := range map[string]string{
		"auth: {password: Vvalue123}":     "auth: {password: [скрыто:password]}",
		"auth: password: Vvalue123":       "auth: password: [скрыто:password]",
		`auth='{"password":"Vvalue123"}'`: `auth='{"password":"[скрыто:password]"}'`,
	} {
		if got, _ := Mask(input); got != want {
			t.Errorf("%q: got %q, want %q", input, got, want)
		}
	}
	for _, input := range []string{"pwd: /home/user/project", "pwd=~/src", "--pwd /tmp/x", "token: abc-note:"} {
		got, found := Mask(input)
		if input == "token: abc-note:" {
			if got == input {
				t.Errorf("a word ending in a colon is still a value: %q", got)
			}
			continue
		}
		if got != input || len(found) != 0 {
			t.Errorf("%q changed to %q, %v", input, got, found)
		}
	}
}

// A structure needs a name inside; a word with a colon and nothing after it is a
// value; only a name exactly pwd takes a path; a plain word or a path under auth
// and credentials is a mode or a file, a token-like value is hidden.
func TestMaskStructureNeedsNameAndPlainAuthValues(t *testing.T) {
	for _, input := range []string{
		"password: {a:Vvalue123}",
		"--password={a=Vvalue123}",
		"--password {a:Vvalue123}",
		"password: Vvalue123:",
		"password: token:Vvalue123",
		"MYSQL_PWD=/Vvalue123",
		"MYSQL_PWD=~Vvalue123",
		"DB_PWD: ~Vvalue123!",
		"pwd=/Vvalue123",
		"auth: Vvalue123",
		"credentials=/x/Vvalue123",
		`{"auth":["Vvalue123"]}`,
		`{"name":"credentials","value":"Vvalue123"}`,
		`{"args":["--auth","Vvalue123"]}`,
		"auth=3f9a7c2e1b4d5a6f",
		"password: token:",
		"MYSQL_PWD=/var/lib",
	} {
		t.Run(input, func(t *testing.T) {
			if got, _ := Mask(input); strings.Contains(got, formValue) || got == input {
				t.Fatalf("value left in %q", got)
			}
		})
	}
	for _, input := range []string{
		`fetch(u, {credentials: "include"})`,
		"auth=oauth2",
		"auth: basic",
		"credentials: /etc/app/creds.json",
		"credentials: aws-profile",
		`{"name":"pwd","value":"/srv"}`,
		`{"args":["--auth","sso"]}`,
		`{"auth":["sso"]}`,
		"pwd=/srv/app",
	} {
		got, found := Mask(input)
		if got != input || len(found) != 0 {
			t.Errorf("%q changed to %q, %v", input, got, found)
		}
	}
}

// Tokens without digits under auth and credentials are hidden; plain words
// stay. A name with a trailing digit is no next name.
func TestMaskAuthTokensWithoutDigits(t *testing.T) {
	for input, secret := range map[string]string{
		"auth: dXNlcjpwYXNz":       "dXNlcjpwYXNz",
		"_auth=dXNlcjpwYXNz":       "dXNlcjpwYXNz",
		`"auth": "aHVudGVyMg"`:     "aHVudGVyMg",
		"auth: deadbeefcafebabe":   "deadbeefcafebabe",
		"auth: ABCDEFGHIJKLMNOP":   "ABCDEFGHIJKLMNOP",
		"auth=abcdefghijklmnopqrs": "abcdefghijklmnopqrs",
		"password: Secret1: x":     "Secret1",
	} {
		if got, _ := Mask(input); strings.Contains(got, secret) {
			t.Errorf("%q: value left in %q", input, got)
		}
	}
	for _, word := range []string{"none", "kerberos", "ldap", "trust", "true", "include", "omit", "same-origin", "aws-profile", "ntlm", "OAuth", "NTLM", "Kerberos"} {
		input := "auth: " + word
		if got, found := Mask(input); got != input || len(found) != 0 {
			t.Errorf("%q changed to %q, %v", input, got, found)
		}
	}
}
