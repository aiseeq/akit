package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
)

const (
	runCacheVersion        = 3
	transcriptCacheVersion = 5
	cacheDirMode           = 0700
	runBloomBytes          = 1536
	runHeaderBytes         = 24 + runBloomBytes
	runIndexKeyBytes       = 32
	runIndexRecordBytes    = runIndexKeyBytes + 20 + runBloomBytes
	bloomFnvPrime          = uint64(1099511628211)
)

var runCacheMagic = [4]byte{'P', 'R', 'V', '3'}
var replyCacheMagic = [4]byte{'R', 'P', 'L', '1'}
var runIndexMagic = [8]byte{'P', 'R', 'I', 'D', 'X', '0', '0', '7'}

type transcriptLinesCache struct {
	Version      int            `json:"version"`
	Path         string         `json:"path"`
	Size         int64          `json:"size"`
	ModTime      int64          `json:"mtime"`
	ScannedBytes int64          `json:"scanned_bytes"`
	PrefixTail   []byte         `json:"prefix_tail"`
	LineCount    int            `json:"line_count"`
	Lines        map[string]int `json:"lines"`
}

type replyPrewarmCache struct {
	Version      int    `json:"version"`
	Path         string `json:"path"`
	Size         int64  `json:"size"`
	ModTime      int64  `json:"mtime"`
	ScannedBytes int64  `json:"scanned_bytes"`
	PrefixTail   []byte `json:"prefix_tail"`
}

type runIndexManifest struct {
	Version int                      `json:"version"`
	Files   map[string]runIndexEntry `json:"files"`
}

type runIndexEntry struct {
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
	Bloom   []byte `json:"bloom"`
	CRC     uint32 `json:"crc"`
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
		len(cache.PrefixTail) != int(min(int64(64), cache.ScannedBytes)) {
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
	if cache.Version != 1 || cache.Size < 0 || cache.ScannedBytes < 0 || cache.ScannedBytes > cache.Size ||
		len(cache.PrefixTail) != int(min(int64(64), cache.ScannedBytes)) {
		return replyPrewarmCache{}, false, fmt.Errorf("версия или данные индекса прогрева реплик не подходят")
	}
	return cache, true, nil
}

func saveReplyPrewarmCache(cache replyPrewarmCache) error {
	cache.Version = 1
	return saveJSONCache(cachePath("reply-prewarm.json"), "индекс прогрева реплик", cache)
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

func readCachedReplies(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("журнал ответов не stat-нулся: %w", err)
	}
	cacheFile := replyCachePath(path)
	if cacheFile != "" {
		state, cacheErr := readReplyCache(cacheFile, info)
		logCacheError(cacheErr)
		if state.Valid {
			return state.Data, nil
		}
		if state.Size > 0 && state.Size < info.Size() && len(state.Data) > 0 && state.Data[len(state.Data)-1] == '\n' {
			data, appended, appendErr := appendReplyTail(path, info.Size(), state.Size, state.Data)
			logCacheError(appendErr)
			if appended {
				logCacheError(writeReplyCache(cacheFile, info, data))
				return data, nil
			}
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("журнал ответов не прочитался: %w", err)
	}
	if cacheFile != "" {
		logCacheError(writeReplyCache(cacheFile, info, data))
	}
	return data, nil
}

func appendReplyTail(path string, size, offset int64, prefix []byte) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("журнал для дописывания не открылся: %w", err)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, false, errors.Join(fmt.Errorf("журнал не позиционировался: %w", err), f.Close())
	}
	tail, readErr := io.ReadAll(io.LimitReader(f, size-offset+1))
	closeErr := f.Close()
	if readErr != nil {
		return nil, false, fmt.Errorf("дописанный журнал не прочитался: %w", readErr)
	}
	if closeErr != nil {
		return nil, false, fmt.Errorf("журнал не закрылся: %w", closeErr)
	}
	if int64(len(tail)) != size-offset {
		return nil, false, nil
	}
	return append(prefix, tail...), true, nil
}

func replyCachePath(path string) string {
	hash := sha256.Sum256([]byte(path))
	return cachePath("replies-" + hex.EncodeToString(hash[:]) + ".bin")
}

type replyCacheState struct {
	Data  []byte
	Size  int64
	Valid bool
}

func readReplyCache(path string, sourceInfo os.FileInfo) (replyCacheState, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return replyCacheState{}, nil
	}
	if err != nil {
		return replyCacheState{}, fmt.Errorf("кэш журнала не открылся: %w", err)
	}
	header := make([]byte, 20)
	if _, err := io.ReadFull(f, header); err != nil {
		return replyCacheState{}, errors.Join(fmt.Errorf("заголовок кэша журнала не прочитался: %w", err), f.Close())
	}
	if !bytes.Equal(header[:4], replyCacheMagic[:]) {
		return replyCacheState{}, errors.Join(fmt.Errorf("формат кэша журнала не подходит"), f.Close())
	}
	size := int64(binary.LittleEndian.Uint64(header[4:12]))
	mtime := int64(binary.LittleEndian.Uint64(header[12:20]))
	if size < 0 || size > sourceInfo.Size() || (size == sourceInfo.Size() && mtime != sourceInfo.ModTime().UnixNano()) {
		if err := f.Close(); err != nil {
			return replyCacheState{}, fmt.Errorf("кэш журнала не закрылся: %w", err)
		}
		return replyCacheState{}, nil
	}
	compressed, err := io.ReadAll(io.LimitReader(f, size+(1<<20)))
	closeErr := f.Close()
	if err != nil {
		return replyCacheState{}, fmt.Errorf("тело кэша журнала не прочиталось: %w", err)
	}
	if closeErr != nil {
		return replyCacheState{}, fmt.Errorf("кэш журнала не закрылся: %w", closeErr)
	}
	data, err := decompressRun(compressed, size)
	if err != nil || int64(len(data)) != size {
		return replyCacheState{}, fmt.Errorf("кэш журнала повреждён")
	}
	return replyCacheState{Data: data, Size: size, Valid: size == sourceInfo.Size() && mtime == sourceInfo.ModTime().UnixNano()}, nil
}

func writeReplyCache(path string, info os.FileInfo, data []byte) error {
	var compressed bytes.Buffer
	gz, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	if err != nil {
		return fmt.Errorf("компрессор журнала не создался: %w", err)
	}
	if _, err := gz.Write(data); err == nil {
		err = gz.Close()
	} else {
		err = errors.Join(err, gz.Close())
	}
	if err != nil {
		return fmt.Errorf("кэш журнала не сжался: %w", err)
	}
	header := make([]byte, 20)
	copy(header[:4], replyCacheMagic[:])
	binary.LittleEndian.PutUint64(header[4:12], uint64(info.Size()))
	binary.LittleEndian.PutUint64(header[12:20], uint64(info.ModTime().UnixNano()))
	return saveCacheBytes(path, append(header, compressed.Bytes()...))
}

func runEntryPath(path string) string {
	hash := sha256.Sum256([]byte(path))
	return cachePath("run-" + hex.EncodeToString(hash[:]) + ".bin")
}

func runIndexKey(path string) string {
	hash := sha256.Sum256([]byte(path))
	return hex.EncodeToString(hash[:])
}

func loadRunIndex() (runIndexManifest, error) {
	index := runIndexManifest{Version: runCacheVersion, Files: make(map[string]runIndexEntry)}
	path := cachePath("runs-index-v6.bin")
	if path == "" {
		return index, nil
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return index, nil
	}
	if err != nil {
		return index, fmt.Errorf("отчётный индекс не stat-нулся: %w", err)
	}
	if info.Size() > 128<<20 {
		return index, fmt.Errorf("отчётный индекс слишком велик")
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return index, nil
	}
	if err != nil {
		return index, fmt.Errorf("отчётный индекс не прочитался: %w", err)
	}
	r := bytes.NewReader(data)
	header := make([]byte, 16)
	if _, err := io.ReadFull(r, header); err != nil {
		return index, fmt.Errorf("заголовок отчётного индекса не прочитался: %w", err)
	}
	if !bytes.Equal(header[:8], runIndexMagic[:]) || binary.LittleEndian.Uint32(header[8:12]) != runCacheVersion {
		return index, fmt.Errorf("версия отчётного индекса не подходит")
	}
	count := binary.LittleEndian.Uint32(header[12:16])
	if count > 1_000_000 {
		return index, fmt.Errorf("число записей отчётного индекса недопустимо")
	}
	for range count {
		if uint64(runIndexRecordBytes) > uint64(r.Len()) {
			return index, fmt.Errorf("размер записи отчётного индекса недопустим")
		}
		pathHash := make([]byte, runIndexKeyBytes)
		metadata := make([]byte, 20)
		bloom := make([]byte, runBloomBytes)
		if _, err := io.ReadFull(r, pathHash); err != nil {
			return index, fmt.Errorf("хеш пути отчётного индекса не прочитался: %w", err)
		}
		if _, err := io.ReadFull(r, metadata); err != nil {
			return index, fmt.Errorf("метаданные отчётного индекса не прочитались: %w", err)
		}
		if _, err := io.ReadFull(r, bloom); err != nil {
			return index, fmt.Errorf("фильтр отчётного индекса не прочитался: %w", err)
		}
		entry := runIndexEntry{
			Size: int64(binary.LittleEndian.Uint64(metadata[:8])), ModTime: int64(binary.LittleEndian.Uint64(metadata[8:16])),
			CRC: binary.LittleEndian.Uint32(metadata[16:20]), Bloom: bloom,
		}
		if entry.Size < 0 || crc32.ChecksumIEEE(bloom) != entry.CRC {
			return index, fmt.Errorf("запись отчётного индекса повреждена")
		}
		key := hex.EncodeToString(pathHash)
		if _, exists := index.Files[key]; exists {
			return index, fmt.Errorf("путь отчётного индекса повторяется")
		}
		index.Files[key] = entry
	}
	if r.Len() != 0 {
		return index, fmt.Errorf("в отчётном индексе остались лишние данные")
	}
	return index, nil
}

func saveRunIndex(index runIndexManifest) error {
	path := cachePath("runs-index-v6.bin")
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
		if err != nil || len(pathHash) != runIndexKeyBytes || len(entry.Bloom) != runBloomBytes {
			return fmt.Errorf("запись отчётного индекса недопустима")
		}
		data = append(data, pathHash...)
		metadata := make([]byte, 20)
		binary.LittleEndian.PutUint64(metadata[:8], uint64(entry.Size))
		binary.LittleEndian.PutUint64(metadata[8:16], uint64(entry.ModTime))
		binary.LittleEndian.PutUint32(metadata[16:20], entry.CRC)
		data = append(data, metadata...)
		data = append(data, entry.Bloom...)
	}
	return saveCacheBytes(path, data)
}

func matchRunFilesCached(q query, files []string) []runFileResult {
	index, indexErr := loadRunIndex()
	logCacheError(indexErr)
	indexChanged := atomic.Bool{}
	if indexErr != nil || len(index.Files) != len(files) {
		indexChanged.Store(true)
	}
	results := make([]runFileResult, len(files))
	nextEntries := make([]runIndexEntry, len(files))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(len(files), runtime.GOMAXPROCS(0), 4) {
		workers.Go(func() {
			for i := range jobs {
				line, entry, hasEntry, err := matchRunFileCached(q, files[i], index, &indexChanged)
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
			if len(entry.Bloom) == runBloomBytes {
				next.Files[runIndexKey(files[i])] = entry
			}
		}
		logCacheError(saveRunIndex(next))
	}
	return results
}

func matchRunFileCached(q query, path string, index runIndexManifest, changed *atomic.Bool) (string, runIndexEntry, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", runIndexEntry{}, false, fmt.Errorf("отчёт не прочитался: %w", err)
	}
	entryPath := runEntryPath(path)
	if entryPath != "" {
		if entry, ok := index.Files[runIndexKey(path)]; ok && runIndexMetadataMatches(entry, info) {
			line, valid, cacheErr := readRunEntry(q, entryPath, entry, info)
			if cacheErr != nil {
				logCacheError(cacheErr)
				changed.Store(true)
			}
			if valid {
				return line, entry, true, nil
			}
		} else {
			changed.Store(true)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", runIndexEntry{}, false, fmt.Errorf("отчёт не прочитался: %w", err)
	}
	if entryPath != "" {
		entry, cacheErr := writeRunEntry(entryPath, info, data)
		if cacheErr != nil {
			logCacheError(cacheErr)
			changed.Store(true)
		} else {
			changed.Store(true)
			return firstMatchDataFiltered(q, data, q.runs), entry, true, nil
		}
	}
	return firstMatchDataFiltered(q, data, q.runs), runIndexEntry{}, false, nil
}

func runIndexMetadataMatches(entry runIndexEntry, info os.FileInfo) bool {
	return entry.Size == info.Size() && entry.ModTime == info.ModTime().UnixNano() &&
		len(entry.Bloom) == runBloomBytes && crc32.ChecksumIEEE(entry.Bloom) == entry.CRC
}

func readRunEntry(q query, path string, entry runIndexEntry, info os.FileInfo) (string, bool, error) {
	if !runFilterMayMatchBloom(q.runs, entry.Bloom) {
		return "", true, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false, fmt.Errorf("кэш отчёта не открылся: %w", err)
	}
	header := make([]byte, runHeaderBytes)
	if _, err := io.ReadFull(f, header); err != nil {
		return "", false, errors.Join(fmt.Errorf("заголовок кэша отчёта не прочитался: %w", err), f.Close())
	}
	bloom := header[24:]
	if !bytes.Equal(header[:4], runCacheMagic[:]) ||
		int64(binary.LittleEndian.Uint64(header[4:12])) != info.Size() ||
		int64(binary.LittleEndian.Uint64(header[12:20])) != entry.ModTime ||
		binary.LittleEndian.Uint32(header[20:24]) != entry.CRC || crc32.ChecksumIEEE(bloom) != entry.CRC ||
		!bytes.Equal(bloom, entry.Bloom) {
		if err := f.Close(); err != nil {
			return "", false, fmt.Errorf("кэш отчёта не закрылся: %w", err)
		}
		return "", false, nil
	}
	reader, err := gzip.NewReader(io.LimitReader(f, info.Size()+(1<<20)))
	if err != nil {
		return "", false, errors.Join(fmt.Errorf("кэш отчёта не распаковался: %w", err), f.Close())
	}
	limited := &io.LimitedReader{R: reader, N: info.Size() + 1}
	line, readErr := firstMatchStream(q, limited, q.runs)
	gzipErr := reader.Close()
	fileErr := f.Close()
	if readErr != nil {
		return "", false, errors.Join(readErr, gzipErr, fileErr)
	}
	if limited.N == 0 && line == "" {
		return "", false, errors.Join(fmt.Errorf("кэшированный отчёт превышает исходный размер"), gzipErr, fileErr)
	}
	if gzipErr != nil {
		return "", false, errors.Join(fmt.Errorf("поток кэша отчёта не закрылся: %w", gzipErr), fileErr)
	}
	if fileErr != nil {
		return "", false, fmt.Errorf("кэш отчёта не закрылся: %w", fileErr)
	}
	return line, true, nil
}

func writeRunEntry(path string, info os.FileInfo, data []byte) (runIndexEntry, error) {
	var compressed bytes.Buffer
	gz, err := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	if err != nil {
		return runIndexEntry{}, fmt.Errorf("компрессор отчёта не создался: %w", err)
	}
	if _, err := gz.Write(data); err == nil {
		err = gz.Close()
	} else {
		closeErr := gz.Close()
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		return runIndexEntry{}, fmt.Errorf("кэш отчёта не сжался: %w", err)
	}
	header := make([]byte, runHeaderBytes)
	copy(header[:4], runCacheMagic[:])
	binary.LittleEndian.PutUint64(header[4:12], uint64(info.Size()))
	binary.LittleEndian.PutUint64(header[12:20], uint64(info.ModTime().UnixNano()))
	bloom := runTextBloom(data)
	crc := crc32.ChecksumIEEE(bloom)
	binary.LittleEndian.PutUint32(header[20:24], crc)
	copy(header[24:], bloom)
	if err := saveCacheBytes(path, append(header, compressed.Bytes()...)); err != nil {
		return runIndexEntry{}, err
	}
	return runIndexEntry{Size: info.Size(), ModTime: info.ModTime().UnixNano(), Bloom: bloom, CRC: crc}, nil
}

func runTextBloom(data []byte) []byte {
	bloom := make([]byte, runBloomBytes)
	for at := 0; at < len(data); {
		end := bytes.IndexByte(data[at:], '\n')
		if end < 0 {
			end = len(data) - at
		}
		folded := foldText(string(data[at : at+end]))
		for i := 0; i+3 <= len(folded); i++ {
			bloomAdd(bloom, folded[i:i+3])
		}
		if at+end == len(data) {
			break
		}
		at += end + 1
	}
	return bloom
}

func bloomAdd(bloom []byte, gram string) {
	base := bloomHash(gram)
	bits := uint64(len(bloom) * 8)
	for i := range uint64(4) {
		bit := (base + i*0x9e3779b97f4a7c15) % bits
		bloom[bit/8] |= 1 << (bit % 8)
	}
}

func bloomHas(bloom []byte, gram string) bool {
	base := bloomHash(gram)
	bits := uint64(len(bloom) * 8)
	for i := range uint64(4) {
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
		if len(variant) < 3 || bloomHasVariant(variant, bloom) {
			return true
		}
	}
	return false
}

func bloomHasVariant(variant string, bloom []byte) bool {
	for i := 0; i+3 <= len(variant); i++ {
		if !bloomHas(bloom, variant[i:i+3]) {
			return false
		}
	}
	return true
}

func decompressRun(data []byte, maxBytes int64) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("кэш отчёта не распаковался: %w", err)
	}
	content, readErr := io.ReadAll(io.LimitReader(r, maxBytes+1))
	closeErr := r.Close()
	if readErr != nil {
		return nil, fmt.Errorf("кэш отчёта не прочитался: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("кэш отчёта не закрылся: %w", closeErr)
	}
	if int64(len(content)) > maxBytes {
		return nil, fmt.Errorf("кэш отчёта превысил исходный размер")
	}
	return content, nil
}
