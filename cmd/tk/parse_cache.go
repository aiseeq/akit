package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"syscall"
	"time"
)

const parseCacheVersion = 1

var parseCacheAfterStat func(string)
var parseCacheBeforeRead func(string)

type parseCacheEntry struct {
	Version int        `json:"version"`
	Key     string     `json:"key"`
	Size    int64      `json:"size"`
	ModNS   int64      `json:"mod_ns"`
	CTimeNS int64      `json:"ctime_ns"`
	Written int64      `json:"written_ns"`
	Task    cachedTask `json:"task"`
}

type cachedTask struct {
	Slug, Status, Prio, Diff, Title string
	Created, Changed                time.Time
	Facts                           taskFacts
}

func cacheIdentity(info os.FileInfo) string {
	id := fmt.Sprintf("%s:%d:%d", info.Name(), info.Size(), info.ModTime().UnixNano()) //nolint:time-derived-id // Executable stat changes invalidate cached parse results.
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		id += fmt.Sprintf(":%d:%d:%d", st.Dev, st.Ino, st.Ctim.Sec*1e9+st.Ctim.Nsec)
	}
	return id
}

func parseCachePath(ticket string) (string, string, error) {
	return parseCachePathFor(filepath.Dir(ticket), ticket)
}

func parseCachePathFor(dir, ticket string) (string, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", "", fmt.Errorf("resolve executable for ticket cache: %w", err)
	}
	bin, err := os.Stat(exe)
	if err != nil {
		return "", "", fmt.Errorf("stat executable for ticket cache: %w", err)
	}
	abs, err := filepath.Abs(ticket)
	if err != nil {
		return "", "", fmt.Errorf("resolve ticket path for cache: %w", err)
	}
	identity := fmt.Sprintf("v%d:%s", parseCacheVersion, cacheIdentity(bin))
	key := identity + ":" + abs
	cacheKey := identity + ":" + dir
	digest := sha256.Sum256([]byte(cacheKey))
	base, err := os.UserCacheDir()
	if err != nil {
		return "", "", fmt.Errorf("resolve user cache directory: %w", err)
	}
	return filepath.Join(base, "tk", "parse", hex.EncodeToString(digest[:])+".gz"), key, nil
}

func cacheStat(info os.FileInfo) (int64, int64) {
	ct := info.ModTime().UnixNano()
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		ct = st.Ctim.Sec*1e9 + st.Ctim.Nsec
	}
	return info.ModTime().UnixNano(), ct
}

func statMatches(a, b os.FileInfo) bool {
	am, ac := cacheStat(a)
	bm, bc := cacheStat(b)
	return a.Size() == b.Size() && am == bm && ac == bc && os.SameFile(a, b)
}

func loadCachedTask(path string, tolerateUnknownStatus bool) (task, error) {
	pre, err := os.Stat(path)
	if err != nil {
		return task{}, fmt.Errorf("stat ticket %s: %w", path, err)
	}
	if parseCacheAfterStat != nil {
		parseCacheAfterStat(path)
	}
	cachePath, key, cacheErr := parseCachePath(path)
	if cacheErr == nil && os.Getenv("TK_NO_PARSE_CACHE") == "" {
		if t, ok := readParseCache(cachePath, key, path, pre); ok {
			return t, nil
		}
	}
	if parseCacheBeforeRead != nil {
		parseCacheBeforeRead(path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return task{}, fmt.Errorf("read ticket %s: %w", path, err)
	}
	post, err := os.Stat(path)
	if err != nil {
		return task{}, fmt.Errorf("restat ticket %s: %w", path, err)
	}
	stable := statMatches(pre, post)
	if !stable {
		pre = post
	}
	parsed, parseErr := parseCachedBody(path, string(body), pre, tolerateUnknownStatus)
	if parseErr != nil {
		return task{}, parseErr
	}
	if cacheErr == nil && stable && os.Getenv("TK_NO_PARSE_CACHE") == "" {
		if err := writeParseCache(cachePath, key, post, parsed); err != nil {
			return task{}, err
		}
	}
	return parsed, nil
}

func parseCachedBody(path, body string, info os.FileInfo, tolerateUnknownStatus bool) (task, error) {
	name, ok := parseTaskName(filepath.Base(path))
	if !ok {
		return task{}, fmt.Errorf("invalid task name %s", path)
	}
	created, err := time.ParseInLocation("20060102-1504", name.date+"-"+name.clock, time.Local)
	if err != nil {
		return task{}, fmt.Errorf("invalid task filename %s: %w", filepath.Base(path), err)
	}
	st, err := status(body)
	if err != nil {
		if !tolerateUnknownStatus {
			return task{}, fmt.Errorf("%s: %w", path, err)
		}
		st, _ = explicitStatus(body)
	}
	return task{path: path, slug: name.slug, status: st, prio: name.prio, diff: name.diff, title: title(body), body: body, created: created, changed: lastChange(body, info.ModTime())}, nil
}

type parseCacheFile struct {
	Version int
	Entries map[string]parseCacheEntry
}

func loadCachedTaskFromEntries(ticket string, tolerateUnknownStatus bool, entries map[string]parseCacheEntry) (task, error) {
	pre, err := os.Stat(ticket)
	if err != nil {
		return task{}, fmt.Errorf("stat ticket %s: %w", ticket, err)
	}
	if parseCacheAfterStat != nil {
		parseCacheAfterStat(ticket)
	}
	_, key, keyErr := parseCachePath(ticket)
	if keyErr == nil && os.Getenv("TK_NO_PARSE_CACHE") == "" {
		if entry, ok := entries[key]; ok {
			if cached, valid := taskFromParseCache(ticket, key, pre, entry); valid {
				return cached, nil
			}
		}
	}
	if parseCacheBeforeRead != nil {
		parseCacheBeforeRead(ticket)
	}
	body, err := os.ReadFile(ticket)
	if err != nil {
		return task{}, fmt.Errorf("read ticket %s: %w", ticket, err)
	}
	post, err := os.Stat(ticket)
	if err != nil {
		return task{}, fmt.Errorf("restat ticket %s: %w", ticket, err)
	}
	stable := statMatches(pre, post)
	if !stable {
		pre = post
	}
	parsed, err := parseCachedBody(ticket, string(body), pre, tolerateUnknownStatus)
	if err != nil {
		return task{}, err
	}
	if keyErr == nil && stable && os.Getenv("TK_NO_PARSE_CACHE") == "" && !post.ModTime().After(time.Now().Add(-2*time.Second)) {
		entries[key] = makeParseCacheEntry(key, post, parsed)
	}
	return parsed, nil
}

func readParseCache(path, key, ticket string, current os.FileInfo) (task, bool) {
	if os.Getenv("TK_NO_PARSE_CACHE") != "" {
		return task{}, false
	}
	return taskFromParseCache(ticket, key, current, readParseCacheEntries(path)[key])
}

func taskFromParseCache(ticket, key string, current os.FileInfo, entry parseCacheEntry) (task, bool) {
	if current.ModTime().After(time.Now().Add(-2*time.Second)) || entry.Version != parseCacheVersion || entry.Key != key {
		return task{}, false
	}
	mod, ct := cacheStat(current)
	if entry.Size != current.Size() || entry.ModNS != mod || entry.CTimeNS != ct || time.Duration(math.Abs(float64(entry.Written-entry.ModNS))) < 2*time.Second {
		return task{}, false
	}
	return task{path: ticket, slug: entry.Task.Slug, status: entry.Task.Status, prio: entry.Task.Prio, diff: entry.Task.Diff, title: entry.Task.Title, created: entry.Task.Created, changed: entry.Task.Changed, facts: &entry.Task.Facts}, true
}

func makeParseCacheEntry(key string, info os.FileInfo, t task) parseCacheEntry {
	mod, ct := cacheStat(info)
	return parseCacheEntry{Version: parseCacheVersion, Key: key, Size: info.Size(), ModNS: mod, CTimeNS: ct, Written: time.Now().UnixNano(), Task: cachedTask{Slug: t.slug, Status: t.status, Prio: t.prio, Diff: t.diff, Title: t.title, Created: t.created, Changed: t.changed, Facts: *factsForTask(t)}}
}

func writeParseCache(path, key string, info os.FileInfo, t task) error {
	if info.ModTime().After(time.Now().Add(-2 * time.Second)) {
		return nil
	}
	entries := readParseCacheEntries(path)
	entries[key] = makeParseCacheEntry(key, info, t)
	return writeParseCacheEntries(path, entries)
}

func readParseCacheEntries(path string) map[string]parseCacheEntry {
	entries := map[string]parseCacheEntry{}
	compressed, err := os.ReadFile(path)
	if err != nil { //nolint:silent-error-handling // Missing or unreadable optional cache data is a miss; tickets remain authoritative.
		return entries
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil { //nolint:silent-error-handling // Corrupt compressed cache data is intentionally discarded.
		return entries
	}
	data, readErr := io.ReadAll(io.LimitReader(zr, (32<<20)+1))
	closeErr := zr.Close()
	if readErr != nil { //nolint:silent-error-handling // Corrupt compressed cache data is intentionally discarded.
		return entries
	}
	if closeErr != nil { //nolint:silent-error-handling // Corrupt compressed cache data is intentionally discarded.
		return entries
	}
	if len(data) > 32<<20 {
		return entries
	}
	var file parseCacheFile
	if err := json.Unmarshal(data, &file); err != nil { //nolint:silent-error-handling // Invalid cache JSON is a cache miss.
		return entries
	}
	if file.Version != parseCacheVersion || file.Entries == nil {
		return entries
	}
	return file.Entries
}

func writeParseCacheEntries(path string, entries map[string]parseCacheEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create parse cache directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("set parse cache directory mode: %w", err)
	}
	data, err := json.Marshal(parseCacheFile{Version: parseCacheVersion, Entries: entries})
	if err != nil {
		return fmt.Errorf("encode parse cache: %w", err)
	}
	var compressed bytes.Buffer
	zw, err := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("compress parse cache: %w", err)
	}
	if _, err = zw.Write(data); err != nil {
		return fmt.Errorf("compress parse cache: %w", err)
	}
	if err = zw.Close(); err != nil {
		return fmt.Errorf("finish parse cache compression: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".parse-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary parse cache: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set parse cache file mode: %w", err)
	}
	if _, err := tmp.Write(compressed.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary parse cache: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary parse cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary parse cache: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace parse cache: %w", err)
	}
	return nil
}
