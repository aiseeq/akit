package replycache

import (
	"encoding/gob"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcript"
)

const row = `{"type":"user","origin":{"kind":"human"},"timestamp":"2026-10-25T10:00:00Z","message":{"role":"user","content":"hello"}}`

func TestCacheInvalidation(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "session.jsonl")
	write := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(row)
	c := New()
	check := func(wantMiss int64, count int) {
		t.Helper()
		events, _, ok, _ := c.Events(path, false)
		if !ok || len(events) != count || c.Misses.Load() != wantMiss {
			t.Fatalf("events=%d ok=%v misses=%d want=%d", len(events), ok, c.Misses.Load(), wantMiss)
		}
	}
	check(1, 1)
	check(1, 1)
	// A final line without a newline is reparsed after an append.
	write(row + "\n" + row[:len(row)/2])
	check(2, 1)
	write(row + "\n" + row + "\n")
	check(3, 2)
	write(row)
	check(4, 1)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := path + ".new"
	if err := os.WriteFile(replacement, []byte(strings.Replace(row, "hello", "world", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	check(5, 1)
	if err := os.WriteFile(c.path(path), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	check(6, 1)
	other := New()
	other.dir += "-other-code"
	if other.Valid(path, false) {
		t.Fatal("another code reused entry")
	}
	_, _, ok, _ := other.Events(path, false)
	if !ok || other.Misses.Load() != 1 {
		t.Fatal("other code did not rebuild")
	}
	write(row + "\nnot json\n")
	_, meta, ok, _ := c.Events(path, false)
	if !ok || meta.Bad != 1 || c.Valid(path, true) {
		t.Fatalf("bad lines: %+v", meta)
	}
	_, _, ok, _ = c.Events(path, true)
	if ok {
		t.Fatal("bad lines with prefilter must fall back")
	}
	before := time.Local
	t.Cleanup(func() { time.Local = before })
	time.Local = time.FixedZone("other", 11*3600)
	events, _, ok, _ := c.Events(path, false)
	if !ok || events[0].Time.Location() != time.UTC || events[0].Time.Hour() != 10 {
		t.Fatal("cached time is not UTC")
	}
}

func TestCorruptPayload(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	c := New()
	if _, _, ok, _ := c.Events(path, false); !ok {
		t.Fatal("cold read")
	}
	f, err := os.Open(c.path(path))
	if err != nil {
		t.Fatal(err)
	}
	dec := gob.NewDecoder(f)
	var h header
	var payload []byte
	if err := dec.Decode(&h); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&payload); err != nil {
		t.Fatal(err)
	}
	f.Close()
	payload[len(payload)-1] ^= 1
	f, err = os.Create(c.path(path))
	if err != nil {
		t.Fatal(err)
	}
	enc := gob.NewEncoder(f)
	if err := enc.Encode(h); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(payload); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := c.Events(path, false); !ok || c.Misses.Load() != 2 {
		t.Fatal("corrupt payload was accepted")
	}
}

func TestFixturesMatchReader(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	for _, name := range []string{"claude.jsonl", "codex.jsonl"} {
		path := filepath.Join("..", "testdata", name)
		var expected []transcript.Event
		m, err := transcript.Read(path, nil, func(ev transcript.Event) {
			if ev.Role == transcript.User || ev.Role == transcript.Ask {
				expected = append(expected, transcript.Event{Time: ev.Time.UTC(), Line: ev.Line, Role: ev.Role, Who: ev.Who, Subtype: ev.Subtype, Text: ev.Shown()})
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		c := New()
		for i := range 2 {
			got, meta, ok, _ := c.Events(path, false)
			if !ok || !reflect.DeepEqual(got, expected) || meta.Name != m.Name || meta.Bad != m.Bad {
				t.Fatalf("%s pass %d differs from reader", name, i)
			}
		}
		if c.Misses.Load() != 1 {
			t.Fatal("second read missed")
		}
	}
}

func TestConcurrentProcesses(t *testing.T) {
	if os.Getenv("REPLYCACHE_HELPER") == "1" {
		c := New()
		if _, _, ok, _ := c.Events(os.Getenv("REPLYCACHE_TRANSCRIPT"), false); !ok {
			os.Exit(2)
		}
		return
	}
	cacheDir := t.TempDir()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(row+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	commands := make([]*exec.Cmd, 2)
	for i := range commands {
		commands[i] = exec.Command(os.Args[0], "-test.run=^TestConcurrentProcesses$")
		commands[i].Env = append(os.Environ(), "XDG_CACHE_HOME="+cacheDir, "REPLYCACHE_HELPER=1", "REPLYCACHE_TRANSCRIPT="+path)
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_CACHE_HOME", cacheDir)
	c := New()
	events, _, ok, _ := c.Events(path, false)
	if !ok || len(events) != 1 || c.Misses.Load() != 0 {
		t.Fatal("concurrent writers left an invalid entry")
	}
}
