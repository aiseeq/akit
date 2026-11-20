package replycache

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
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

func TestCacheKeyIncludesCtimeForSameSizeAndMtime(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	c := New()
	if _, _, ok, err := c.Events(path, false); err != nil || !ok {
		t.Fatalf("cold read: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(row, "hello", "world", 1)
	if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	events, _, ok, err := c.Events(path, false)
	if err != nil || !ok || len(events) != 1 || events[0].Text != "world" || c.Misses.Load() != 2 {
		t.Fatalf("same-size update was reused: events=%+v ok=%v misses=%d err=%v", events, ok, c.Misses.Load(), err)
	}
}

func TestCacheKeyIncludesInodeIndependentlyOfCtime(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	c := New()
	if _, _, ok, err := c.Events(path, false); err != nil || !ok {
		t.Fatalf("cold read: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := path + ".replacement"
	if err := os.WriteFile(replacement, []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	current, err := fileKey(path)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := os.Open(c.path(path))
	if err != nil {
		t.Fatal(err)
	}
	var h header
	var payload []byte
	decoder := gob.NewDecoder(entry)
	if err := decoder.Decode(&h); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&payload); err != nil {
		t.Fatal(err)
	}
	_ = entry.Close()
	h.Key.Ctime = current.Ctime
	entry, err = os.Create(c.path(path))
	if err != nil {
		t.Fatal(err)
	}
	encoder := gob.NewEncoder(entry)
	if err := encoder.Encode(h); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(payload); err != nil {
		t.Fatal(err)
	}
	if err := entry.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(c.path(path), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := c.Events(path, false); err != nil || !ok || c.Misses.Load() != 2 {
		t.Fatalf("replacement inode reused with matching ctime: ok=%v misses=%d err=%v", ok, c.Misses.Load(), err)
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
	payload = bytes.Replace(payload, []byte("hello"), []byte("jello"), 1)
	h.Length = int64(len(payload))
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

func TestCachedEventsAreQueryIndependent(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	c := New()
	snapshot := c.Snapshot(path)
	stamp, _ := time.Parse(time.RFC3339, "2026-10-25T10:00:00Z")
	events := []transcript.Event{{Time: stamp, Role: transcript.User, Text: "hello", Raw: []byte(row)}}
	bucket := stamp.Unix() / RecordBucketSeconds * RecordBucketSeconds
	from, until := stamp.Add(-time.Hour), stamp.Add(time.Hour)
	if reason := c.StoreEvents(path, snapshot, events, []int64{bucket}, []string{"2026-10-25"}, false, from, until, transcript.Meta{}); reason != "" {
		t.Fatal(reason)
	}
	for range 2 { // Запрос и зона не входят в ключ общего файла.
		cached, ok, err := c.CachedEvents(path)
		if err != nil || !ok || !cached.Covers(stamp, stamp.Add(time.Minute)) || len(cached.Events) != 1 || cached.Events[0].Text != "hello" || string(cached.Events[0].Raw) != row || len(cached.RawDays) != 1 || cached.RawDays[0] != "2026-10-25" || len(cached.RecordBuckets) != 1 || cached.RecordBuckets[0] != bucket || len(cached.BadDays) != 1 || cached.BadDays[0] != "2026-10-25" {
			t.Fatalf("cached transcript: %+v, ok=%v", cached, ok)
		}
	}
	second := stamp.Add(2 * time.Hour)
	if reason := c.StoreEvents(path, snapshot, []transcript.Event{{Time: second, Role: transcript.User, Text: "next"}}, nil, []string{"2026-10-26"}, false, second.Add(-time.Hour), second.Add(time.Hour), transcript.Meta{}); reason != "" {
		t.Fatal(reason)
	}
	cached, ok, err := c.CachedEvents(path)
	if err != nil || !ok || len(cached.Events) != 2 || !cached.Covers(from, second.Add(time.Hour)) || !reflect.DeepEqual(cached.BadDays, []string{"2026-10-25", "2026-10-26"}) {
		t.Fatalf("merged coverage and events: %+v, ok=%v, err=%v", cached, ok, err)
	}
}

func TestSnapshotFileUsesOpenedFileAndRejectsPathReplacement(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	c := New()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := c.SnapshotFile(path, info)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	stamp, _ := time.Parse(time.RFC3339, "2026-10-25T10:00:00Z")
	if reason := c.StoreEvents(path, snapshot, []transcript.Event{{Time: stamp, Role: transcript.User, Text: "synthetic"}}, nil, nil, false, stamp, stamp, transcript.Meta{}); reason != "" {
		t.Fatalf("store from opened-file snapshot: %s", reason)
	}

	f, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err = f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	replacedSnapshot := c.SnapshotFile(path, info)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	replacement := path + ".new"
	if err := os.WriteFile(replacement, []byte(strings.Replace(row, "hello", "changed", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if reason := c.StoreEvents(path, replacedSnapshot, nil, nil, nil, false, stamp, stamp, transcript.Meta{}); reason == "" {
		t.Fatal("published events after the source path was replaced")
	}
}

func TestHasEntriesTracksTranscriptRecords(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	c := New()
	if !c.Available() || c.HasEntries() {
		t.Fatal("new cache should be available and empty")
	}
	stamp := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	if reason := c.StoreEvents(path, c.Snapshot(path), []transcript.Event{{Time: stamp, Role: transcript.User, Text: "synthetic"}}, nil, nil, false, stamp, stamp, transcript.Meta{}); reason != "" {
		t.Fatal(reason)
	}
	if !c.HasEntries() {
		t.Fatal("stored transcript record was not detected")
	}
}

func TestStoreEventsCanSkipSecondLookupAfterColdMiss(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	c := NewForCode("producer")
	snapshot := c.Snapshot(path)
	first := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	second := first.Add(24 * time.Hour)
	if reason := c.StoreEvents(path, snapshot, []transcript.Event{{Time: first, Role: transcript.User, Text: "first"}}, nil, nil, false, first, first, transcript.Meta{}); reason != "" {
		t.Fatal(reason)
	}
	if reason := c.StoreEventsWithBadLinesFromLookup(path, snapshot, []transcript.Event{{Time: second, Role: transcript.User, Text: "second"}}, nil, nil, false, nil, nil, second, second, transcript.Meta{}, false); reason != "" {
		t.Fatal(reason)
	}
	cached, ok, err := c.CachedEvents(path)
	if err != nil || !ok || len(cached.Events) != 1 || cached.Events[0].Text != "second" || !cached.Covers(second, second) || cached.Covers(first, first) {
		t.Fatalf("cold-miss replacement: events=%+v coverage=%+v ok=%v err=%v", cached.Events, cached.Coverage, ok, err)
	}
}

func TestCachedMalformedLineDetailsRoundTrip(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "synthetic.jsonl")
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	c := NewForCode("synthetic-producer")
	snapshot := c.Snapshot(path)
	stamp := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	bad := []BadLine{{Time: stamp, Day: "2026-10-25", Line: 8, Message: "invalid synthetic row"}}
	if reason := c.StoreEventsWithBadLines(path, snapshot, nil, nil, []string{"2026-10-25"}, false, bad, []string{"2026-10-25"}, stamp.Add(-time.Hour), stamp.Add(time.Hour), transcript.Meta{}); reason != "" {
		t.Fatal(reason)
	}
	cached, ok, err := c.CachedEvents(path)
	if err != nil || !ok || !reflect.DeepEqual(cached.BadLines, bad) || !reflect.DeepEqual(cached.BadCoverageDays, []string{"2026-10-25"}) {
		t.Fatalf("bad-line cache round trip: lines=%#v coverage=%v ok=%v err=%v", cached.BadLines, cached.BadCoverageDays, ok, err)
	}
}

func TestCachedEventsRejectsReversedCoverageWithValidChecksum(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	c := NewForCode("producer")
	snapshot := c.Snapshot(path)
	stamp := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	if reason := c.StoreEvents(path, snapshot, []transcript.Event{{Time: stamp, Role: transcript.User, Text: "hello"}}, nil, nil, false, stamp.Add(-time.Hour), stamp.Add(time.Hour), transcript.Meta{}); reason != "" {
		t.Fatal(reason)
	}
	entry := c.path(path)
	f, err := os.Open(entry)
	if err != nil {
		t.Fatal(err)
	}
	var h header
	var payload []byte
	dec := gob.NewDecoder(f)
	if err := dec.Decode(&h); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	var rec record
	if err := gob.NewDecoder(bytes.NewReader(payload)).Decode(&rec); err != nil {
		t.Fatal(err)
	}
	rec.Coverage = []coverageRange{{From: stamp.UnixNano(), Until: stamp.Add(-time.Hour).UnixNano()}}
	var rewritten bytes.Buffer
	if err := gob.NewEncoder(&rewritten).Encode(rec); err != nil {
		t.Fatal(err)
	}
	h.Length = int64(rewritten.Len())
	h.Sum = sha256.Sum256(rewritten.Bytes())
	f, err = os.Create(entry)
	if err != nil {
		t.Fatal(err)
	}
	enc := gob.NewEncoder(f)
	if err := enc.Encode(h); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(rewritten.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.CachedEvents(path); err == nil || ok {
		t.Fatal("reversed coverage with a valid checksum was accepted")
	}
}

func TestPartialBriefRecordIsNotCompleteForTL(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "session.jsonl")
	contents := row + "\n" + strings.Replace(row, "hello", "second", 1) + "\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	c := NewForCode("producer")
	snapshot := c.Snapshot(path)
	stamp := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	if reason := c.StoreEvents(path, snapshot, []transcript.Event{{Time: stamp, Role: transcript.User, Text: "hello"}}, nil, nil, false, stamp.Add(-time.Minute), stamp.Add(time.Minute), transcript.Meta{}); reason != "" {
		t.Fatal(reason)
	}
	if c.Valid(path, false) {
		t.Fatal("partial brief event record was accepted as a complete TL cache")
	}
	events, _, ok, err := c.Events(path, false)
	if err != nil || !ok || len(events) != 2 {
		t.Fatalf("TL did not reload the full transcript: events=%+v ok=%v err=%v", events, ok, err)
	}
	if c.Misses.Load() != 1 || !c.Valid(path, false) {
		t.Fatal("full transcript cache was not restored")
	}
}

func TestActivityProducerVersionIsCheckedInSharedRecord(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	c := NewForCode("producer-v1")
	snapshot := c.Snapshot(path)
	stamp, _ := time.Parse(time.RFC3339, "2026-10-25T10:00:00Z")
	if reason := c.StoreEvents(path, snapshot, []transcript.Event{{Time: stamp, Role: transcript.User, Text: "hello"}}, nil, nil, false, stamp.Add(-time.Hour), stamp.Add(time.Hour), transcript.Meta{}); reason != "" {
		t.Fatal(reason)
	}
	if cached, ok, err := NewForCode("producer-v2").CachedEvents(path); err != nil || !ok || cached.ActivityComplete {
		t.Fatal("activity from a different producer version was trusted")
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

func TestBadHeaderAndLiveFiles(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(row+"\n{broken needle\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	c := New()
	_, m, ok, err := c.Events(path, false)
	if err != nil || !ok || m.Bad != 1 {
		t.Fatalf("cold: %+v %v", m, err)
	}
	entry, err := os.Stat(c.path(path))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, _, ok, err = c.Events(path, true)
		if err != nil || ok || c.Misses.Load() != 1 {
			t.Fatalf("bad header was rebuilt: %v %v %d", ok, err, c.Misses.Load())
		}
	}
	after, err := os.Stat(c.path(path))
	if err != nil || !os.SameFile(entry, after) {
		t.Fatal("bad entry was replaced")
	}
	if err := os.Chtimes(path, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	_, _, ok, err = c.Events(path, true)
	if err != nil || ok || c.Misses.Load() != 1 || c.Valid(path, true) {
		t.Fatal("live transcript was cached")
	}
}

func TestCachePrivacyAndAvailability(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root обходится без разрешения файла")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	c := New()
	wantDir := cacheCode + "-" + runtime.Version() + "-" + runtime.GOOS + "-" + runtime.GOARCH
	if filepath.Base(c.dir) != wantDir {
		t.Fatal("code fingerprint missing from directory")
	}
	if _, _, ok, err := c.Events(path, false); !ok || err != nil {
		t.Fatalf("cold: %v", err)
	}
	entry := c.path(path)
	if st, err := os.Stat(c.dir); err != nil || st.Mode().Perm() != 0700 {
		t.Fatal("cache directory permissions")
	}
	if st, err := os.Stat(entry); err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("cache entry permissions")
	}
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	// chmod меняет ctime: обновляем ключ, чтобы проверить именно доступ к оригиналу.
	f, err := os.Open(entry)
	if err != nil {
		t.Fatal(err)
	}
	decoder := gob.NewDecoder(f)
	var saved header
	var payload []byte
	if err := decoder.Decode(&saved); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	saved.Key, err = fileKey(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err = os.Create(entry)
	if err != nil {
		t.Fatal(err)
	}
	encoder := gob.NewEncoder(f)
	if err := encoder.Encode(saved); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(payload); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if c.Valid(path, false) {
		t.Fatal("unreadable transcript accepted")
	}
	if _, _, ok, err := c.Events(path, false); ok || err == nil {
		t.Fatal("read error hidden")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := c.Events(path, false); !ok || err != nil {
		t.Fatalf("rewarm: %v", err)
	}
	if err := os.Chmod(c.dir, 0755); err != nil {
		t.Fatal(err)
	}
	if c.Valid(path, false) {
		t.Fatal("public cache directory accepted")
	}
	if err := os.Chmod(c.dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(entry, 0644); err != nil {
		t.Fatal(err)
	}
	if c.Valid(path, false) {
		t.Fatal("public cache entry accepted")
	}
	if err := os.Chmod(entry, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(row+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if c.Valid(path, false) {
		t.Fatal("changed transcript accepted")
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(c.dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(c.dir, 0700) })
	before := c.Misses.Load()
	if _, _, ok, err := c.Events(path, true); err != nil || ok || c.Misses.Load() != before {
		t.Fatal("unwritable cache caused a full read")
	}
}

func TestUnsafeDirectoryRejectsReadsAndWrites(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	c := New()
	snapshot := c.Snapshot(path)
	stamp, _ := time.Parse(time.RFC3339, "2026-10-25T10:00:00Z")
	if reason := c.StoreEvents(path, snapshot, []transcript.Event{{Time: stamp, Role: transcript.User, Text: "hello"}}, nil, nil, false, stamp.Add(-time.Hour), stamp.Add(time.Hour), transcript.Meta{}); reason != "" {
		t.Fatal(reason)
	}
	entry := c.path(path)
	if err := os.Chmod(c.dir, 0777); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.CachedEvents(path); err == nil || ok {
		t.Fatal("world-writable cache directory accepted")
	}
	if reason := c.StoreEvents(path, snapshot, nil, nil, nil, false, stamp.Add(-time.Hour), stamp.Add(time.Hour), transcript.Meta{}); reason != "" {
		t.Fatalf("optional cache write returned an error: %s", reason)
	}
	if err := os.Chmod(c.dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("existing private entry disappeared: %v", err)
	}
	if cached, ok, err := c.CachedEvents(path); err != nil || !ok || len(cached.Events) != 1 {
		t.Fatal("unsafe write replaced the existing cache entry")
	}
}

func TestCleanup(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	c := New()
	if err := os.MkdirAll(c.dir, 0700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(c.root, "old-code")
	if err := os.MkdirAll(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(0, time.Now().UnixNano()/1024*1024)
	writeEntry := func(name, source string, size int64, stamp time.Time) string {
		t.Helper()
		p := filepath.Join(c.dir, name)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := gob.NewEncoder(f).Encode(header{Key: key{Path: source}}); err != nil {
			t.Fatal(err)
		}
		if size > 0 {
			if err := f.Truncate(size); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return p
	}
	source := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(source, nil, 0600); err != nil {
		t.Fatal(err)
	}
	expired := writeEntry("expired", source, 0, now.Add(-maxAge-time.Hour))
	orphan := writeEntry("orphan", source+"-missing", 0, now)
	c.Cleanup(now)
	for _, p := range []string{foreign, expired, orphan} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("not removed: %s, %v", p, err)
		}
	}
	oldest := writeEntry("oldest", source, 1024, now.Add(-time.Hour))
	large := writeEntry("large", source, maxCache, now)
	c.Cleanup(now)
	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Fatalf("oldest entry not removed: %v", err)
	}
	if _, err := os.Stat(large); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupSkipsInfoError(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	c := New()
	if err := os.MkdirAll(c.dir, 0700); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(c.dir, "a-blocked")
	expired := filepath.Join(c.dir, "b-expired")
	for _, path := range []string{blocked, expired} {
		if err := os.WriteFile(path, []byte("irrelevant"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Unix(0, time.Now().UnixNano()/1024*1024)
	old := now.Add(-maxAge - time.Hour)
	if err := os.Chtimes(expired, old, old); err != nil {
		t.Fatal(err)
	}
	previous := cleanupInfo
	cleanupInfo = func(entry os.DirEntry) (os.FileInfo, error) {
		if entry.Name() == "a-blocked" {
			return nil, os.ErrPermission
		}
		return entry.Info()
	}
	t.Cleanup(func() { cleanupInfo = previous })
	c.Cleanup(now)
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("cleanup stopped on another entry: %v", err)
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
