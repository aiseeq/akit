package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"unicode/utf8"
)

const (
	runCacheVersion        = 10
	transcriptCacheVersion = 6
	cacheDirMode           = 0700
	// The index keeps the full per-report bloom, so a rejected report needs no file of its own in the cache.
	runBloomBytes      = 768
	runIndexBloomBytes = runBloomBytes
	// Two probes reduce false positives for long reports at the fixed index size.
	runBloomHashes        = 2
	runIndexKeyBytes      = 32
	runIndexMetadataBytes = 52
	runIndexRecordBytes   = runIndexKeyBytes + runIndexMetadataBytes + runIndexBloomBytes
	bloomFnvPrime         = uint64(1099511628211)
)

var runIndexMagic = [8]byte{'P', 'R', 'I', 'D', 'X', '0', '1', '6'}

// runIndexName is the report index file. Its absence marks a cache that may still hold files of earlier layouts:
// the call that writes the index first removes them (removeOldLayoutCacheFiles).
const runIndexName = "runs-index-v14.bin"

var openPriorSource = os.Open

type transcriptLinesCache struct {
	Version      int            `json:"version"`
	Path         string         `json:"path"`
	Size         int64          `json:"size"`
	ModTime      int64          `json:"mtime"`
	Device       uint64         `json:"device"`
	Inode        uint64         `json:"inode"`
	ScannedBytes int64          `json:"scanned_bytes"`
	PrefixHash   []byte         `json:"prefix_hash"`
	LineCount    int            `json:"line_count"`
	Lines        map[string]int `json:"lines"`
}

type replyPrewarmCache struct {
	Version      int    `json:"version"`
	Path         string `json:"path"`
	Size         int64  `json:"size"`
	ModTime      int64  `json:"mtime"`
	Device       uint64 `json:"device"`
	Inode        uint64 `json:"inode"`
	ScannedBytes int64  `json:"scanned_bytes"`
	PrefixHash   []byte `json:"prefix_hash"`
}

// runIndexManifest is the report index. A loaded index keeps the file's records as they are, sorted by path
// hash, and decodes only the entry a report looks up; an index being built keeps its entries in Files.
type runIndexManifest struct {
	Version int                      `json:"version"`
	Files   map[string]runIndexEntry `json:"files"`
	records []byte
	missing bool // no index file existed
}

func (index runIndexManifest) count() int {
	return len(index.Files) + len(index.records)/runIndexRecordBytes
}

// lookup returns the current entry of path. A record whose bloom fails its checksum counts as absent, so the
// report is read and its entry written anew.
func (index runIndexManifest) lookup(path string) (runIndexEntry, bool) {
	if entry, ok := index.Files[runIndexKey(path)]; ok {
		return entry, true
	}
	key := sha256.Sum256([]byte(path))
	n := len(index.records) / runIndexRecordBytes
	i := sort.Search(n, func(i int) bool {
		return bytes.Compare(index.records[i*runIndexRecordBytes:i*runIndexRecordBytes+runIndexKeyBytes], key[:]) >= 0
	})
	if i == n {
		return runIndexEntry{}, false
	}
	record := index.records[i*runIndexRecordBytes : (i+1)*runIndexRecordBytes]
	if !bytes.Equal(record[:runIndexKeyBytes], key[:]) {
		return runIndexEntry{}, false
	}
	metadata := record[runIndexKeyBytes : runIndexKeyBytes+runIndexMetadataBytes]
	bloom := record[runIndexKeyBytes+runIndexMetadataBytes:]
	entry := runIndexEntry{
		Size: int64(binary.LittleEndian.Uint64(metadata[:8])), ModTime: int64(binary.LittleEndian.Uint64(metadata[8:16])),
		Device: binary.LittleEndian.Uint64(metadata[16:24]), Inode: binary.LittleEndian.Uint64(metadata[24:32]),
		CRC: binary.LittleEndian.Uint32(metadata[32:36]), Bloom: bloom[:len(bloom):len(bloom)],
	}
	copy(entry.ContentHash[:], metadata[36:52])
	if entry.Size < 0 || crc32.ChecksumIEEE(bloom) != entry.CRC {
		return runIndexEntry{}, false
	}
	return entry, true
}

type runIndexEntry struct {
	Size        int64    `json:"size"`
	ModTime     int64    `json:"mtime"`
	Device      uint64   `json:"device"`
	Inode       uint64   `json:"inode"`
	ContentHash [16]byte `json:"content_hash"`
	Bloom       []byte   `json:"bloom"`
	CRC         uint32   `json:"crc"`
}

func fileIdentity(info os.FileInfo) (device, inode uint64, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(stat.Dev), stat.Ino, true
}

// filePrefixHash fingerprints the cache prefix using five 4 KiB windows at the start, quarter points, and end.
func filePrefixHash(source io.ReaderAt, size int64) ([]byte, error) {
	if size < 0 {
		return nil, fmt.Errorf("размер префикса файла отрицательный")
	}
	h := sha256.New()
	if size == 0 {
		return h.Sum(nil), nil
	}
	const window = int64(4096)
	maxStart := max(int64(0), size-window)
	starts := []int64{0, size / 4, size / 2, (size * 3) / 4, maxStart}
	seen := make(map[int64]struct{}, len(starts))
	for _, start := range starts {
		start = min(start, maxStart)
		if _, ok := seen[start]; ok {
			continue
		}
		seen[start] = struct{}{}
		length := min(window, size-start)
		var meta [16]byte
		binary.LittleEndian.PutUint64(meta[:8], uint64(start))
		binary.LittleEndian.PutUint64(meta[8:], uint64(length))
		_, _ = h.Write(meta[:])
		buf := make([]byte, length)
		n, err := source.ReadAt(buf, start)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("окно префикса файла не прочиталось: %w", err)
		}
		if int64(n) != length {
			return nil, io.ErrUnexpectedEOF
		}
		_, _ = h.Write(buf)
	}
	return h.Sum(nil), nil
}

func sameFileIdentity(info os.FileInfo, device, inode uint64) bool {
	actualDevice, actualInode, ok := fileIdentity(info)
	return ok && actualDevice == device && actualInode == inode
}

func cachePath(name string) string {
	root := os.Getenv("AKIT_PRIOR_CACHE_DIR")
	if root == "" {
		var err error
		root, err = os.UserCacheDir()
		if err != nil {
			logCacheError(err)
			return ""
		}
		root = filepath.Join(root, "akit", "prior")
	}
	return filepath.Join(root, name)
}

func logCacheError(err error) {
	if err != nil {
		slog.Debug("prior cache unavailable", "error", err)
	}
}

func saveJSONCache(path, description string, value any) error {
	if path == "" {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%s не сериализовался: %w", description, err)
	}
	return saveCacheBytes(path, data)
}

func transcriptLinesCachePath(path string) string {
	hash := sha256.Sum256([]byte(path))
	return cachePath("transcript-" + hex.EncodeToString(hash[:]) + ".json")
}

func loadTranscriptLines(path string) (transcriptLinesCache, bool, error) {
	cacheFile := transcriptLinesCachePath(path)
	if cacheFile == "" {
		return transcriptLinesCache{}, false, nil
	}
	data, err := os.ReadFile(cacheFile)
	if os.IsNotExist(err) {
		return transcriptLinesCache{}, false, nil
	}
	if err != nil {
		return transcriptLinesCache{}, false, fmt.Errorf("индекс транскрипта не прочитался: %w", err)
	}
	var cache transcriptLinesCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return transcriptLinesCache{}, false, fmt.Errorf("индекс транскрипта повреждён: %w", err)
	}
	if cache.Version != transcriptCacheVersion || cache.Path != path || cache.Lines == nil ||
		cache.Size < 0 || cache.ScannedBytes < 0 || cache.ScannedBytes > cache.Size || cache.LineCount < 0 ||
		len(cache.PrefixHash) != sha256.Size || cache.Device == 0 || cache.Inode == 0 {
		return transcriptLinesCache{}, false, fmt.Errorf("версия или данные индекса транскрипта не подходят")
	}
	return cache, true, nil
}

func saveTranscriptLines(cache transcriptLinesCache) error {
	cacheFile := transcriptLinesCachePath(cache.Path)
	if cacheFile == "" {
		return nil
	}
	data, err := json.Marshal(cache)
	if err != nil {
		return fmt.Errorf("индекс транскрипта не сериализовался: %w", err)
	}
	return saveCacheBytes(cacheFile, data)
}

func loadReplyPrewarmCache() (replyPrewarmCache, bool, error) {
	path := cachePath("reply-prewarm.json")
	if path == "" {
		return replyPrewarmCache{}, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return replyPrewarmCache{}, false, nil
		}
		return replyPrewarmCache{}, false, fmt.Errorf("индекс прогрева реплик не прочитался: %w", err)
	}
	var cache replyPrewarmCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return replyPrewarmCache{}, false, fmt.Errorf("индекс прогрева реплик повреждён: %w", err)
	}
	if cache.Version != 2 || cache.Size < 0 || cache.ScannedBytes < 0 || cache.ScannedBytes > cache.Size ||
		len(cache.PrefixHash) != sha256.Size || cache.Device == 0 || cache.Inode == 0 {
		return replyPrewarmCache{}, false, fmt.Errorf("версия или данные индекса прогрева реплик не подходят")
	}
	return cache, true, nil
}

func saveReplyPrewarmCache(cache replyPrewarmCache) error {
	cache.Version = 2
	return saveJSONCache(cachePath("reply-prewarm.json"), "индекс прогрева реплик", cache)
}

func canWritePriorCache() bool {
	root := filepath.Dir(cachePath(".write-probe"))
	if root == "." || root == "" {
		return false
	}
	if err := os.MkdirAll(root, cacheDirMode); err != nil {
		logCacheError(err)
		return false
	}
	f, err := os.CreateTemp(root, ".prior-probe-*.tmp")
	if err != nil {
		logCacheError(err)
		return false
	}
	name := f.Name()
	_, writeErr := f.Write(make([]byte, 64<<10))
	syncErr := f.Sync()
	closeErr := f.Close()
	removeErr := os.Remove(name)
	if err := errors.Join(writeErr, syncErr, closeErr, removeErr); err != nil {
		logCacheError(err)
		return false
	}
	return true
}

func saveCacheBytes(path string, data []byte) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), cacheDirMode); err != nil {
		return fmt.Errorf("каталог кэша не создался: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".prior-cache-*.tmp")
	if err != nil {
		return fmt.Errorf("временный кэш не создался: %w", err)
	}
	name := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			removeErr := os.Remove(name)
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("временный кэш не удалился: %w", removeErr))
			}
		}
	}()
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		if err = os.Rename(name, path); err == nil {
			renamed = true
		}
	}
	if err != nil {
		return fmt.Errorf("кэш не сохранился: %w", err)
	}
	return nil
}

// readReplies reads the reply journal and returns a snapshot that did not change while it was read. The
// journal is read from its file every time: a full read costs less than unpacking a compressed copy of it.
func readReplies(path string) ([]byte, error) {
	var lastData []byte
	for attempt := 1; attempt <= 3; attempt++ {
		source, err := openPriorSource(path)
		if err != nil {
			return nil, err
		}
		info, err := source.Stat()
		if err != nil {
			return nil, errors.Join(fmt.Errorf("журнал ответов не прочитался: %w", err), source.Close())
		}
		_, _, identityOK := fileIdentity(info)
		if !identityOK || !info.Mode().IsRegular() {
			data, readErr := io.ReadAll(source)
			closeErr := source.Close()
			if readErr != nil {
				return nil, fmt.Errorf("журнал ответов не прочитался: %w", readErr)
			}
			if closeErr != nil {
				return nil, fmt.Errorf("журнал ответов не закрылся: %w", closeErr)
			}
			return data, nil
		}
		beforeHash, err := filePrefixHash(source, info.Size())
		if err != nil {
			return nil, errors.Join(fmt.Errorf("журнал ответов не прочитался: %w", err), source.Close())
		}
		data := make([]byte, 0, info.Size()+bytes.MinRead)
		buf := bytes.NewBuffer(data)
		if _, err := buf.ReadFrom(io.NewSectionReader(source, 0, math.MaxInt64)); err != nil {
			return nil, errors.Join(fmt.Errorf("журнал ответов не прочитался: %w", err), source.Close())
		}
		data = buf.Bytes()
		lastData = data // each attempt reads into a buffer of its own
		if replyReadAttemptHook != nil {
			replyReadAttemptHook(path, attempt)
		}
		stable, stableErr := replySourceStable(path, source, info, data, beforeHash)
		closeErr := source.Close()
		if closeErr != nil {
			return nil, fmt.Errorf("журнал ответов не закрылся: %w", closeErr)
		}
		if stableErr != nil {
			return nil, stableErr
		}
		if !stable {
			continue
		}
		return data, nil
	}
	return lastData, nil
}

var replyReadAttemptHook func(string, int)

func replySourceStable(path string, source *os.File, info os.FileInfo, data, beforeHash []byte) (bool, error) {
	after, statErr := source.Stat()
	pathInfo, pathErr := os.Stat(path)
	afterHash, hashErr := filePrefixHash(source, info.Size())
	if err := errors.Join(statErr, pathErr, hashErr); err != nil {
		return false, err
	}
	return os.SameFile(info, after) && os.SameFile(info, pathInfo) && after.Size() == info.Size() &&
		after.ModTime().Equal(info.ModTime()) && int64(len(data)) == info.Size() && bytes.Equal(beforeHash, afterHash), nil
}

func runIndexKey(path string) string {
	hash := sha256.Sum256([]byte(path))
	return hex.EncodeToString(hash[:])
}

// loadRunIndex reads the index in one piece and checks only its header and length; entries are decoded on lookup.
// The length follows from the number of records, so the index has no size limit that a growing report tree
// could reach.
func loadRunIndex() (runIndexManifest, error) {
	index := runIndexManifest{Version: runCacheVersion}
	path := cachePath(runIndexName)
	if path == "" {
		return index, nil
	}
	data, err := readFileRaw(path)
	if errors.Is(err, fs.ErrNotExist) {
		index.missing = true
		return index, nil
	}
	if err != nil {
		return index, fmt.Errorf("отчётный индекс не прочитался: %w", err)
	}
	if len(data) < 16 {
		return index, fmt.Errorf("заголовок отчётного индекса не прочитался: %w", io.ErrUnexpectedEOF)
	}
	if !bytes.Equal(data[:8], runIndexMagic[:]) || binary.LittleEndian.Uint32(data[8:12]) != runCacheVersion {
		return index, fmt.Errorf("версия отчётного индекса не подходит")
	}
	count := uint64(binary.LittleEndian.Uint32(data[12:16]))
	if uint64(len(data)-16) != count*runIndexRecordBytes {
		return index, fmt.Errorf("размер отчётного индекса не совпадает с числом записей")
	}
	index.records = data[16:]
	return index, nil
}

func saveRunIndex(index runIndexManifest) error {
	path := cachePath(runIndexName)
	if path == "" {
		return nil
	}
	keys := slices.Sorted(maps.Keys(index.Files))
	data := make([]byte, 16, 16+len(keys)*runIndexRecordBytes)
	copy(data[:8], runIndexMagic[:])
	binary.LittleEndian.PutUint32(data[8:12], runCacheVersion)
	binary.LittleEndian.PutUint32(data[12:16], uint32(len(keys)))
	for _, key := range keys {
		entry := index.Files[key]
		pathHash, err := hex.DecodeString(key)
		if err != nil || len(pathHash) != runIndexKeyBytes || len(entry.Bloom) != runIndexBloomBytes {
			return fmt.Errorf("запись отчётного индекса недопустима")
		}
		data = append(data, pathHash...)
		metadata := make([]byte, runIndexMetadataBytes)
		binary.LittleEndian.PutUint64(metadata[:8], uint64(entry.Size))
		binary.LittleEndian.PutUint64(metadata[8:16], uint64(entry.ModTime))
		binary.LittleEndian.PutUint64(metadata[16:24], entry.Device)
		binary.LittleEndian.PutUint64(metadata[24:32], entry.Inode)
		binary.LittleEndian.PutUint32(metadata[32:36], entry.CRC)
		copy(metadata[36:52], entry.ContentHash[:])
		data = append(data, metadata...)
		data = append(data, entry.Bloom...)
	}
	return saveCacheBytes(path, data)
}

func matchRunFilesCached(q query, files []string) []runFileResult {
	index, indexErr := loadRunIndex()
	logCacheError(indexErr)
	indexChanged := atomic.Bool{}
	if indexErr != nil || index.count() != len(files) {
		indexChanged.Store(true)
	}
	results := make([]runFileResult, len(files))
	nextEntries := make([]runIndexEntry, len(files))
	contentReads := &runContentReads{entries: make(map[[16]byte]*runContentRead)}
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(len(files), runtime.GOMAXPROCS(0), 4) {
		workers.Go(func() {
			for i := range jobs {
				line, entry, hasEntry, err := matchRunFileCached(q, files[i], index, &indexChanged, contentReads)
				results[i] = runFileResult{line: line, err: err}
				if hasEntry {
					nextEntries[i] = entry
				}
			}
		})
	}
	for i := range files {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	if indexChanged.Load() {
		next := runIndexManifest{Version: runCacheVersion, Files: make(map[string]runIndexEntry, len(files))}
		for i, entry := range nextEntries {
			if len(entry.Bloom) == runIndexBloomBytes {
				next.Files[runIndexKey(files[i])] = entry
			}
		}
		saveErr := saveRunIndex(next)
		logCacheError(saveErr)
		if saveErr == nil && index.missing {
			removeOldLayoutCacheFiles()
		}
	}
	return results
}

// oldLayoutCacheFile matches the cache files of earlier prior layouts: per-report entries, compressed reply
// journals and report indexes before runIndexName.
var oldLayoutCacheFile = regexp.MustCompile(`^(?:run-[0-9a-f]{64}\.bin|replies-[0-9a-f]{64}\.bin|runs-index-v(?:[2-9]|1[0-3])\.(?:bin|json)|runs-v1\.gob|runs-data-v1\.bin)$`)

// removeOldLayoutCacheFiles deletes files of earlier layouts from the prior cache directory. It runs once, when the
// current index is written for the first time, so an older build still in use next to this one rebuilds its
// files at most once instead of on every call. A file that is gone already is not an error.
func removeOldLayoutCacheFiles() {
	dir := filepath.Dir(cachePath(runIndexName))
	entries, errs := readDirEntries(dir)
	for _, err := range errs {
		logCacheError(err)
	}
	for _, entry := range entries {
		if entry.dir || !oldLayoutCacheFile.MatchString(entry.name) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logCacheError(err)
		}
	}
}

type runContentRead struct {
	done chan struct{}
	line string
	err  error
}

// runContentReads shares one cached report scan per content fingerprint within a query.
type runContentReads struct {
	mu      sync.Mutex
	entries map[[16]byte]*runContentRead
}

// errRunSourceChanged is returned by a load whose report changed while it was read: its result belongs to no
// fingerprint, so it is not shared and every other report with that fingerprint loads its own.
var errRunSourceChanged = errors.New("отчёт изменился во время чтения")

func (reads *runContentReads) get(hash [16]byte, load func() (string, error)) (string, error) {
	reads.mu.Lock()
	if cached := reads.entries[hash]; cached != nil {
		reads.mu.Unlock()
		<-cached.done
		if errors.Is(cached.err, errRunSourceChanged) {
			return load()
		}
		return cached.line, cached.err
	}
	cached := &runContentRead{done: make(chan struct{})}
	reads.entries[hash] = cached
	reads.mu.Unlock()

	cached.line, cached.err = load()
	if errors.Is(cached.err, errRunSourceChanged) {
		reads.mu.Lock()
		delete(reads.entries, hash)
		reads.mu.Unlock()
	}
	close(cached.done)
	return cached.line, cached.err
}

// runSource is a report opened by a raw descriptor. Regular files gain nothing from the runtime poller, and
// os.Open spends five more system calls per file on it; thousands of reports are opened per query.
type runSource struct {
	fd   int
	path string
	stat syscall.Stat_t
}

var openRunSource = func(path string) (int, error) {
	fd, err := openRaw(path, 0)
	if err != nil {
		return -1, pathError("open", path, err)
	}
	return fd, nil
}

func (s *runSource) fstat(stat *syscall.Stat_t) error {
	for {
		err := syscall.Fstat(s.fd, stat)
		if err != syscall.EINTR {
			if err != nil {
				return pathError("stat", s.path, err)
			}
			return nil
		}
	}
}

// unchanged reports whether the open report still has the identity, size and mtime seen before reading it.
func (s *runSource) unchanged() bool {
	var current syscall.Stat_t
	return s.fstat(&current) == nil && current.Dev == s.stat.Dev && current.Ino == s.stat.Ino &&
		current.Size == s.stat.Size && current.Mtim == s.stat.Mtim
}

func (s *runSource) size() int64 { return s.stat.Size }

func (s *runSource) modTime() int64 { return s.stat.Mtim.Sec*1e9 + s.stat.Mtim.Nsec }

// readAll reads the report from its start into buf until end of file, or until stop bytes when stop is not
// negative: callers that stop early check afterwards that the report has not changed.
func (s *runSource) readAll(buf []byte, stop int64) ([]byte, error) {
	data := buf[:0]
	if want := int(max(s.size(), 0)) + 512; cap(data) < want {
		data = make([]byte, 0, want)
	}
	for stop < 0 || int64(len(data)) < stop {
		if len(data) == cap(data) {
			data = append(data, 0)[:len(data)]
		}
		end := cap(data)
		if stop >= 0 && int64(end) > stop {
			end = int(stop)
		}
		n, err := syscall.Pread(s.fd, data[len(data):end], int64(len(data)))
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return nil, pathError("read", s.path, err)
		}
		if n == 0 {
			break
		}
		data = data[:len(data)+n]
	}
	return data, nil
}

// runBufferPool keeps report read buffers between reports; it holds only *[]byte.
type runBufferPool struct{ pool sync.Pool }

func (p *runBufferPool) get() *[]byte {
	if buf, ok := p.pool.Get().(*[]byte); ok {
		return buf
	}
	return new([]byte) // an empty pool
}

func (p *runBufferPool) put(buf *[]byte) { p.pool.Put(buf) }

var runBuffers runBufferPool

func matchRunFileCached(q query, path string, index runIndexManifest, changed *atomic.Bool, contentReads *runContentReads) (string, runIndexEntry, bool, error) {
	indexed, rejected, statErr := rejectedByIndex(q, path, index)
	if rejected {
		return "", indexed, true, nil
	}
	// A report the index could not settle is opened and read; when its stat failed, the open below meets the same
	// failure and reports it in place of the report, as an uncached read does.
	logCacheError(statErr)
	fd, err := openRunSource(path)
	if err != nil {
		return "", runIndexEntry{}, false, fmt.Errorf("отчёт не прочитался: %w", err)
	}
	defer syscall.Close(fd)
	source := &runSource{fd: fd, path: path}
	if err := source.fstat(&source.stat); err != nil {
		return "", runIndexEntry{}, false, fmt.Errorf("отчёт не прочитался: %w", err)
	}
	if source.stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return matchIrregularRunFile(q, path)
	}
	if line, entry, hit := cachedRunFileHit(q, path, index, changed, source, contentReads); hit {
		return line, entry, true, nil
	}
	return readRunSource(q, source, changed)
}

// rejectedByIndex settles a report without opening it: its index entry is current, its bloom rules the query out
// and the report is still readable. Then it returns the entry and true. Otherwise the report is not rejected:
// false, with the error of a stat or access check that failed, if one did.
func rejectedByIndex(q query, path string, index runIndexManifest) (runIndexEntry, bool, error) {
	entry, ok := index.lookup(path)
	if !ok || runFilterMayMatchBloom(q.runs, entry.Bloom) {
		return runIndexEntry{}, false, nil
	}
	source := &runSource{path: path}
	if err := retryEINTR(func() error { return syscall.Stat(path, &source.stat) }); err != nil {
		return runIndexEntry{}, false, pathError("stat", path, err)
	}
	if source.stat.Mode&syscall.S_IFMT != syscall.S_IFREG || !runIndexMetadataMatches(entry, source) {
		return runIndexEntry{}, false, nil
	}
	if err := retryEINTR(func() error { return syscall.Access(path, unixReadOK) }); err != nil {
		return runIndexEntry{}, false, pathError("access", path, err)
	}
	return entry, true, nil
}

// retryEINTR runs call until it is not interrupted by a signal.
func retryEINTR(call func() error) error {
	for {
		if err := call(); err != syscall.EINTR {
			return err
		}
	}
}

const unixReadOK = 0x4 // R_OK of access(2)

// matchIrregularRunFile reads a report that is not a regular file through the ordinary file API, uncached.
func matchIrregularRunFile(q query, path string) (string, runIndexEntry, bool, error) {
	source, err := openPriorSource(path)
	if err != nil {
		return "", runIndexEntry{}, false, fmt.Errorf("отчёт не прочитался: %w", err)
	}
	defer source.Close()
	data, readErr := io.ReadAll(source)
	if readErr != nil {
		return "", runIndexEntry{}, false, fmt.Errorf("отчёт не прочитался: %w", readErr)
	}
	return firstMatchDataFiltered(q, data, q.runs), runIndexEntry{}, false, nil
}

func cachedRunFileHit(q query, path string, index runIndexManifest, changed *atomic.Bool, source *runSource, contentReads *runContentReads) (string, runIndexEntry, bool) {
	entry, ok := index.lookup(path)
	if !ok || !runIndexMetadataMatches(entry, source) {
		changed.Store(true)
		return "", runIndexEntry{}, false
	}
	line := ""
	if runFilterMayMatchBloom(q.runs, entry.Bloom) {
		var readErr error
		line, readErr = contentReads.get(entry.ContentHash, func() (string, error) {
			found, err := readRunSourceMatch(q, source)
			if err == nil && !source.unchanged() {
				return "", errRunSourceChanged
			}
			return found, err
		})
		if errors.Is(readErr, errRunSourceChanged) {
			return "", runIndexEntry{}, false
		}
		if readErr != nil {
			logCacheError(readErr)
			return "", runIndexEntry{}, false
		}
	}
	// The source is checked again before the indexed result is used.
	if source.unchanged() {
		return line, entry, true
	}
	return "", runIndexEntry{}, false
}

func readRunSourceMatch(q query, source *runSource) (string, error) {
	if runEntryReadHook != nil {
		runEntryReadHook(source.path)
	}
	buf := runBuffers.get()
	defer runBuffers.put(buf)
	data, err := source.readAll((*buf)[:0], source.size())
	if err != nil {
		return "", err
	}
	*buf = data[:0]
	return firstMatchDataFiltered(q, data, q.runs), nil
}

func readRunSource(q query, source *runSource, changed *atomic.Bool) (string, runIndexEntry, bool, error) {
	changed.Store(true)
	buf := runBuffers.get()
	defer runBuffers.put(buf)
	data, err := source.readAll((*buf)[:0], -1)
	if err != nil {
		return "", runIndexEntry{}, false, fmt.Errorf("отчёт не прочитался: %w", err)
	}
	*buf = data[:0]
	if runSourceReadHook != nil {
		runSourceReadHook(source.path)
	}
	var after syscall.Stat_t
	if err := source.fstat(&after); err != nil {
		return "", runIndexEntry{}, false, fmt.Errorf("отчёт после чтения не stat-нулся: %w", err)
	}
	line := firstMatchDataFiltered(q, data, q.runs)
	if int64(len(data)) != after.Size {
		return line, runIndexEntry{}, false, nil
	}
	source.stat = after
	return line, newRunIndexEntry(source, data), true, nil
}

func runIndexMetadataMatches(entry runIndexEntry, source *runSource) bool {
	return entry.Size == source.size() && entry.ModTime == source.modTime() &&
		entry.Device == uint64(source.stat.Dev) && entry.Inode == source.stat.Ino && len(entry.Bloom) == runIndexBloomBytes && crc32.ChecksumIEEE(entry.Bloom) == entry.CRC
}

var runEntryReadHook func(string)

// runSourceReadHook runs between reading a report that is indexed anew and checking that it did not change.
var runSourceReadHook func(string)

func newRunIndexEntry(source *runSource, data []byte) runIndexEntry {
	contentHashFull := sha256.Sum256(data)
	var contentHash [16]byte
	copy(contentHash[:], contentHashFull[:len(contentHash)])
	bloom := runTextBloom(data)
	return runIndexEntry{Size: source.size(), ModTime: source.modTime(), Device: uint64(source.stat.Dev), Inode: source.stat.Ino, ContentHash: contentHash, Bloom: bloom, CRC: crc32.ChecksumIEEE(bloom)}
}

func runTextBloom(data []byte) []byte {
	return runTextBloomSize(data, runBloomBytes)
}

// runTextBloomSize records every run of three runes of each folded line. Rune trigrams separate Cyrillic
// words far better than byte trigrams: those of two-byte text are mostly pairs of letters.
func runTextBloomSize(data []byte, size int) []byte {
	bloom := make([]byte, size)
	var folded []byte
	for at := 0; at < len(data); {
		end := bytes.IndexByte(data[at:], '\n')
		if end < 0 {
			end = len(data) - at
		}
		folded = appendFold(folded[:0], data[at:at+end])
		forEachRuneTrigram(string(folded), func(gram string) { bloomAdd(bloom, gram) })
		if at+end == len(data) {
			break
		}
		at += end + 1
	}
	return bloom
}

// forEachRuneTrigram calls fn with every substring of text that holds three consecutive runes.
func forEachRuneTrigram(text string, fn func(string)) {
	var starts [3]int
	n := 0
	for i := range text {
		if n >= 3 {
			fn(text[starts[n%3]:i])
		}
		starts[n%3] = i
		n++
	}
	if n >= 3 {
		fn(text[starts[n%3]:])
	}
}

func bloomAdd(bloom []byte, gram string) {
	base := bloomHash(gram)
	bits := uint64(len(bloom) * 8)
	for i := range uint64(runBloomHashes) {
		bit := (base + i*0x9e3779b97f4a7c15) % bits
		bloom[bit/8] |= 1 << (bit % 8)
	}
}

func bloomHas(bloom []byte, gram string) bool {
	base := bloomHash(gram)
	bits := uint64(len(bloom) * 8)
	for i := range uint64(runBloomHashes) {
		bit := (base + i*0x9e3779b97f4a7c15) % bits
		if bloom[bit/8]&(1<<(bit%8)) == 0 {
			return false
		}
	}
	return true
}

func bloomHash(gram string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(gram); i++ {
		h ^= uint64(gram[i])
		h *= bloomFnvPrime
	}
	return h
}

func runFilterMayMatchBloom(filter []runNeedle, bloom []byte) bool {
	if len(filter) == 0 {
		return true
	}
	for _, needle := range filter {
		if bloomMayMatchNeedle(needle, bloom) {
			return true
		}
	}
	return false
}

func bloomMayMatchNeedle(needle runNeedle, bloom []byte) bool {
	for _, variants := range needle {
		if !bloomMayMatchComponent(variants, bloom) {
			return false
		}
	}
	return true
}

func bloomMayMatchComponent(variants []string, bloom []byte) bool {
	for _, variant := range variants {
		if utf8.RuneCountInString(variant) < 3 || bloomHasVariant(variant, bloom) {
			return true
		}
	}
	return false
}

// bloomHasVariant reports whether every rune trigram of the folded variant may be in the bloom. Each line is
// folded rune by rune, so a line that contains the variant contains all of its trigrams.
func bloomHasVariant(variant string, bloom []byte) bool {
	found := true
	forEachRuneTrigram(variant, func(gram string) {
		if found && !bloomHas(bloom, gram) {
			found = false
		}
	})
	return found
}
