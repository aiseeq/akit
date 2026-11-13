package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func cacheTestFile(t *testing.T, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "docs", "tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "20200101-1000-MED-cache-test.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return path
}

func cacheTestBody(statusText, note string) string {
	return "# Cache test\n\nСтатус: " + statusText + "\n\n## Что сделать\nsynthetic\n\n## Журнал\n" + note + "\n"
}

func useTempParseCache(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("TK_NO_PARSE_CACHE", "")
}

func TestParseCacheSkipsFileChangedDuringRead(t *testing.T) {
	useTempParseCache(t)
	path := cacheTestFile(t, cacheTestBody("открыт", "before"))
	parseCacheAfterStat = func(got string) {
		if got == path {
			parseCacheAfterStat = nil
			if err := os.WriteFile(path, []byte(cacheTestBody("готово", "after")), 0o600); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	defer func() { parseCacheAfterStat = nil; parseCacheBeforeRead = nil }()
	got, err := loadCachedTask(path, true)
	if err != nil || got.status != "готово" || !bytes.Contains([]byte(got.body), []byte("after")) {
		t.Fatalf("task = %#v, %v", got, err)
	}
	cachePath, _, err := parseCachePath(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("racy read was cached: %v", err)
	}
}

func TestParseCacheRereadsSameSizeEditWithCoarseTime(t *testing.T) {
	useTempParseCache(t)
	path := cacheTestFile(t, cacheTestBody("открыт", "first"))
	reads := 0
	parseCacheBeforeRead = func(string) { reads++ }
	defer func() { parseCacheAfterStat = nil; parseCacheBeforeRead = nil }()
	if _, err := loadCachedTask(path, true); err != nil {
		t.Fatal(err)
	}
	body := cacheTestBody("открыт", "other")
	if len(body) != len(cacheTestBody("открыт", "first")) {
		t.Fatal("fixture sizes differ")
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
	got, err := loadCachedTask(path, true)
	if err != nil || !bytes.Contains([]byte(got.body), []byte("other")) || reads != 2 {
		t.Fatalf("task=%q reads=%d err=%v", got.body, reads, err)
	}
}

func TestParseCacheDistrustsEntryWrittenInSameTick(t *testing.T) {
	useTempParseCache(t)
	oldBody := cacheTestBody("открыт", "before")
	path := cacheTestFile(t, oldBody)
	if _, err := loadCachedTask(path, true); err != nil {
		t.Fatal(err)
	}
	cachePath, key, err := parseCachePath(path)
	if err != nil {
		t.Fatal(err)
	}
	stale := readParseCacheEntries(cachePath)[key]
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	newBody := cacheTestBody("готово", "after!")
	if len(oldBody) != len(newBody) {
		t.Fatalf("fixture sizes differ: %d != %d", len(oldBody), len(newBody))
	}
	if err := os.WriteFile(path, []byte(newBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stale.Size = current.Size()
	stale.ModNS, stale.CTimeNS = cacheStat(current)
	stale.Written = stale.ModNS
	writeParseCacheEntries(cachePath, map[string]parseCacheEntry{key: stale})
	got, err := loadCachedTask(path, true)
	if err != nil || got.status != "готово" {
		t.Fatalf("same-tick cache reused stale status %q: %v", got.status, err)
	}
}

func TestParseCacheConcurrentRootsKeepEachOther(t *testing.T) {
	useTempParseCache(t)
	first := cacheTestFile(t, cacheTestBody("открыт", "first"))
	second := cacheTestFile(t, cacheTestBody("ждёт", "second"))
	var wg sync.WaitGroup
	for _, path := range []string{first, second} {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			if _, err := loadCachedTask(p, true); err != nil {
				t.Error(err)
			}
		}(path)
	}
	wg.Wait()
	for _, path := range []string{first, second} {
		if _, ok := readParseCacheEntryForTest(t, path); !ok {
			t.Errorf("cache missing for %s", filepath.Base(path))
		}
	}
}

func TestParseCacheSubsetOfRootsKeepsOtherRoot(t *testing.T) {
	useTempParseCache(t)
	first := cacheTestFile(t, cacheTestBody("открыт", "first"))
	second := cacheTestFile(t, cacheTestBody("ждёт", "second"))
	for _, p := range []string{first, second} {
		if _, err := loadCachedTask(p, true); err != nil {
			t.Fatal(err)
		}
	}
	reads := 0
	parseCacheBeforeRead = func(string) { reads++ }
	defer func() { parseCacheAfterStat = nil; parseCacheBeforeRead = nil }()
	if _, err := loadCachedTask(first, true); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCachedTask(second, true); err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Fatalf("warm subset reread %d ticket files", reads)
	}
}

func TestParseCacheWalkErrorKeepsEntries(t *testing.T) {
	useTempParseCache(t)
	path := cacheTestFile(t, cacheTestBody("открыт", "valid"))
	if _, err := loadCachedTask(path, true); err != nil {
		t.Fatal(err)
	}
	cachePath, _, err := parseCachePath(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	moved := dir + "-saved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTasksMode(dir, true, true); err == nil {
		t.Fatal("regular file used as task directory did not fail")
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache entry removed after traversal error: %v", err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, dir); err != nil {
		t.Fatal(err)
	}
}

func TestParseCacheWarmReadFitsBudget(t *testing.T) {
	useTempParseCache(t)
	path := cacheTestFile(t, cacheTestBody("открыт", string(bytes.Repeat([]byte("synthetic journal text\n"), 12000))))
	if _, err := loadCachedTask(path, true); err != nil {
		t.Fatal(err)
	}
	cachePath, _, err := parseCachePath(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode = %o, want 600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(cachePath))
	if err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("cache directory mode = %v, %v", dirInfo, err)
	}
	if info.Size() >= 2*1024*1024 {
		t.Fatalf("compressed cache is %d bytes", info.Size())
	}
	got, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var cacheFile parseCacheFile
	if err := json.Unmarshal(decoded, &cacheFile); err != nil || cacheFile.Version != parseCacheVersion {
		t.Fatalf("cache version=%d err=%v", cacheFile.Version, err)
	}
}

func TestParseCacheIgnoresCorruptOrForeignVersion(t *testing.T) {
	useTempParseCache(t)
	path := cacheTestFile(t, cacheTestBody("открыт", "fresh"))
	cachePath, key, err := parseCachePath(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadCachedTask(path, true)
	if err != nil || !bytes.Contains([]byte(got.body), []byte("fresh")) {
		t.Fatalf("corrupt cache fallback: %#v %v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mod, ct := cacheStat(info)
	entry := parseCacheEntry{Version: parseCacheVersion + 1, Key: key, Size: info.Size(), ModNS: mod, CTimeNS: ct, Written: time.Now().Add(-time.Hour - 5*time.Second).UnixNano()}
	data, _ := json.Marshal(parseCacheFile{Version: parseCacheVersion, Entries: map[string]parseCacheEntry{key: entry}})
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = zw.Write(data)
	_ = zw.Close()
	if err := os.WriteFile(cachePath, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = loadCachedTask(path, true)
	if err != nil || !bytes.Contains([]byte(got.body), []byte("fresh")) {
		t.Fatalf("foreign cache fallback: %#v %v", got, err)
	}
}

func TestParseCacheTimeAndSessionFieldsMatchUncached(t *testing.T) {
	useTempParseCache(t)
	path := cacheTestFile(t, cacheTestBody("ждёт", "ждёт до 23:59"))
	dir := filepath.Dir(path)
	root := filepath.Dir(filepath.Dir(dir))
	run := func(disabled bool) string {
		if disabled {
			_ = os.Setenv("TK_NO_PARSE_CACHE", "1")
		} else {
			_ = os.Unsetenv("TK_NO_PARSE_CACHE")
		}
		var out, errout bytes.Buffer
		if code := list(root, options{all: true, json: true}, &out, &errout); code != 0 {
			t.Fatalf("list=%d stderr=%s", code, errout.String())
		}
		return out.String()
	}
	cached := run(false)
	uncached := run(true)
	if cached != uncached {
		t.Fatalf("cached JSON differs from disabled cache\ncached: %s\nplain: %s", cached, uncached)
	}
}

func TestParseCacheNoteRefreshesNextList(t *testing.T) {
	useTempParseCache(t)
	root, dir := testRunMainRoot(t)
	path := filepath.Join(dir, "20200101-1000-MED-cache-note.md")
	if err := os.WriteFile(path, []byte(cacheTestBody("открыт", "initial")), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := list(root, options{all: true, json: true}, &out, &errout); code != 0 {
		t.Fatalf("warm list: %d %s", code, errout.String())
	}
	out.Reset()
	errout.Reset()
	if code := runMain([]string{"note", "cache-note", "updated", "--by", "tester"}, time.Now(), &out, &errout); code != 0 {
		t.Fatalf("tk note: %d %s", code, errout.String())
	}
	out.Reset()
	errout.Reset()
	if code := list(root, options{all: true, json: true}, &out, &errout); code != 0 {
		t.Fatalf("updated list: %d %s", code, errout.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("tester")) {
		t.Fatalf("next ls missed note actor: %s", out.String())
	}
}

func TestParseCacheRepresentativeDirectoryBudget(t *testing.T) {
	useTempParseCache(t)
	dir := filepath.Join(t.TempDir(), "docs", "tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for i := range 2200 {
		name := fmt.Sprintf("20200101-1000-MED-cache-%04d.md", i)
		body := fmt.Sprintf("# Synthetic ticket %04d\n\nСтатус: открыт\n\n## Что сделать\nsynthetic\n\n## Приёмка\nsynthetic\n", i)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := loadCachedTasks(dir); err != nil {
		t.Fatal(err)
	}
	cachePath, _, err := parseCachePath(filepath.Join(dir, "20200101-1000-MED-cache-0000.md"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= 2*1024*1024 {
		t.Fatalf("2200 parsed records use %d cache bytes", info.Size())
	}
	reads := 0
	parseCacheBeforeRead = func(string) { reads++ }
	defer func() { parseCacheAfterStat = nil; parseCacheBeforeRead = nil }()
	if _, err := loadCachedTasks(dir); err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Fatalf("warm scan reread %d ticket files", reads)
	}
}

func readParseCacheEntryForTest(t *testing.T, ticket string) (parseCacheEntry, bool) {
	t.Helper()
	path, key, err := parseCachePath(ticket)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return parseCacheEntry{}, false
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return parseCacheEntry{}, false
	}
	data, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil {
		return parseCacheEntry{}, false
	}
	var cacheFile parseCacheFile
	if json.Unmarshal(data, &cacheFile) != nil {
		return parseCacheEntry{}, false
	}
	entry, ok := cacheFile.Entries[key]
	return entry, ok
}
