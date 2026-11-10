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
