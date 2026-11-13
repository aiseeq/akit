package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func cacheProject(t *testing.T, body string) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	gateDir := filepath.Join(home, ".local", "state", "queue")
	if err := os.MkdirAll(gateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	gate, err := json.Marshal(map[string]any{
		"at": time.Now().Add(-time.Minute).Format(time.RFC3339), "live": []string{filepath.Base(root)},
		"open": []string{"MED-DO", "MED-HUNT", "MED-ARCH"}, "open_by_root": map[string][]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gateDir, "gate.json"), gate, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "20200101-1000-MED-cache-test.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("TK_NO_PARSE_CACHE", "")
	return root, path
}

func cacheBody(titleText, statusText, extra string) string {
	return "# " + titleText + "\n\nСтатус: " + statusText + "\nИсполнитель: worker\nЗаблокирован: 20200101-0900-MED-blocker.md\n\n## План\n- verify: go test ./...\n\n## Журнал\n- 2020-01-02 11:30 note — actor: " + extra + "\n"
}

func runCacheList(t *testing.T, root string, disabled bool) (int, string, string) {
	t.Helper()
	if disabled {
		t.Setenv("TK_NO_PARSE_CACHE", "1")
	} else {
		t.Setenv("TK_NO_PARSE_CACHE", "")
	}
	var out, errout bytes.Buffer
	code := list(root, options{all: true, json: true, root: root}, &out, &errout)
	return code, out.String(), errout.String()
}

func assertCacheEqual(t *testing.T, root string) string {
	t.Helper()
	if code, _, stderr := runCacheList(t, root, false); code != 0 {
		t.Fatalf("warm list: %d %s", code, stderr)
	}
	code, got, stderr := runCacheList(t, root, false)
	plainCode, want, plainErr := runCacheList(t, root, true)
	if code != 0 || plainCode != 0 || got != want || stderr != plainErr {
		t.Fatalf("cached (%d) %s%s != uncached (%d) %s%s", code, got, stderr, plainCode, want, plainErr)
	}
	return got
}

func TestParseCacheReadRaceUsesUncachedBytes(t *testing.T) {
	root, path := cacheProject(t, cacheBody("before", "открыт", "first"))
	oldInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	oldCTime := parseCacheCTime
	parseCacheCTime = func(os.FileInfo) int64 { return 17 }
	parseCacheAfterStat = func(got string) {
		if got == path {
			parseCacheAfterStat = nil
			replacement := path + ".replacement"
			_ = os.WriteFile(replacement, []byte(cacheBody("after", "готово", "second")), 0o600)
			_ = os.Chtimes(replacement, oldInfo.ModTime(), oldInfo.ModTime())
			_ = os.Rename(replacement, path)
		}
	}
	defer func() { parseCacheAfterStat = nil; parseCacheCTime = oldCTime }()
	_, got, _ := runCacheList(t, root, false)
	if !bytes.Contains([]byte(got), []byte("after")) {
		t.Fatalf("list missed concurrent edit: %s", got)
	}
	cachePath, key, err := parseCachePath(path)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := parseCacheBinaryIdentity()
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := readParseCacheEntries(cachePath, binary)
	if _, ok := entries[key]; ok {
		t.Fatal("unstable read was added to cache")
	}
	assertCacheEqual(t, root)
}

func TestParseCacheStatReadMetadataChangesAreNotCached(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		change    func(string, os.FileInfo) error
		ctimeHook bool
	}{
		{name: "size", body: cacheBody("expanded", "открыт", "x"), ctimeHook: true},
		{name: "mtime", body: cacheBody("before", "открыт", "x"), ctimeHook: true, change: func(path string, old os.FileInfo) error {
			return os.Chtimes(path, old.ModTime().Add(time.Hour), old.ModTime().Add(time.Hour))
		}},
		{name: "ctime", body: cacheBody("target", "открыт", "x"), change: func(path string, old os.FileInfo) error { return os.Chtimes(path, old.ModTime(), old.ModTime()) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, path := cacheProject(t, cacheBody("before", "открыт", "x"))
			oldInfo, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			oldCTime := parseCacheCTime
			if tc.ctimeHook {
				parseCacheCTime = func(os.FileInfo) int64 { return 29 }
			}
			defer func() { parseCacheCTime = oldCTime; parseCacheAfterStat = nil }()
			parseCacheAfterStat = func(got string) {
				if got != path {
					return
				}
				parseCacheAfterStat = nil
				if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
				if tc.change != nil {
					if err := tc.change(path, oldInfo); err != nil {
						t.Fatal(err)
					}
				} else if tc.name == "size" {
					_ = os.Chtimes(path, oldInfo.ModTime(), oldInfo.ModTime())
				}
			}
			code, _, stderr := runCacheList(t, root, false)
			if code != 0 {
				t.Fatalf("list: %d %s", code, stderr)
			}

			cachePath, key, err := parseCachePath(path)
			if err != nil {
				t.Fatal(err)
			}
			binary, err := parseCacheBinaryIdentity()
			if err != nil {
				t.Fatal(err)
			}
			entries, _ := readParseCacheEntries(cachePath, binary)
			if _, ok := entries[key]; ok {
				t.Fatalf("%s change was cached", tc.name)
			}
		})
	}
}

func TestParseCacheSameSizeEditWithMtimeMovedBack(t *testing.T) {
	root, path := cacheProject(t, cacheBody("title-a", "открыт", "first"))
	assertCacheEqual(t, root)
	old, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := cacheBody("title-b", "готово", "first")
	if len(updated) != int(old.Size()) {
		t.Fatal("synthetic fixture sizes differ")
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old.ModTime(), old.ModTime()); err != nil {
		t.Fatal(err)
	}
	got := assertCacheEqual(t, root)
	if !bytes.Contains([]byte(got), []byte("title-b")) {
		t.Fatal("same-size edit was not observed")
	}
}

func TestParseCacheMtimeChangeInvalidatesWithStableCtime(t *testing.T) {
	root, path := cacheProject(t, cacheBody("title-a", "открыт", "x"))
	oldCTime := parseCacheCTime
	parseCacheCTime = func(os.FileInfo) int64 { return 23 }
	defer func() { parseCacheCTime = oldCTime }()
	assertCacheEqual(t, root)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := cacheBody("title-b", "готово", "x")
	if len(updated) != int(info.Size()) {
		t.Fatal("synthetic fixture sizes differ")
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	changedMtime := info.ModTime().Add(time.Hour)
	if err := os.Chtimes(path, changedMtime, changedMtime); err != nil {
		t.Fatal(err)
	}
	got := assertCacheEqual(t, root)
	if !bytes.Contains([]byte(got), []byte("title-b")) {
		t.Fatal("mtime change was not observed")
	}
}

func TestParseCacheSizeChangeInvalidatesEntry(t *testing.T) {
	root, path := cacheProject(t, cacheBody("short", "открыт", "x"))
	oldCTime := parseCacheCTime
	parseCacheCTime = func(os.FileInfo) int64 { return 19 }
	defer func() { parseCacheCTime = oldCTime }()
	assertCacheEqual(t, root)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(cacheBody("longer title", "готово", "x")), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(path, before.ModTime(), before.ModTime())
	got := assertCacheEqual(t, root)
	if !bytes.Contains([]byte(got), []byte("longer title")) {
		t.Fatal("size change was missed")
	}
}

func TestParseCacheRecentMtimeDoesNotTrustOldEntry(t *testing.T) {
	root, path := cacheProject(t, cacheBody("old", "открыт", "x"))
	assertCacheEqual(t, root)
	cachePath, key, err := parseCachePath(path)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := parseCacheBinaryIdentity()
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := readParseCacheEntries(cachePath, binary)
	entry := entries[key]
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(cacheBody("new", "готово", "x")), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(path, before.ModTime(), before.ModTime())
	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	entry.Size = current.Size()
	entry.ModNS, entry.CTimeNS, entry.FileID = cacheStat(current)
	entry.Written = entry.ModNS + int64(5*time.Second)
	entries[key] = entry
	if err := writeParseCacheEntries(cachePath, binary, entries, nil); err != nil {
		t.Fatal(err)
	}
	parseCacheNow = func() time.Time { return current.ModTime().Add(time.Second) }
	defer func() { parseCacheNow = time.Now }()
	got := assertCacheEqual(t, root)
	if !bytes.Contains([]byte(got), []byte("new")) {
		t.Fatal("recent mtime entry was trusted")
	}
}

func TestParseCacheSameTickEntryIsDistrusted(t *testing.T) {
	root, path := cacheProject(t, cacheBody("stale", "открыт", "first"))
	assertCacheEqual(t, root)
	cachePath, key, err := parseCachePath(path)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := parseCacheBinaryIdentity()
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := readParseCacheEntries(cachePath, binary)
	entry := entries[key]
	oldInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := cacheBody("fresh", "открыт", "first")
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, oldInfo.ModTime(), oldInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	entry.Size = current.Size()
	entry.ModNS, entry.CTimeNS, entry.FileID = cacheStat(current)
	entry.Written = entry.ModNS
	entries[key] = entry
	if err := writeParseCacheEntries(cachePath, binary, entries, nil); err != nil {
		t.Fatal(err)
	}
	got := assertCacheEqual(t, root)
	if !bytes.Contains([]byte(got), []byte("fresh")) {
		t.Fatal("same-tick cache entry was reused")
	}
}

func TestParseCacheRenameReplaceRefreshesTicket(t *testing.T) {
	root, path := cacheProject(t, cacheBody("before", "открыт", "one"))
	oldCTime := parseCacheCTime
	parseCacheCTime = func(os.FileInfo) int64 { return 31 }
	defer func() { parseCacheCTime = oldCTime }()
	assertCacheEqual(t, root)
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(cacheBody("target", "готово", "two")), 0o600); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Stat(path)
	_ = os.Chtimes(tmp, old.ModTime(), old.ModTime())
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if got := assertCacheEqual(t, root); !bytes.Contains([]byte(got), []byte("target")) {
		t.Fatal("rename replacement was missed")
	}
}

func TestParseCacheDeleteCreateSameNameRefreshesTicket(t *testing.T) {
	root, path := cacheProject(t, cacheBody("before", "открыт", "one"))
	oldCTime := parseCacheCTime
	parseCacheCTime = func(os.FileInfo) int64 { return 47 }
	defer func() { parseCacheCTime = oldCTime }()
	assertCacheEqual(t, root)
	old, _ := os.Stat(path)
	replacement := path + ".replacement"
	if err := os.WriteFile(replacement, []byte(cacheBody("target", "готово", "two")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(replacement, old.ModTime(), old.ModTime())
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if got := assertCacheEqual(t, root); !bytes.Contains([]byte(got), []byte("target")) {
		t.Fatal("recreated ticket was missed")
	}
}

func TestParseCacheSymlinkRootSwapRefreshesTicket(t *testing.T) {
	parent := t.TempDir()
	a := filepath.Join(parent, "a")
	b := filepath.Join(parent, "b")
	link := filepath.Join(parent, "project")
	for _, item := range []struct{ root, title string }{{a, "first"}, {b, "second"}} {
		dir := filepath.Join(item.root, "docs", "tasks")
		_ = os.MkdirAll(dir, 0o700)
		_ = os.Mkdir(filepath.Join(item.root, ".git"), 0o700)
		_ = os.WriteFile(filepath.Join(dir, "20200101-1000-MED-cache-test.md"), []byte(cacheBody(item.title, "открыт", "x")), 0o600)
	}
	_ = os.Symlink(a, link)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("TK_NO_PARSE_CACHE", "")
	_, _, _ = runCacheList(t, link, false)
	_ = os.Remove(link)
	_ = os.Symlink(b, link)
	got := assertCacheEqual(t, link)
	if !bytes.Contains([]byte(got), []byte("second")) {
		t.Fatal("symlink root replacement was missed")
	}
}

func TestParseCacheExecutableIdentityInvalidatesEntries(t *testing.T) {
	root, _ := cacheProject(t, cacheBody("binary", "открыт", "body"))
	assertCacheEqual(t, root)
	fakeBinary := filepath.Join(t.TempDir(), filepath.Base(os.Args[0]))
	if err := os.WriteFile(fakeBinary, []byte("different executable identity"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := parseCacheExecutableInfo
	parseCacheExecutableInfo = func() (os.FileInfo, error) {
		return os.Stat(fakeBinary)
	}
	defer func() { parseCacheExecutableInfo = old }()
	reads := 0
	oldRead := parseCacheBeforeRead
	parseCacheBeforeRead = func(string) { reads++ }
	defer func() { parseCacheBeforeRead = oldRead }()
	if got := assertCacheEqual(t, root); !bytes.Contains([]byte(got), []byte("binary")) || reads == 0 {
		t.Fatalf("binary identity did not force ticket parse: reads=%d output=%s", reads, got)
	}
}

func TestParseCacheTimezoneAndDynamicFieldsMatchUncached(t *testing.T) {
	root, _ := cacheProject(t, cacheBody("local", "ждёт", "ждёт до 23:59"))
	original := time.Local
	defer func() { time.Local = original }()
	time.Local = time.FixedZone("first", 3600)
	assertCacheEqual(t, root)
	time.Local = time.FixedZone("second", 9*3600)
	got := assertCacheEqual(t, root)
	if !bytes.Contains([]byte(got), []byte("+09:00")) {
		t.Fatalf("cache retained old timezone: %s", got)
	}
}

func cacheOutputDiff(got, first string) string {
	return "--- got\n+++ first\n-" + got + "\n+" + first
}

func TestParseCacheWarmFactsAndFolderReuse(t *testing.T) {
	root, _ := cacheProject(t, cacheBody("facts", "открыт", "journal"))
	reads, writes, ticketReads := 0, 0, 0
	oldRead, oldWrite, oldTicketRead := parseCacheBeforeFileRead, parseCacheBeforeFileWrite, parseCacheBeforeRead
	parseCacheBeforeFileRead = func(string) { reads++ }
	parseCacheBeforeFileWrite = func(string) { writes++ }
	parseCacheBeforeRead = func(string) { ticketReads++ }
	defer func() {
		parseCacheBeforeFileRead = oldRead
		parseCacheBeforeFileWrite = oldWrite
		parseCacheBeforeRead = oldTicketRead
	}()
	first := assertCacheEqual(t, root)
	reads, writes, ticketReads = 0, 0, 0
	_, got, _ := runCacheList(t, root, false)
	if reads != 1 || writes != 0 || ticketReads != 0 {
		t.Fatalf("warm cache operations: cache_reads=%d cache_writes=%d ticket_reads=%d", reads, writes, ticketReads)
	}
	_, uncached, _ := runCacheList(t, root, true)
	if got != uncached {
		t.Fatalf("warm cache differs from uncached output:\n%s", cacheOutputDiff(got, uncached))
	}
	if !bytes.Contains([]byte(got), []byte("worker")) || !bytes.Contains([]byte(got), []byte("verify")) || !bytes.Contains([]byte(got), []byte("actor")) || !bytes.Contains([]byte(got), []byte("blocker")) || !bytes.Contains([]byte(got), []byte("next_rank")) || !bytes.Contains([]byte(got), []byte("can_take")) {
		t.Fatalf("cached parsed facts/body missing; got/first diff:\n%s", cacheOutputDiff(got, first))
	}
}

func TestParseCacheConcurrentAndSubsetRootsKeepEntries(t *testing.T) {
	first, _ := cacheProject(t, cacheBody("one", "открыт", "one"))
	second, _ := cacheProject(t, cacheBody("two", "открыт", "two"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("TK_NO_PARSE_CACHE", "")
	var wg sync.WaitGroup
	for range 20 {
		for _, root := range []string{first, second} {
			wg.Add(1)
			go func(r string) {
				defer wg.Done()
				var out, errout bytes.Buffer
				if code := list(r, options{all: true, json: true, root: r}, &out, &errout); code != 0 {
					t.Errorf("list: %d %s", code, errout.String())
				}
			}(root)
		}
	}
	wg.Wait()
	assertCacheEqual(t, first)
	assertCacheEqual(t, second)
	ticketReads := 0
	oldTicketRead := parseCacheBeforeRead
	parseCacheBeforeRead = func(string) { ticketReads++ }
	defer func() { parseCacheBeforeRead = oldTicketRead }()
	for _, root := range []string{first, second} {
		if code, _, stderr := runCacheList(t, root, false); code != 0 {
			t.Fatalf("subset list: %d %s", code, stderr)
		}
	}
	if ticketReads != 0 {
		t.Fatalf("subset warm scan reread %d tickets", ticketReads)
	}
}

func TestParseCacheUnavailableAndDisabledFallBack(t *testing.T) {
	root, _ := cacheProject(t, cacheBody("fallback", "открыт", "x"))
	cacheHome := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(cacheHome, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	t.Setenv("HOME", t.TempDir())
	_, got, stderr := runCacheList(t, root, false)
	_, want, _ := runCacheList(t, root, true)
	if got != want {
		t.Fatalf("cache error changed output: %s / %s (%s)", got, want, stderr)
	}
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("TK_NO_PARSE_CACHE", "1")
	paths, reads, writes := 0, 0, 0
	oldPath, oldRead, oldWrite := parseCacheBeforePath, parseCacheBeforeFileRead, parseCacheBeforeFileWrite
	parseCacheBeforePath = func() { paths++ }
	parseCacheBeforeFileRead = func(string) { reads++ }
	parseCacheBeforeFileWrite = func(string) { writes++ }
	defer func() {
		parseCacheBeforePath = oldPath
		parseCacheBeforeFileRead = oldRead
		parseCacheBeforeFileWrite = oldWrite
	}()
	var out, errout bytes.Buffer
	if code := list(root, options{all: true, json: true, root: root}, &out, &errout); code != 0 {
		t.Fatalf("disabled cache: %d %s", code, errout.String())
	}
	if out.String() != want {
		t.Fatalf("disabled cache output changed without cache home: got %s want %s", out.String(), want)
	}
	if paths+reads+writes != 0 {
		t.Fatalf("disabled cache touched cache: path=%d read=%d write=%d", paths, reads, writes)
	}
	t.Setenv("TK_NO_PARSE_CACHE", "")
	var missingHomeOut, missingHomeErr bytes.Buffer
	if code := list(root, options{all: true, json: true, root: root}, &missingHomeOut, &missingHomeErr); code != 0 || missingHomeOut.String() != want {
		t.Fatalf("missing cache home changed ls: code=%d output=%s want=%s stderr=%s", code, missingHomeOut.String(), want, missingHomeErr.String())
	}
}

func TestParseCacheWalkErrorDoesNotPruneEntries(t *testing.T) {
	root, ticket := cacheProject(t, cacheBody("kept", "открыт", "x"))
	assertCacheEqual(t, root)
	cachePath, key, err := parseCachePath(ticket)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := parseCacheBinaryIdentity()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(ticket)
	moved := dir + ".saved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCacheList(t, root, false); code == 0 {
		t.Fatal("walk error did not fail ls")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := readParseCacheEntries(cachePath, binary)
	if _, ok := entries[key]; !ok {
		t.Fatal("walk error pruned ticket cache entry")
	}
}

func TestParseCacheNoteRefreshesNextList(t *testing.T) {
	root, dir := testRunMainRoot(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("TK_NO_PARSE_CACHE", "")
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "20200101-1000-MED-cache-note.md")
	if err := os.WriteFile(path, []byte(cacheBody("note", "открыт", "initial")), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = os.Chtimes(path, old, old)
	if code, _, stderr := runCacheList(t, root, false); code != 0 {
		t.Fatalf("warm list: %d %s", code, stderr)
	}
	var out, errout bytes.Buffer
	if code := runMain([]string{"note", "cache-note", "updated", "--by", "worker"}, time.Now(), &out, &errout); code != 0 {
		t.Fatalf("note: %d %s", code, errout.String())
	}
	got := assertCacheEqual(t, root)
	if bytes.Contains([]byte(got), []byte("2020-01-02T11:30")) {
		t.Fatalf("next list retained the pre-note change time: %s", got)
	}
	body, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(body, []byte("updated")) {
		t.Fatalf("note fixture was not updated: %v", err)
	}
}

func TestParseCacheBlockStatusPreservesCRLFBehavior(t *testing.T) {
	root, dir := testRunMainRoot(t)
	blocker := filepath.Join(dir, "20200101-0900-MED-cache-blocker.md")
	if err := os.WriteFile(blocker, []byte("# Blocker\nСтатус: открыт\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(dir, "20200101-1000-MED-cache-parent.md")
	body := "# Parent\nСтатус: открыт\nЗаблокирован: cache-blocker\n"
	if err := os.WriteFile(parent, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := list(root, options{all: true}, &out, &errout); code != 0 {
		t.Fatalf("list: %d %s", code, errout.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("cache-parent")) || bytes.Contains(out.Bytes(), []byte("заблокирован: cache-blocker")) {
		t.Fatalf("fast status changed CRLF list behavior: %s", out.String())
	}

}

func TestParseCacheBlockStatusDefaultsMissingStatusToOpen(t *testing.T) {
	root, dir := testRunMainRoot(t)
	blocker := filepath.Join(dir, "20200101-0900-MED-cache-blocker.md")
	if err := os.WriteFile(blocker, []byte("# Blocker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(dir, "20200101-1000-MED-cache-parent.md")
	if err := os.WriteFile(parent, []byte("# Parent\nЗаблокирован: cache-blocker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := list(root, options{all: true}, &out, &errout); code != 0 {
		t.Fatalf("list: %d %s", code, errout.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("заблокирован: cache-blocker")) {
		t.Fatalf("missing implicit open status block: %s", out.String())
	}
}

func TestParseCacheBlockStatusKeepsRenameAndDuplicateSemantics(t *testing.T) {
	root, dir := testRunMainRoot(t)
	parent := filepath.Join(dir, "20200101-1000-MED-cache-parent.md")
	if err := os.WriteFile(parent, []byte("# Parent\nЗаблокирован: cache-old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(dir, "20200101-0900-MED-cache-current.md")
	body := "# Current\nСтатус: открыт\n\n## Журнал\n- 2020-01-02 11:30 переименован — worker: cache-old → cache-current\n"
	if err := os.WriteFile(renamed, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := list(root, options{all: true}, &out, &errout); code != 0 {
		t.Fatalf("rename list: %d %s", code, errout.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("заблокирован: cache-old")) {
		t.Fatalf("rename alias was lost: %s", out.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "20200102-0900-MED-cache-old.md"), []byte("# Duplicate\nСтатус: открыт\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errout.Reset()
	if code := list(root, options{all: true}, &out, &errout); code != 0 {
		t.Fatalf("duplicate list: %d %s", code, errout.String())
	}
	if bytes.Contains(out.Bytes(), []byte("заблокирован: cache-old")) {
		t.Fatalf("ambiguous link was marked blocked: %s", out.String())
	}
}

func TestParseCacheBlockStatusIgnoresArchivedOpenTicket(t *testing.T) {
	root, dir := testRunMainRoot(t)
	archive := filepath.Join(root, "docs", "tasks-archive")
	if err := os.MkdirAll(archive, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archive, "20200101-0900-MED-cache-blocker.md"), []byte("# Blocker\nСтатус: открыт\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "20200101-1000-MED-cache-parent.md"), []byte("# Parent\nЗаблокирован: cache-blocker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := list(root, options{all: true}, &out, &errout); code != 0 {
		t.Fatalf("list: %d %s", code, errout.String())
	}
	if bytes.Contains(out.Bytes(), []byte("заблокирован: cache-blocker")) {
		t.Fatalf("archived ticket still blocks: %s", out.String())
	}
}

func TestParseCacheWarmRankDoesNotRescanLinkedTickets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("TK_NO_PARSE_CACHE", "")
	root := filepath.Join(home, "git")
	for _, repo := range []string{"a", "b"} {
		project := filepath.Join(root, repo)
		if err := os.MkdirAll(filepath.Join(project, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(project, "docs", "tasks"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	parent := filepath.Join(root, "a", "docs", "tasks", "20200101-1000-MED-cache-parent.md")
	if err := os.WriteFile(parent, []byte("# Parent\nСтатус: открыт\nЗаблокирован: git/b/cache-blocker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(root, "b", "docs", "tasks", "20200101-0900-MED-cache-blocker.md")
	if err := os.WriteFile(blocker, []byte("# Blocker\nСтатус: открыт\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertCacheEqual(t, root)
	finds := 0
	oldFind := parseCacheBeforeLinkedFind
	parseCacheBeforeLinkedFind = func() { finds++ }
	defer func() { parseCacheBeforeLinkedFind = oldFind }()
	if code, _, stderr := runCacheList(t, root, false); code != 0 {
		t.Fatalf("warm list: %d %s", code, stderr)
	}
	if finds != 0 {
		t.Fatalf("warm next-rank lookup rescanned %d task directories", finds)
	}
}

func TestParseCacheJSONRankKeepsRenamedReferenceSemantics(t *testing.T) {
	root, ticket := cacheProject(t, "# Parent\nСтатус: открыт\nЗаблокирован: cache-old\n")
	dir := filepath.Dir(ticket)
	current := filepath.Join(dir, "20200101-0900-MED-cache-current.md")
	currentBody := "# Current\nСтатус: открыт\n\n## Журнал\n- 2020-01-02 11:30 переименован — worker: cache-old → cache-current\n"
	if err := os.WriteFile(current, []byte(currentBody), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "20200101-0800-MED-cache-other.md")
	if err := os.WriteFile(other, []byte("# Other\nСтатус: открыт\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertCacheEqual(t, root)
}

func TestParseCacheCorruptAndForeignVersionRebuild(t *testing.T) {
	root, ticket := cacheProject(t, cacheBody("recovered", "открыт", "fresh"))
	assertCacheEqual(t, root)
	cachePath, key, err := parseCachePath(ticket)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{[]byte("invalid gzip"), []byte("\\x1f\\x8b invalid")} {
		if err := os.WriteFile(cachePath, bad, 0o600); err != nil {
			t.Fatal(err)
		}
		got := assertCacheEqual(t, root)
		if !bytes.Contains([]byte(got), []byte("recovered")) {
			t.Fatalf("bad cache changed list: %s", got)
		}
	}
	binary, err := parseCacheBinaryIdentity()
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := readParseCacheEntries(cachePath, binary)
	entry := entries[key]
	entry.Task.Title = "stale"
	entries[key] = entry
	payload, err := json.Marshal(parseCacheFile{Version: parseCacheVersion + 1, Binary: binary, Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	got := assertCacheEqual(t, root)
	if !bytes.Contains([]byte(got), []byte("recovered")) {
		t.Fatalf("foreign version changed list: %s", got)
	}
}

func TestParseCacheDeletedEntriesPrunedAndNestedDirectoriesSkipped(t *testing.T) {
	root, path := cacheProject(t, cacheBody("listed", "открыт", "x"))
	if code, _, stderr := runCacheList(t, root, false); code != 0 {
		t.Fatalf("initial cache list: %d %s", code, stderr)
	}
	cachePath, _, err := parseCachePath(path)
	if err != nil {
		t.Fatal(err)
	}
	initialDir, dirErr := os.Stat(filepath.Dir(cachePath))
	initialFile, fileErr := os.Stat(cachePath)
	if dirErr != nil || initialDir.Mode().Perm() != 0o700 {
		t.Fatalf("new cache directory mode: %v %v", initialDir, dirErr)
	}
	if fileErr != nil || initialFile.Mode().Perm() != 0o600 {
		t.Fatalf("new cache file mode: %v %v", initialFile, fileErr)
	}
	if err := os.Chmod(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cachePath, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCacheList(t, root, false); code != 0 {
		t.Fatalf("warm after chmod: %d %s", code, stderr)
	}
	dirInfo, err := os.Stat(filepath.Dir(cachePath))
	if err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("cache directory mode: %v %v", dirInfo, err)
	}
	fileInfo, err := os.Stat(cachePath)
	if err != nil || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("cache file mode: %v %v", fileInfo, err)
	}
	nested := filepath.Join(filepath.Dir(path), "20200101-1000-MED-cache-nested.md")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCacheList(t, root, false); code != 0 {
		t.Fatalf("list after delete: %d %s", code, stderr)
	}
	binary, err := parseCacheBinaryIdentity()
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := readParseCacheEntries(cachePath, binary)
	if len(entries) != 0 {
		t.Fatalf("deleted entry retained: %d", len(entries))
	}
	info, err := os.Stat(filepath.Dir(cachePath))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("cache directory permissions: %v %v", info, err)
	}
}
