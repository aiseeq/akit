package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A key pasted into a transcript never comes out of tl: not in a conversation, a whole event, a grep or
// raw jsonl lines. The fake key is built at run time and failures never print the output that held it.
func TestOutputMasksSecrets(t *testing.T) {
	e := fakeHome(t)
	key := "sk" + "-proj-" + strings.Repeat("x9Y8", 6)
	line := `{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-10-25T10:00:00.000Z","message":{"role":"user","content":"ключ ` + key + ` для шлюза"}}` + "\n"
	path := filepath.Join(e.home, ".claude", "projects", "-home-x-proj", "9f8e7d6c-0000-4000-8000-000000000002.jsonl")
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"9f8e7d6c"},
		{"9f8e7d6c:1"},
		{"--all", "--role", "user", "--grep", "ключ"},
		{"9f8e7d6c", "--raw", "шлюза"},
	} {
		code, out, errOut := runTL(t, e, args...)
		if strings.Contains(out, key) || strings.Contains(errOut, key) {
			t.Errorf("tl %v: the key is in the output", args)
			continue
		}
		if code != 0 || !strings.Contains(out, "[скрыто:sk]") {
			t.Errorf("tl %v: code %d, no mask marker in %d bytes of output", args, code, len(out))
		}
	}
}
