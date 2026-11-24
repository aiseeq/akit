package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// shadowPidsEnv names a file the repeat started from a test adds its pid to: what the call started is seen.
const shadowPidsEnv = "TL_TEST_SHADOW_PIDS"

// TestMain is also the repeat of the shadow: startShadow runs this test binary with TL_SHADOW_CHILD set.
func TestMain(m *testing.M) {
	if spec := os.Getenv(shadowChildEnv); spec != "" {
		if path := os.Getenv(shadowPidsEnv); path != "" {
			if f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); err == nil {
				_, _ = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
				_ = f.Close()
			}
		}
		if err := runShadowChild(spec); err != nil {
			os.Exit(exitUsage)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("no %s in 30 s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func countOf(list []string, name string) int {
	n := 0
	for _, s := range list {
		if s == name {
			n++
		}
	}
	return n
}

func TestShadowCutoffUsesDBThrough(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	through := now.Add(-7 * time.Second)
	if got := shadowDBCutoff(options{}, now, through); !got.Equal(through) {
		t.Fatalf("default cutoff %v, want %v", got, through)
	}
	opts := options{until: through.Add(time.Hour)}
	if got := shadowDBCutoff(opts, now, through); !got.Equal(through) {
		t.Fatalf("future --until cutoff %v, want %v", got, through)
	}
	opts.until = through.Add(-time.Second)
	want := opts.until
	if got := shadowDBCutoff(opts, now, through); !got.Equal(want) {
		t.Fatalf("earlier --until cutoff %v, want %v", got, want)
	}
}

// The shadow repeats a call answered from files through the database after the answer: the call does not wait
// for it, the repeat writes one journal line with no differences, and no more repeats of one form run at once
// than shadowPerForm — the call does not even start another.
func TestShadowRepeatsThroughDB(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	ch := newFakeCH(t, paths, unloaded)
	gate := make(chan struct{})
	ch.gate = gate
	srv := httptest.NewServer(ch)
	defer srv.Close()
	opened := false
	open := func() {
		if !opened {
			opened = true
			close(gate)
		}
	}
	defer open()
	dir := t.TempDir()
	journal, pids := filepath.Join(dir, "shadow.jsonl"), filepath.Join(dir, "pids")
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	t.Setenv("HOME", e.home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	statePath := filepath.Join(os.Getenv("XDG_STATE_HOME"), "akit", "tlfeed", "state.json")
	stateData, _ := json.Marshal(dbState{Heartbeat: time.Now().Format(time.RFC3339Nano), DBThrough: time.Now(), Home: e.home})
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, stateData, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TL_SHADOW_JOURNAL", journal)
	t.Setenv("TL_SHADOW_RATE", "1")
	t.Setenv("TL_BACKEND", "")
	t.Setenv(shadowPidsEnv, pids)
	args := []string{"--all", "--role", "all", "--grep", "sub", "--max", "0"}
	call := func() string {
		t.Helper()
		done := make(chan string, 1)
		go func() {
			var out, errOut bytes.Buffer
			code := start(args, e.now, &out, &errOut)
			done <- strings.Join([]string{out.String(), errOut.String(), strconv.Itoa(code)}, "\x00")
		}()
		select {
		case got := <-done:
			return got
		case <-time.After(20 * time.Second):
			t.Fatal("the call waits for its repeat through the database")
			return ""
		}
	}
	first := call()
	if !strings.Contains(first, "sub") || strings.Contains(first, "shadow:") {
		t.Fatalf("answer %q", first)
	}
	// the repeat waits at the gate; the call has returned
	waitFor(t, "repeat", func() bool { return countOf(ch.requests(), "files") == 1 })
	if n := countLines(t, journal); n != 0 {
		t.Fatalf("%d journal lines before the repeat got its answer", n)
	}
	call()
	waitFor(t, "second repeat", func() bool { return countOf(ch.requests(), "files") == 2 })
	before := shadowStarts.Load()
	call()
	call()
	if n := shadowStarts.Load() - before; n != 0 {
		t.Errorf("%d repeats started with %d of the form running", n, shadowPerForm)
	}
	open()
	waitFor(t, "journal lines", func() bool { return countLines(t, journal) >= 2 })
	if n := countLines(t, pids); n != 2 {
		t.Errorf("%d repeats ran, want 2", n)
	}
	data, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d journal lines, want 2:\n%s", len(lines), data)
	}
	for _, l := range lines {
		var rec shadowRecord
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		if rec.Diff != 0 || rec.Error != "" || rec.FilesKeys == 0 || rec.DBKeys != rec.FilesKeys || !slices.Equal(rec.Args, args) {
			t.Errorf("record %s", l)
		}
		if !strings.Contains(l, `"diff":0`) {
			t.Errorf("no diff field: %s", l)
		}
	}
}

// Keys appended after the call began are not a difference; others are, counted with their repeats.
func TestShadowKeyDiff(t *testing.T) {
	n, onlyA, onlyB := keyDiff([]string{"/p/a.jsonl:1", "/p/a.jsonl:2", "/p/a.jsonl:2"}, []string{"/p/a.jsonl:2", "/p/a.jsonl:3"})
	if n != 3 || len(onlyA) != 2 || len(onlyB) != 1 || onlyB[0] != "a:3" {
		t.Errorf("%d %q %q", n, onlyA, onlyB)
	}
	opts := options{}
	at := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	res := &result{hits: []hit{{src: source{Path: "/p/a.jsonl"}}, {src: source{Path: "/p/a.jsonl"}}}}
	res.hits[0].ev.Time, res.hits[0].ev.Line = at, 1
	res.hits[1].ev.Time, res.hits[1].ev.Line = at.Add(time.Minute), 2
	if keys := answerKeys(&answer{res: res}, opts); !slices.Equal(keys, []string{"/p/a.jsonl:1", "/p/a.jsonl:2"}) {
		t.Errorf("keys %q", keys)
	}
}

func TestShadowKeysUseStoredLineLimits(t *testing.T) {
	got := filterShadowLines([]string{"/p/a.jsonl:1", "/p/a.jsonl:2", "/p/a.jsonl:3", "/p/gone.jsonl:1"},
		map[string]int{"/p/a.jsonl": 2})
	want := []string{"/p/a.jsonl:1", "/p/a.jsonl:2"}
	if !slices.Equal(got, want) {
		t.Fatalf("line-limited shadow keys %q, want %q", got, want)
	}
}

// A slot holdSlot took stays held for the life of the process, through garbage collection: the lock is the open
// file's, and a file nothing refers to is closed by the collector.
func TestHoldSlotSurvivesGC(t *testing.T) {
	dir := t.TempDir()
	held, err := holdSlot(dir, "form", 1)
	if err != nil || !held {
		t.Fatalf("holdSlot %v %v", held, err)
	}
	t.Cleanup(func() {
		heldMu.Lock()
		defer heldMu.Unlock()
		for _, release := range heldSlots {
			release()
		}
		heldSlots = nil
	})
	// the collector closes a dropped file from its finalizer goroutine: give it a few turns
	for range 20 {
		runtime.GC()
		time.Sleep(5 * time.Millisecond)
		if free, err := slotFree(dir, "form", 1); err != nil || free {
			t.Fatalf("after GC the held slot is free %v (%v)", free, err)
		}
	}
	if held, err := holdSlot(dir, "form", 1); err != nil || held {
		t.Errorf("a second hold of the only slot %v %v", held, err)
	}
}
