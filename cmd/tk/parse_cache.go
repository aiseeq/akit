package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const parseCacheVersion = 3
const parseCacheRacyWindow = 2 * time.Second

var parseCacheAfterStat func(string)
var parseCacheBeforeRead func(string)
var parseCacheBeforeFileRead func(string)
var parseCacheBeforeFileWrite func(string)
var parseCacheBeforePath func()
var parseCacheBeforeLinkedFind func()
var parseCacheNow = time.Now
var parseCacheExecutableInfo = executableFileInfo
var parseCacheCTime = func(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ctim.Sec*1e9 + st.Ctim.Nsec
	}
	return info.ModTime().UnixNano()
}

type parseCacheEntry struct {
	Size    int64      `json:"size"`
	ModNS   int64      `json:"mod_ns"`
	CTimeNS int64      `json:"ctime_ns"`
	FileID  string     `json:"file_id"`
	Written int64      `json:"written_ns"`
	Task    cachedTask `json:"task"`
}

type cachedTask struct {
	Slug, Status, Prio, Diff, Title string
	CreatedStamp, ChangedStamp      string
	Facts                           taskFacts
}

type parseCacheFile struct {
	Version int
	Binary  string
	Entries map[string]parseCacheEntry
}

func executableFileInfo() (os.FileInfo, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable for ticket cache: %w", err)
	}
	info, err := os.Stat(exe)
	if err != nil {
		return nil, fmt.Errorf("stat executable for ticket cache: %w", err)
	}
	return info, nil
}

func cacheIdentity(info os.FileInfo) string {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d:%d:%d", st.Dev, st.Ino, info.Size(), st.Ctim.Sec*1e9+st.Ctim.Nsec)
	}
	return fmt.Sprintf("%s:%d:%o", info.Name(), info.Size(), info.Mode().Perm())
}

func parseCacheBinaryIdentity() (string, error) {
	info, err := parseCacheExecutableInfo()
	if err != nil {
		return "", err
	}
	return cacheIdentity(info), nil
}

func parseCachePath(ticket string) (string, string, error) {
	return parseCachePathFor(filepath.Dir(ticket), ticket)
}

func parseCachePathFor(dir, ticket string) (string, string, error) {
	if parseCacheBeforePath != nil {
		parseCacheBeforePath()
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", "", fmt.Errorf("resolve ticket directory for cache: %w", err)
	}
	absTicket, err := filepath.Abs(ticket)
	if err != nil {
		return "", "", fmt.Errorf("resolve ticket path for cache: %w", err)
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", "", fmt.Errorf("resolve user cache directory: %w", err)
	}
	digest := sha256.Sum256([]byte(absDir))
	path := filepath.Join(base, "tk", "parse", hex.EncodeToString(digest[:])+".gz")
	return path, absTicket, nil
}

func cacheStat(info os.FileInfo) (int64, int64, string) {
	mod := info.ModTime().UnixNano()
	ctime := parseCacheCTime(info)
	fileID := ""
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		fileID = fmt.Sprintf("%d:%d", st.Dev, st.Ino)
	}
	return mod, ctime, fileID
}

func statMatches(a, b os.FileInfo) bool {
	am, ac, aid := cacheStat(a)
	bm, bc, bid := cacheStat(b)
	return a.Size() == b.Size() && am == bm && ac == bc && aid == bid && os.SameFile(a, b)
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

func loadCachedTaskFromEntries(ticket string, tolerateUnknownStatus bool, entries map[string]parseCacheEntry) (task, error) {
	pre, err := os.Stat(ticket)
	if err != nil {
		return task{}, fmt.Errorf("stat ticket %s: %w", ticket, err)
	}
	if parseCacheAfterStat != nil {
		parseCacheAfterStat(ticket)
	}
	key, err := filepath.Abs(ticket)
	if err != nil {
		return task{}, fmt.Errorf("resolve ticket path %s: %w", ticket, err)
	}
	if entry, ok := entries[key]; ok {
		if cached, valid := taskFromParseCache(ticket, pre, entry); valid {
			return cached, nil
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
	if stable {
		if cacheEntry, ok := makeParseCacheEntry(post, parsed); ok {
			entries[key] = cacheEntry
		}
	}
	return parsed, nil
}

func taskFromParseCache(ticket string, current os.FileInfo, entry parseCacheEntry) (task, bool) {
	if cacheTooFresh(current, entry.Written) {
		return task{}, false
	}
	mod, ctime, fileID := cacheStat(current)
	_ = ctime
	if entry.Size != current.Size() || entry.ModNS != mod || entry.CTimeNS != ctime || entry.FileID != fileID {
		return task{}, false
	}
	created, err := time.ParseInLocation("20060102-1504", entry.Task.CreatedStamp, time.Local)
	if err != nil { //nolint:error-masked-as-false-bool // A malformed cached stamp is a cache miss; ticket contents remain authoritative.
		return task{}, false
	}
	changed := current.ModTime()
	if entry.Task.ChangedStamp != "" {
		changed, err = time.ParseInLocation("2006-01-02 15:04", entry.Task.ChangedStamp, time.Local)
		if err != nil { //nolint:error-masked-as-false-bool // A malformed cached stamp is a cache miss; ticket contents remain authoritative.
			return task{}, false
		}
	}
	return task{path: ticket, slug: entry.Task.Slug, status: entry.Task.Status, prio: entry.Task.Prio, diff: entry.Task.Diff, title: entry.Task.Title, created: created, changed: changed, facts: &entry.Task.Facts}, true
}

func cacheTooFresh(info os.FileInfo, written int64) bool {
	mod := info.ModTime()
	now := parseCacheNow()
	if mod.After(now.Add(-parseCacheRacyWindow)) {
		return true
	}
	delta := written - mod.UnixNano()
	if delta < 0 {
		delta = -delta
	}
	return time.Duration(delta) < parseCacheRacyWindow
}

func lastChangeStamp(body string) string {
	inJournal := false
	last := ""
	for line := range strings.SplitSeq(body, "\n") {
		if line == "## Журнал" {
			inJournal = true
			continue
		}
		if inJournal && strings.HasPrefix(line, "## ") {
			break
		}
		if !inJournal || !strings.HasPrefix(line, "- ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		stamp := fields[1] + " " + fields[2]
		if _, err := time.Parse("2006-01-02 15:04", stamp); err == nil { //nolint:error-masking // Malformed journal lines are ignored, matching the uncached parser.
			last = stamp
		}
	}
	return last
}

func makeParseCacheEntry(info os.FileInfo, t task) (parseCacheEntry, bool) {
	if cacheTooFresh(info, parseCacheNow().UnixNano()) {
		return parseCacheEntry{}, false
	}
	name, ok := parseTaskName(filepath.Base(t.path))
	if !ok {
		return parseCacheEntry{}, false
	}
	mod, ctime, fileID := cacheStat(info)
	return parseCacheEntry{
		Size: info.Size(), ModNS: mod, CTimeNS: ctime, FileID: fileID, Written: parseCacheNow().UnixNano(),
		Task: cachedTask{Slug: t.slug, Status: t.status, Prio: t.prio, Diff: t.diff, Title: t.title,
			CreatedStamp: name.date + "-" + name.clock, ChangedStamp: lastChangeStamp(t.body), Facts: *factsForTask(t)},
	}, true
}

func readParseCacheEntries(path, binary string) (map[string]parseCacheEntry, []byte) {
	entries := map[string]parseCacheEntry{}
	if parseCacheBeforeFileRead != nil {
		parseCacheBeforeFileRead(path)
	}
	compressed, err := os.ReadFile(path)
	if err != nil { //nolint:silent-error-handling // A missing or unreadable optional cache is an empty cache.
		return entries, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil { //nolint:silent-error-handling // Invalid gzip data is an empty cache and will be replaced after parsing.
		return entries, compressed
	}
	data, readErr := io.ReadAll(io.LimitReader(zr, (32<<20)+1))
	closeErr := zr.Close()
	if readErr != nil || closeErr != nil || len(data) > 32<<20 { //nolint:masked-error-in-or-condition // Invalid or oversized cache data is an empty cache.
		return entries, compressed
	}
	var file parseCacheFile
	if err := json.Unmarshal(data, &file); err != nil { //nolint:silent-error-handling // Invalid cache JSON is an empty cache.
		return entries, compressed
	}
	if file.Version != parseCacheVersion || file.Binary != binary || file.Entries == nil {
		return entries, compressed
	}
	return file.Entries, compressed
}

func writeParseCacheEntries(path, binary string, entries map[string]parseCacheEntry, previous []byte) error {
	data, err := json.Marshal(parseCacheFile{Version: parseCacheVersion, Binary: binary, Entries: entries})
	if err != nil {
		return fmt.Errorf("encode parse cache: %w", err)
	}
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err = zw.Write(data); err != nil {
		return fmt.Errorf("compress parse cache: %w", err)
	}
	if err = zw.Close(); err != nil {
		return fmt.Errorf("finish parse cache compression: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create parse cache directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("set parse cache directory mode: %w", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("set parse cache file mode: %w", err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("stat parse cache file: %w", statErr)
	}
	if bytes.Equal(previous, compressed.Bytes()) {
		return nil
	}
	if parseCacheBeforeFileWrite != nil {
		parseCacheBeforeFileWrite(path)
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
