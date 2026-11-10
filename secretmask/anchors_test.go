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

func TestMaskAuthOrdinaryWordFormsStayVisible(t *testing.T) {
	for _, input := range []string{
		"auth: CedarMaple",
		"credentials: nodeProfile",
		"auth: HTTPClient",
		"credentials: /srv/tools/myProject/creds.json",
		"pwd: /home/u/myProject",
		"auth: internationalization",
		"auth: PostgreSQL",
		"auth: nodeSync",
		"auth: AWSProfile",
		"credentials: /Users/me/MyApp/creds.json",
		"auth: 12345",
		`{"auth":"CedarMaple"}`,
		`{"argv":["--auth","CedarMaple"]}`,
	} {
		got, found := Mask(input)
		if got != input || len(found) != 0 {
			t.Errorf("%q changed to %q, %v", input, got, found)
		}
	}
}

func TestMaskAuthTokenLikeValuesStayHidden(t *testing.T) {
	for _, input := range []string{
		"auth: aB3xY9q",
		"credentials: Zq7xW9v",
		"auth: dXNlcmFkbWlu",
		`{"auth":"dXNlcmFkbWlu"}`,
		"auth: qwhxkzjvplmbnrty",
		"auth: a1b2c3d4e5f6",
		"auth: kQzXwPvRtYmN",
		"auth: abcDef123456",
		`{"auth":"ghp_syntheticTokenValue123456"}`,
		`{"argv":["--auth","abcdef0123456789"]}`,
		"auth: /srv/dXNlcmFkbWlu",
		"auth: YWJjZGVm",
		"auth: xurSyjqjIbQe",
		"auth: kitVTTDAMPCSdib",
		"auth: GaehCzdzmeCN",
		"auth: rekXFovaPur",
	} {
		got, _ := Mask(input)
		if !strings.Contains(got, maskMarker+"token") {
			t.Errorf("token-like value remained in %q", got)
		}
	}
}
