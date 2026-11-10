package secretmask

import (
	"strings"
	"testing"
)

// The JSON forms are general: the secret is found by its name or flag at any
// position, not only as the first element.
func TestMaskMoreFormsGeneralJSON(t *testing.T) {
	for _, input := range []string{
		`{"args":["-v","--password","Vvalue123"]}`,
		`{"args":["--token","Vvalue123","-v"]}`,
		`{"argv":["curl","-s","-u","admin:Vvalue123","https://x"]}`,
		`{"api_token":["Vvalue123"]}`,
		`{"db_password":{"value":"Vvalue123"}}`,
		`{"name":"api_token","value":"Vvalue123"}`,
	} {
		t.Run(input, func(t *testing.T) {
			got, _ := Mask(input)
			if strings.Contains(got, formValue) {
				t.Fatalf("value left in %q", got)
			}
		})
	}
}

// A plain user name in a URL and an echo outside the sudo -S pipeline stay.
func TestMaskMoreFormsKeepNeighbours(t *testing.T) {
	for _, input := range []string{
		"git clone https://git@github.com/x/y",
		"open https://user@example.com/path",
		"echo hello world\nls | sudo -S whoami",
		"echo done; cat f | sudo -S tee x",
		"echo done && cat f | sudo -S tee x",
	} {
		t.Run(input, func(t *testing.T) {
			got, found := Mask(input)
			if got != input || len(found) != 0 {
				t.Fatalf("changed to %q, %v", got, found)
			}
		})
	}
}
