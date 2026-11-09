package secretmask

import (
	"strings"
	"testing"
)

const formValue = "Vvalue123"

// Forms where a password or a token stands in a place the masker has to know.
func TestMaskMoreFormsHideTheValue(t *testing.T) {
	for _, input := range []string{
		"redis-cli -a Vvalue123",
		"redis-cli -h host -a Vvalue123 ping",
		"mongo -u a -p Vvalue123 db",
		"docker login -p Vvalue123 reg",
		"htpasswd -b file user Vvalue123",
		"echo Vvalue123 | sudo -S ls",
		"curl --oauth2-bearer Vvalue123 x",
		"curl -H 'Cookie: sid=Vvalue123' x",
		"curl -b sid=Vvalue123 x",
		"git clone https://Vvalue123@github.com/x",
		"pwd=Vvalue123",
		"auth=Vvalue123",
		"credentials=Vvalue123",
		"gh auth login --with-token <<< Vvalue123",
		"mysql --password=$'Vvalue123'",
		`{"content":"api_token: abcdefghij\npassword: Vvalue123\n"}`,
		`{"content":"deploy --password\tVvalue123"}`,
		`{"content":"sshpass -V\nsshpass -p Vvalue123 ssh host"}`,
		`{"content":"curl -u admin:\"Vvalue123 x\" https://y"}`,
		"сделай\n--password Vvalue123\nконец",
		"claude --model 'x\nmysql -pVvalue123' -p hi",
		`{"args":["--password","Vvalue123"]}`,
		`{"argv":["curl","-u","admin:Vvalue123","https://x"]}`,
		`{"password":["Vvalue123"]}`,
		`{"token":{"value":"Vvalue123"}}`,
		`{"name":"password","value":"Vvalue123"}`,
	} {
		t.Run(input, func(t *testing.T) {
			got, _ := Mask(input)
			if strings.Contains(got, formValue) {
				t.Fatalf("value left in %q", got)
			}
		})
	}
}

// The same tools and words in their ordinary use stay as they are.
func TestMaskMoreFormsKeepOrdinaryCommands(t *testing.T) {
	for _, input := range []string{
		"redis-cli ping",
		"redis-cli -h host -p 6379 get key",
		"docker login reg",
		"docker run -p 8080:80 image",
		"docker ps -a",
		"htpasswd -c file user",
		"echo hello | sudo ls",
		"sudo -S ls",
		"curl -H 'Accept: text/plain' x",
		"curl -b cookies.txt x",
		"git clone https://github.com/x/y",
		"git clone git@github.com:x/y.git",
		"pwd",
		"cd $(pwd)",
		"auth_required",
		"mongo --port 27017 db",
		"ls -a",
		"grep -a pattern file",
		`{"name":"build","value":"ok"}`,
		`{"args":["--verbose","file"]}`,
		`{"token_count":12}`,
	} {
		t.Run(input, func(t *testing.T) {
			got, found := Mask(input)
			if got != input || len(found) != 0 {
				t.Fatalf("changed to %q, %v", got, found)
			}
		})
	}
}
