package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/internal/dbtest"
	"github.com/aiseeq/akit/transcript"
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
	dbtest.Main(m)
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

func TestShadowCutoffUsesCallTime(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	if got := shadowCutoff(options{}, now); !got.Equal(now) {
		t.Fatalf("default cutoff %v, want call time %v", got, now)
	}
	opts := options{until: now.Add(time.Hour)}
	if got := shadowCutoff(opts, now); !got.Equal(now) {
		t.Fatalf("future --until cutoff %v, want %v", got, now)
	}
	opts.until = now.Add(-time.Second)
	want := opts.until
	if got := shadowCutoff(opts, now); !got.Equal(want) {
		t.Fatalf("earlier --until cutoff %v, want %v", got, want)
	}
}

func TestShadowBoundsUntilAndLoadedLines(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	opts := boundShadowOptions(options{}, now)
	if !opts.until.Equal(now) {
		t.Fatalf("shadow until %v, want call moment %v", opts.until, now)
	}
	opts.until = now.Add(-time.Minute)
	if got := boundShadowOptions(opts, now); !got.until.Equal(now.Add(-time.Minute)) {
		t.Fatalf("explicit earlier until moved: %v", got.until)
	}
	got := filterShadowLines([]string{"/p/a.jsonl:2", "/p/a.jsonl:3"}, map[string]int{"/p/a.jsonl": 2})
	if len(got) != 1 || got[0] != "/p/a.jsonl:2" {
		t.Fatalf("shadow line cap = %v", got)
	}
}

func TestShadowAnswerKeysStopAtCallTime(t *testing.T) {
	cutoff := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	path := "/p/session.jsonl"
	a := &answer{res: &result{hits: []hit{
		{src: source{Path: path}, ev: transcript.Event{Line: 1, Time: cutoff.Add(-time.Second)}},
		{src: source{Path: path}, ev: transcript.Event{Line: 2, Time: cutoff.Add(time.Second)}},
	}}}
	got := answerKeysThrough(a, options{}, cutoff)
	if len(got) != 1 || got[0] != path+":1" {
		t.Fatalf("shadow keys through call time = %v", got)
	}
}

func TestShadowDropsFileKeysPastStoredLines(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	main := paths[0]
	cutoff := time.Now()
	keys := make([]string, 0, len(ch.files[0].events)+1)
	for _, event := range ch.files[0].events {
		keys = append(keys, main+":"+strconv.Itoa(event.ev.Line))
	}
	keys = append(keys, main+":999999")
	spec := shadowSpec{Args: []string{"--role", "all", "--max", "0", main}, Now: cutoff,
		Cutoff: cutoff, Keys: keys}
	rec := shadowRepeat(spec, e.home)
	if rec.Error != "" || rec.Diff != 0 || rec.DBKeys != len(keys)-1 {
		t.Fatalf("shadow repeat = %+v", rec)
	}
}

func TestShadowRepeatsDBThroughFilesWithLineCaps(t *testing.T) {
	e, paths, _ := dbCorpus(t)
	t.Setenv("HOME", e.home)
	main := paths[0]
	args := []string{"--since", "2026-10-01", "--role", "all", main}
	now := time.Now()
	opts, err := parseArgs(args, now)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := resolve(opts, e)
	if err != nil {
		t.Fatal(err)
	}
	res, err := scan(sources, opts, e.home)
	if err != nil {
		t.Fatal(err)
	}
	keys := answerKeys(&answer{res: &res}, opts)
	cutoff := now.Add(time.Minute)
	limits := map[string]int{main: 100000}
	spec := shadowSpec{Args: args, Now: now, Cutoff: cutoff, Keys: keys, Primary: backendDB,
		Limits: limits, PrimaryCode: 0}
	rec := shadowRepeat(spec, e.home)
	if rec.Error != "" || rec.Primary != backendDB || rec.Diff != 0 || rec.FilesKeys != rec.DBKeys || rec.DBS != round3(spec.PrimaryS) || rec.DBCode != spec.PrimaryCode {
		t.Fatalf("database primary repeat: %+v", rec)
	}
	firstLine := 0
	for _, key := range keys {
		_, raw, ok := strings.Cut(key, ":")
		line, parseErr := strconv.Atoi(raw)
		if ok && parseErr == nil && (firstLine == 0 || line < firstLine) {
			firstLine = line
		}
	}
	lateLimits := map[string]int{main: firstLine}
	spec.Keys = filterShadowLines(keys, lateLimits)
	spec.Limits = lateLimits
	rec = shadowRepeat(spec, e.home)
	if rec.Error != "" || rec.Diff != 0 {
		t.Fatalf("event loaded after database answer: %+v", rec)
	}
	// The primary database missed one event inside its captured line boundary.
	spec.Keys = append([]string(nil), keys[1:]...)
	spec.Limits = map[string]int{main: 100000}
	rec = shadowRepeat(spec, e.home)
	if rec.Error != "" || rec.Diff != 1 || len(rec.OnlyFiles) != 1 {
		t.Fatalf("line capped repeat: %+v", rec)
	}
}

func TestShadowRepeatTimeoutRecord(t *testing.T) {
	old := shadowRepeatTimeout
	shadowRepeatTimeout = time.Millisecond
	t.Cleanup(func() { shadowRepeatTimeout = old })
	spec := shadowSpec{Args: []string{"--all"}, Primary: backendDB, Started: time.Now().Add(-time.Second)}
	rec := shadowRepeat(spec, t.TempDir())
	if rec.Diff != -1 || rec.Error != "files repeat timed out after 30m" {
		t.Fatalf("timeout record: %+v", rec)
	}
}

func TestFirstShadowFormIsSelectedAtTinyRate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("TL_SHADOW_RATE", "0.0001")
	opts := options{originalArgs: []string{"--role", "user", "session-a"}}
	old := shadowRandomFloat
	shadowRandomFloat = func() float64 { return 0.5 }
	t.Cleanup(func() { shadowRandomFloat = old })
	if pick, err := pickShadow(opts, backendDB); err != nil || !pick {
		t.Fatalf("first form pick = %v, %v", pick, err)
	}
	if pick, err := pickShadow(opts, backendDB); err != nil || pick {
		t.Fatalf("second form pick = %v, %v", pick, err)
	}
}

func TestDBUnselectedDoesNotCollectOrQueryShadowFiles(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("HOME", e.home)
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	t.Setenv("TL_BACKEND", backendDB)
	t.Setenv("TL_SHADOW_RATE", "0")
	args := []string{"--all", "--max", "0"}
	opts, err := parseArgs(args, e.now)
	if err != nil {
		t.Fatal(err)
	}
	e.backend = backendDB
	prepareShadow(opts, &e, io.Discard)
	if e.collect != nil || e.shadowPicked {
		t.Fatalf("unselected call has shadow collector: collect=%v picked=%v", e.collect, e.shadowPicked)
	}
	if _, ok, err := runOutputDB(opts, e, nil, io.Discard, io.Discard); err != nil || !ok {
		t.Fatal("database did not answer synthetic call")
	}
	if got := countOf(ch.requests(), "shadow_files"); got != 0 {
		t.Fatalf("unselected call queried shadow_files %d times; requests=%v", got, ch.requests())
	}
}

func TestFallbackJournalFromStartAtZeroRate(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	ch := newFakeCH(t, paths, unloaded)
	ch.fail = map[string]bool{"events": true}
	srv := httptest.NewServer(ch)
	defer srv.Close()
	journal := filepath.Join(t.TempDir(), "journal.jsonl")
	t.Setenv("HOME", e.home)
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	t.Setenv("TL_BACKEND", backendDB)
	t.Setenv("TL_SHADOW_RATE", "0")
	t.Setenv("TL_SHADOW_JOURNAL", journal)
	var stderr bytes.Buffer
	start([]string{"--all", "--max", "0"}, e.now, io.Discard, &stderr)
	data, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	var rec shadowRecord
	if err := json.Unmarshal(bytes.TrimSpace(data), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Primary != backendDB || rec.Fallback == "" || rec.Diff != -1 || rec.FilesCode == exitUsage {
		t.Fatalf("fallback record: %+v", rec)
	}
}

func TestNoLoaderDoesNotWriteFallbackJournal(t *testing.T) {
	e, _, _ := dbCorpus(t)
	state := filepath.Join(os.Getenv("XDG_STATE_HOME"), "akit", "tlfeed", "state.json")
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(t.TempDir(), "journal.jsonl")
	t.Setenv("HOME", e.home)
	t.Setenv("TL_BACKEND", "")
	t.Setenv("TL_CLICKHOUSE_URL", "http://127.0.0.1:1")
	t.Setenv("TL_SHADOW_RATE", "0")
	t.Setenv("TL_SHADOW_JOURNAL", journal)
	start([]string{"--all"}, e.now, io.Discard, io.Discard)
	if countLines(t, journal) != 0 {
		t.Fatal("NoLoader call wrote a fallback journal row")
	}
}

func TestShadowRepeatUsesCallTimeForFileKeys(t *testing.T) {
	previousSpawner := shadowSpawner
	shadowSpawner = func(specPath, _, _ string) error {
		go func() { _ = runShadowChild(specPath) }()
		return nil
	}
	t.Cleanup(func() { shadowSpawner = previousSpawner })
	e, paths, unloaded := dbCorpus(t)
	cutoff := e.now
	file, err := os.OpenFile(paths[0], os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	marker := "synthetic cutoff marker"
	_, writeErr := fmt.Fprintln(file, userRecord(cutoff.Add(-time.Second).Format(time.RFC3339Nano), marker))
	if writeErr == nil {
		_, writeErr = fmt.Fprintln(file, userRecord(cutoff.Add(time.Second).Format(time.RFC3339Nano), marker))
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	e.now = cutoff
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	t.Setenv("HOME", e.home)
	t.Setenv("TL_BACKEND", backendFiles)
	journal := filepath.Join(t.TempDir(), "shadow.jsonl")
	t.Setenv("TL_SHADOW_JOURNAL", journal)
	statePath := filepath.Join(os.Getenv("XDG_STATE_HOME"), "akit", "tlfeed", "state.json")
	stateData, _ := json.Marshal(dbState{Heartbeat: time.Now().Format(time.RFC3339Nano), DBThrough: time.Now(), Home: e.home})
	if err := os.WriteFile(statePath, stateData, 0o600); err != nil {
		t.Fatal(err)
	}

	args := []string{paths[0], "--grep", marker, "--role", "all"}
	opts, err := parseArgs(args, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	e.now = cutoff
	var answerHits []hit
	for _, event := range ch.files[0].events {
		if strings.Contains(event.ev.Shown(), marker) {
			answerHits = append(answerHits, hit{src: source{Path: paths[0]}, ev: event.ev})
		}
	}
	if len(answerHits) != 2 {
		t.Fatalf("synthetic marker events = %d, want two", len(answerHits))
	}
	a := &answer{filled: true, res: &result{hits: answerHits}}
	shadowAfter(a, opts, e, 0, time.Second, io.Discard)
	waitFor(t, "shadow cutoff journal", func() bool { return countLines(t, journal) == 1 })
	data, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	var rec shadowRecord
	if err := json.Unmarshal(bytes.TrimSpace(data), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Error != "" || rec.Diff != 0 || rec.FilesKeys != rec.DBKeys {
		t.Fatalf("shadow cutoff record = %+v", rec)
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
	t.Setenv("TL_BACKEND", backendFiles)
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

// The shadow repeats through the database only what files answered: a call answered by the database (the default)
// is not compared with itself.
func TestPickShadowOnlyForFiles(t *testing.T) {
	t.Setenv("TL_SHADOW_RATE", "1")
	opts, err := parseArgs([]string{"--all", "--grep", "x"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if pick, err := pickShadow(opts, backendFiles); err != nil || !pick {
		t.Fatalf("files at rate 1: pick=%t err=%v", pick, err)
	}
	if pick, err := pickShadow(opts, backendDB); err != nil || !pick {
		t.Fatalf("database: pick=%t err=%v", pick, err)
	}
}
