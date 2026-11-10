package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aiseeq/akit/secretmask"
)

// The key is created once, private, and read back the same; a short key file is
// an error, not a new key.
func TestMaskKeyCreatedOnceAndKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "akit", "mask-key")
	first, err := loadMaskKey(path)
	if err != nil || len(first) != maskKeyLen {
		t.Fatalf("first load: %d bytes, %v", len(first), err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", info, err)
	}
	again, err := loadMaskKey(path)
	if err != nil || !bytes.Equal(first, again) {
		t.Fatalf("second load differs: %v", err)
	}
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMaskKey(path); err == nil {
		t.Error("short key accepted")
	}
}

// A name the mask takes for a key is hidden in --raw output too; its marker
// gives the length and a tag, so a reader can tell it from the expected name
// without seeing it: the same name gives the same tag, another name another.
func TestRawMaskedNameComparable(t *testing.T) {
	e := fakeHome(t)
	keyPath := filepath.Join(t.TempDir(), "mask-key")
	t.Cleanup(func() { hide = secretmask.Mask })
	if err := useMaskKey(keyPath); err != nil {
		t.Fatal(err)
	}
	name := "pk" + "_test_ConnectionTimeoutCase"
	other := "pk" + "_test_ConnectionRetryingCase"
	var lines strings.Builder
	for i, text := range []string{name, name, other} {
		lines.WriteString(`{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-10-25T10:00:0` +
			strconv.Itoa(i) + `.000Z","message":{"role":"user","content":"тест ` + text + ` упал"}}` + "\n")
	}
	path := filepath.Join(e.home, ".claude", "projects", "-home-x-proj", "9f8e7d6c-0000-4000-8000-000000000004.jsonl")
	if err := os.WriteFile(path, []byte(lines.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	key, err := loadMaskKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	tagger, err := secretmask.NewTagger(key)
	if err != nil {
		t.Fatal(err)
	}
	marker := func(s string) string {
		return "[скрыто:sk_live len=" + strconv.Itoa(len(s)) + " #" + tagger.Tag(s) + "]"
	}
	code, out, _ := runTL(t, e, "9f8e7d6c", "--raw", "упал")
	if code != 0 || strings.Contains(out, "ConnectionTimeoutCase") {
		t.Fatalf("code %d, output:\n%s", code, out)
	}
	if n := strings.Count(out, marker(name)); n != 2 || strings.Count(out, marker(other)) != 1 || marker(name) == marker(other) {
		t.Errorf("want two %s and one %s in:\n%s", marker(name), marker(other), out)
	}
}

// Without a key tl still masks, without length and tag, and says why.
func TestMaskKeyUnavailable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hide = secretmask.Mask })
	err := useMaskKey(filepath.Join(file, "akit", "mask-key"))
	if err == nil || !strings.Contains(err.Error(), "without length and tag") {
		t.Fatalf("error %v", err)
	}
	key := "sk" + "-proj-" + strings.Repeat("x9Y8", 6)
	if got, _ := hide(key); got != "[скрыто:sk]" {
		t.Errorf("plain mask: %q", got)
	}
}

// Several tl starting at once on a fresh directory get one whole key, and no
// temporary file is left next to it.
func TestMaskKeyCreatedOnceUnderRace(t *testing.T) {
	for round := range 50 {
		dir := t.TempDir()
		path := filepath.Join(dir, "akit", "mask-key")
		keys := make([][]byte, 16)
		errs := make([]error, len(keys))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range keys {
			wg.Go(func() {
				<-start
				keys[i], errs[i] = loadMaskKey(path)
			})
		}
		close(start)
		wg.Wait()
		for i := range keys {
			if errs[i] != nil || !bytes.Equal(keys[i], keys[0]) || len(keys[i]) != maskKeyLen {
				t.Fatalf("round %d, caller %d: %d bytes, %v", round, i, len(keys[i]), errs[i])
			}
		}
		if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 1 {
			t.Fatalf("round %d: directory holds %d entries, %v", round, len(entries), err)
		}
	}
}
