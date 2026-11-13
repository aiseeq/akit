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

// Russian password and token labels use the same value-shape rules as English labels.
func TestMaskRussianSecretLabels(t *testing.T) {
	for _, input := range []string{
		"пароль: Zq7xW9vK2mPq8Rt",
		"Пароль учётки: Zq7xW9vK2mPq8Rt",
		"пароль=Zq7xW9vK2mPq8Rt",
		"пароля — Zq7xW9vK2mPq8Rt",
		"паролем: Zq7xW9vK2mPq8Rt",
		"токен: Zq7xW9vK2mPq8Rt",
		"токена: Zq7xW9vK2mPq8Rt",
		"ключ: aB4cD9eF0gH2",
	} {
		got, _ := Mask(input)
		secret := "Zq7xW9vK2mPq8Rt"
		if strings.HasPrefix(input, "ключ:") {
			secret = "aB4cD9eF0gH2"
		}
		if strings.Contains(got, secret) {
			t.Errorf("secret value left visible for input %q: %q", input, got)
		}
	}
	for _, input := range []string{
		"пароль не подошёл",
		"пароль: нет",
		"токенов: 1200",
		"ключ: XY-1234",
		"ключ: incident-reference",
		"ключ: Abcdefghijklmnop",
	} {
		if got, found := Mask(input); got != input || len(found) != 0 {
			t.Errorf("ordinary value changed for %q: %q, %v", input, got, found)
		}
	}
}

// Markdown around a secret label stays visible while its value is masked.
func TestMaskMarkdownSecretLabels(t *testing.T) {
	for _, tc := range []struct {
		input  string
		label  string
		secret string
	}{
		{"**Пароль:** Zq7xW9vK2mPq8Rt", "**Пароль:**", "Zq7xW9vK2mPq8Rt"},
		{"__Пароль__: Zq7xW9vK2mPq8Rt", "__Пароль__:", "Zq7xW9vK2mPq8Rt"},
		{"`Пароль`: Zq7xW9vK2mPq8Rt", "`Пароль`:", "Zq7xW9vK2mPq8Rt"},
		{"**Password:** Zq7xW9vK2mPq8Rt", "**Password:**", "Zq7xW9vK2mPq8Rt"},
		{"__Password__: Zq7xW9vK2mPq8Rt", "__Password__:", "Zq7xW9vK2mPq8Rt"},
		{"`Password`: Zq7xW9vK2mPq8Rt", "`Password`:", "Zq7xW9vK2mPq8Rt"},
		{"Password: Zq7xW9vK2mPq8Rt", "Password:", "Zq7xW9vK2mPq8Rt"},
	} {
		got, _ := Mask(tc.input)
		if strings.Contains(got, tc.secret) || !strings.Contains(got, tc.label) {
			t.Errorf("label/value handling for %q: %q", tc.input, got)
		}
	}
	for _, input := range []string{"**Пароль:**, описание", "__Password__:, prose"} {
		if got, found := Mask(input); got != input || len(found) != 0 {
			t.Errorf("closing markup without a value changed %q to %q", input, got)
		}
	}
}

// Secret labels can have up to four words before the value delimiter.
func TestMaskSecretLabelsWithInterveningWords(t *testing.T) {
	for _, input := range []string{
		"пароль для учётной записи: Zq7xW9vK2mPq8Rt",
		"пароль для этой учётной записи: Zq7xW9vK2mPq8Rt",
		"password for the account: Zq7xW9vK2mPq8Rt",
		"password for the current user: Zq7xW9vK2mPq8Rt",
	} {
		got, _ := Mask(input)
		if strings.Contains(got, "Zq7xW9vK2mPq8Rt") {
			t.Errorf("secret value left visible for %q: %q", input, got)
		}
	}
	for _, input := range []string{
		"пароль для учётной записи\nZq7xW9vK2mPq8Rt",
		"пароль для=учётной записи: Zq7xW9vK2mPq8Rt",
		"password for the\naccount: Zq7xW9vK2mPq8Rt",
		"password for = the account: Zq7xW9vK2mPq8Rt",
		"password - the account: Zq7xW9vK2mPq8Rt",
		"password for the current user account: Zq7xW9vK2mPq8Rt",
	} {
		got, _ := Mask(input)
		if !strings.Contains(got, "Zq7xW9vK2mPq8Rt") {
			t.Errorf("label crossed a line or delimiter in %q: %q", input, got)
		}
	}
}

// Russian and English labels classify the same value form identically.
func TestMaskRussianAndEnglishValueShape(t *testing.T) {
	for _, tc := range []struct {
		input string
		value string
		mask  bool
	}{
		{"пароль: Abcdefghijklmno", "Abcdefghijklmno", true},
		{"пароль: \"Abcdefghijklmno\"", "Abcdefghijklmno", true},
		{"password: Abcdefghijklmno", "Abcdefghijklmno", true},
		{"password: \"Abcdefghijklmno\"", "Abcdefghijklmno", true},
		{"пароль: нет", "нет", false},
		{"password: нет", "нет", true},
	} {
		got, _ := Mask(tc.input)
		if strings.Contains(got, tc.value) == tc.mask {
			t.Errorf("value shape for %q: %q", tc.input, got)
		}
	}
}

// A spaced hyphen separates a label from its value; an unspaced hyphen does not.
func TestMaskSecretLabelsWithSpacedHyphen(t *testing.T) {
	for _, input := range []string{
		"пароль - Zq7xW9vK2mPq8Rt",
	} {
		got, _ := Mask(input)
		if strings.Contains(got, "Zq7xW9vK2mPq8Rt") {
			t.Errorf("secret value left visible for %q: %q", input, got)
		}
	}
	for _, input := range []string{
		"пароль-Zq7xW9vK2mPq8Rt",
		"пароль -Zq7xW9vK2mPq8Rt",
		"пароль- Zq7xW9vK2mPq8Rt",
	} {
		got, _ := Mask(input)
		if !strings.Contains(got, "Zq7xW9vK2mPq8Rt") {
			t.Errorf("unspaced hyphen was treated as a delimiter for %q: %q", input, got)
		}
	}
}

func TestMaskSecretValueAfterAnySecretLabel(t *testing.T) {
	for _, input := range []string{
		"ключ и пароль: Abcdefghijklmno",
		"ключ и токен: Abcdefghijklmno",
	} {
		got, _ := Mask(input)
		if strings.Contains(got, "Abcdefghijklmno") {
			t.Errorf("secret value left visible for %q: %q", input, got)
		}
	}
}

func TestMaskRussianLabelWordBoundaries(t *testing.T) {
	for _, label := range []string{
		"пароль", "пароля", "паролю", "паролем", "пароле", "пароли", "паролей", "паролям", "паролями", "паролях",
		"токен", "токена", "токену", "токеном", "токене", "токены", "токенов", "токенам", "токенами", "токенах",
		"ключ", "ключа", "ключу", "ключом", "ключе", "ключи", "ключей", "ключам", "ключами", "ключах",
	} {
		got, _ := Mask(label + ": Abcdefghijk12")
		if strings.Contains(got, "Abcdefghijk12") {
			t.Errorf("secret form %q was not recognized: %q", label, got)
		}
	}
	for _, label := range []string{"токенизация", "паролизация", "ключевой", "ключница"} {
		input := label + ": Abcdefghijk12"
		if got, found := Mask(input); got != input || len(found) != 0 {
			t.Errorf("non-label %q changed to %q, %v", label, got, found)
		}
	}
}

func TestMaskLongWhitespaceDoesNotHideLabel(t *testing.T) {
	input := "password" + strings.Repeat(" ", 512) + ": Qz7mK9xR2nV4bY6"
	got, _ := Mask(input)
	if strings.Contains(got, "Qz7mK9xR2nV4bY6") {
		t.Fatalf("secret after long label spacing remained visible")
	}
}

func TestMaskQuotedSecretLabels(t *testing.T) {
	for _, tc := range []struct{ input, value string }{
		{"«пароль»: Qz7mK9xR2nV4bY6", "Qz7mK9xR2nV4bY6"},
		{`"пароль": Qz7mK9xR2nV4bY6`, "Qz7mK9xR2nV4bY6"},
		{`'пароль': Qz7mK9xR2nV4bY6`, "Qz7mK9xR2nV4bY6"},
		{"`пароль`: Qz7mK9xR2nV4bY6", "Qz7mK9xR2nV4bY6"},
		{`"password": "Qz7mK9xR2nV4bY6"`, "Qz7mK9xR2nV4bY6"},
	} {
		got, _ := Mask(tc.input)
		if strings.Contains(got, tc.value) {
			t.Errorf("quoted secret label left value visible for %q", tc.input)
		}
	}
}

func TestMaskRussianLabelsAreNotInsideWords(t *testing.T) {
	for _, label := range []string{"супертокен", "сверхпароль", "электроключ", "superпароль", "abcтокен"} {
		input := label + ": Qz7mK9xR2nV4bY6"
		if got, found := Mask(input); got != input || len(found) != 0 {
			t.Errorf("embedded label %q changed", label)
		}
	}
}

func TestMaskShortPlainValueBoundary(t *testing.T) {
	for _, tc := range []struct {
		input, value string
		masked       bool
	}{
		{"пароль: abcdefg", "abcdefg", false},
		{"пароль: abcdefgh", "abcdefgh", true},
	} {
		got, _ := Mask(tc.input)
		if strings.Contains(got, tc.value) == tc.masked {
			t.Errorf("short value boundary for %q: %q", tc.input, got)
		}
	}
}

func TestMaskPreservesQuotesAroundValue(t *testing.T) {
	for _, input := range []string{
		`password: "Qz7mK9xR2nV4bY6"`,
		`пароль: "Qz7mK9xR2nV4bY6"`,
	} {
		got, _ := Mask(input)
		if !strings.Contains(got, `"[скрыто:password]"`) {
			t.Errorf("value quote or quote contents were lost for %q: %q", input, got)
		}
	}
}

func TestMaskRussianShortValueAndGuillemets(t *testing.T) {
	for _, input := range []string{
		"пароль: example",
		"пароль: абвгдежзийклмно",
		"пароль: «нет»",
		"auth: same-origin",
	} {
		if got, found := Mask(input); got != input || len(found) != 0 {
			t.Errorf("ordinary short value changed %q to %q, %v", input, got, found)
		}
	}
	for _, input := range []string{
		"пароль: абвгдежзийклмноп",
		"пароль: «Abcdefghijklmno»",
		"password: example",
	} {
		_, found := Mask(input)
		if found["password"] == 0 {
			t.Errorf("secret-shaped value was not masked for %q", input)
		}
	}
}

func TestMaskMarkdownValueMayStartWithEmphasis(t *testing.T) {
	input := `password=**Zq7xW9vK2mPq8Rt**`
	got, _ := Mask(input)
	if strings.Contains(got, "Zq7xW9vK2mPq8Rt") {
		t.Fatalf("secret value left visible: %q", got)
	}
}

func TestMaskNameAnchorDoesNotStartInsideName(t *testing.T) {
	input := `__PWD=Zq7xW9vK2mPq8Rt`
	got, _ := Mask(input)
	if strings.Contains(got, "Zq7xW9vK2mPq8Rt") {
		t.Fatalf("secret value left visible: %q", got)
	}
}

func TestMaskQuotedLabelDashBeforeExplanation(t *testing.T) {
	input := `"pass": - ключ.`
	if got, found := Mask(input); got != input || len(found) != 0 {
		t.Fatalf("explanation separator changed %q to %q, %v", input, got, found)
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
		{"токен: x", anchorName | anchorRussianName},
		{`{"api_token1":"x"}`, anchorName},
		{"--db_password x", anchorName},
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
