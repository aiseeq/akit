// Package replycache stores the user and ask events of individual transcripts.
package replycache

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aiseeq/akit/transcript"
)

const version = 5
const maxEntry = 256 << 20
const maxCache = 512 << 20
const maxAge = 30 * 24 * time.Hour
const liveAge = 10 * time.Minute
const privateMode = 0700
const otherPermissions = 0077

var errBadPrefilter = errors.New("bad lines require filtered reading")

type key struct {
	Path               string
	Size, Mtime, Ctime int64
	Dev, Inode         uint64
	Version            int
}

type header struct {
	Key          key
	Bad          int
	CompleteFile bool
	Length       int64
	Sum          [32]byte
}

type event struct {
	Nano               int64
	Zero               bool
	Line               int
	Role               transcript.Role
	Images             int
	Who, Subtype, Text string
	Raw, RawDay        string
	Answer             string
}

type record struct {
	Events                []event
	ActivityEvents        []event
	ActivityDays          []activityDay
	ActivityTailHash      [32]byte
	ActivitySize          int64
	ActivityLastTimestamp time.Time
	ActivityPartial       bool
	ActivityUncertain     bool
	ActivityUnordered     bool
	BadLines              []badLine
	BadCoverageDays       []string
	RecordBuckets         []int64 // Exact UnixNano timestamps of transcript activity.
	RecordTimes           []RecordTime
	CompleteFile          bool
	ActivityComplete      bool
	ActivityCode          string
	Coverage              []coverageRange
	BadDays               []string
	BadGlobal             bool
	Name                  string
	Bad                   int
	FirstBad              string
}

type activityDay struct {
	Day                       string
	HasRecord, Bad, Uncertain bool
}

type badLine struct {
	Nano    int64
	Zero    bool
	Day     string
	Line    int
	Message string
}

// RecordTime preserves an exact instant and the source timestamp's calendar day.
// Some callers decide --until by that text day before comparing the instant.
type RecordTime struct {
	Seconds    int64
	Nanosecond int32
	TextDay    string
	RecordDay  bool
}

// Snapshot binds a cached event record to the source file state observed before scanning.
type Snapshot struct {
	key    key
	Reason string
}

// Matches reports whether an open file still has the source identity captured by this snapshot.
func (s Snapshot) Matches(path string, info os.FileInfo) bool {
	k, err := fileKeyFromInfo(path, info)
	return err == nil && s.Reason == "" && s.key == k
}

// CachedTranscript is the query-independent content cached for one transcript.
type CachedTranscript struct {
	Events                []transcript.Event
	ActivityEvents        []transcript.Event
	ActivityDays          []ActivityDay
	ActivitySize          int64
	ActivityTailHash      [32]byte
	ActivityLastTimestamp time.Time
	ActivityPartial       bool
	ActivityUncertain     bool
	ActivityUnordered     bool
	BadLines              []BadLine
	BadCoverageDays       []string
	RawDays               []string
	Meta                  transcript.Meta
	RecordBuckets         []int64 // Exact UnixNano timestamps of transcript activity.
	RecordTimes           []RecordTime
	ActivityComplete      bool
	Coverage              []CoverageRange
	BadDays               []string
	BadGlobal             bool
}

// ActivityDay summarizes records for conservative transcript candidate selection.
type ActivityDay struct {
	Day                       string
	HasRecord, Bad, Uncertain bool
}

// ActivityIndex contains query-independent Parser events and conservative day flags.
type ActivityIndex struct {
	Events           []transcript.Event
	Days             []ActivityDay
	IndexedSize      int64
	TailHash         [32]byte
	LastTimestamp    time.Time
	Partial          bool
	Uncertain        bool
	NonChronological bool
}

// ActivityState binds an index lookup to the source snapshot and reports a safe append point.
type ActivityState struct {
	Index      ActivityIndex
	Snapshot   Snapshot
	ReadFrom   int64
	Reusable   bool
	Appendable bool
}

// BadLine describes a malformed source record captured during a covered scan.
type BadLine struct {
	Time    time.Time
	Day     string
	Line    int
	Message string
}

type coverageRange struct{ From, Until int64 }

// CoverageRange is one contiguous time interval represented by a file record.
type CoverageRange struct{ From, Until time.Time }

// Covers reports whether the stored file scan includes the requested time interval.
func (c CachedTranscript) Covers(from, until time.Time) bool {
	if !c.ActivityComplete {
		return false
	}
	for _, interval := range c.Coverage {
		if !from.Before(interval.From) && !until.After(interval.Until) {
			return true
		}
	}
	return false
}

// Cache is safe for concurrent use. Misses counts original transcript reads;
// Issues counts cache failures that fall back to original transcript reads.
type Cache struct {
	root, dir    string
	activityCode string
	Misses       atomic.Int64
	Issues       atomic.Int64 // cache failures which fall back to original transcript reads
	lastIssue    atomic.Pointer[cacheIssue]
}

type cacheIssue struct{ message string }

// New uses the standard user cache directory. An unavailable cache is harmless.
func New() *Cache {
	return NewForCode("")
}

// NewForCode binds query-independent activity data to its producer's parser version.
func NewForCode(activityCode string) *Cache {
	root, err := os.UserCacheDir()
	if err != nil { //nolint:silent-error-handling // Без каталога кэша потребитель читает оригинал.
		return &Cache{}
	}
	root = filepath.Join(root, "akit", "transcript-replies")
	code := cacheCode + "-" + runtime.Version() + "-" + runtime.GOOS + "-" + runtime.GOARCH
	return &Cache{root: root, dir: filepath.Join(root, code), activityCode: activityCode}
}

func fileKey(path string) (key, error) {
	info, err := os.Stat(path)
	if err != nil {
		return key{}, fmt.Errorf("stat transcript: %w", err)
	}
	return fileKeyFromInfo(path, info)
}

func fileKeyFromInfo(path string, info os.FileInfo) (key, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() {
		return key{}, errors.New("not a regular file")
	}
	return key{Path: path, Size: info.Size(), Mtime: info.ModTime().UnixNano(), Ctime: stat.Ctim.Sec*1e9 + stat.Ctim.Nsec, Dev: uint64(stat.Dev), Inode: stat.Ino, Version: version}, nil
}

func (c *Cache) path(path string) string {
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])+".gob")
}

// Snapshot captures the source identity before a scan.
func (c *Cache) Snapshot(path string) Snapshot {
	k, err := fileKey(path)
	if err != nil {
		return Snapshot{Reason: fmt.Sprintf("snapshot source: %v", err)}
	}
	return Snapshot{key: k}
}

// SnapshotFile binds a scan to the identity of the already-open source file.
// A later store still checks the path identity, so replacement during the scan
// prevents publishing events from the old file under the new path.
func (c *Cache) SnapshotFile(path string, info os.FileInfo) Snapshot {
	k, err := fileKeyFromInfo(path, info)
	if err != nil {
		return Snapshot{Reason: fmt.Sprintf("snapshot source: %v", err)}
	}
	return Snapshot{key: k}
}

// NoteIssue accounts for a cache error which the caller deliberately treats as
// a miss, preserving the normal transcript-reading result.
func (c *Cache) NoteIssue(message string) {
	if c != nil {
		c.lastIssue.Store(&cacheIssue{message: message})
		c.Issues.Add(1)
	}
}

// LastIssue returns the most recent cache failure retained for diagnostics.
func (c *Cache) LastIssue() string {
	if c == nil {
		return ""
	}
	issue := c.lastIssue.Load()
	if issue == nil {
		return ""
	}
	return issue.message
}

// Available reports whether the cache directory is private and can accept a file.
func (c *Cache) Available() bool {
	return c.writable()
}

// HasEntries reports whether the current cache directory contains any transcript records.
// Callers can skip one cache lookup per source file when a newly created cache is empty.
func (c *Cache) HasEntries() bool {
	if c == nil || c.dir == "" {
		return false
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".gob") {
			return true
		}
	}
	return false
}

func compactInt64(values []int64) []int64 {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, v := range values[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

func privateDir(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&otherPermissions != 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid())
}

func (c *Cache) open(path string, rejectBad bool) (*os.File, *gob.Decoder, header, error) {
	if c == nil || c.dir == "" || !privateDir(c.dir) {
		return nil, nil, header{}, errors.New("no cache")
	}
	cachePath := c.path(path)
	info, err := os.Lstat(cachePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&otherPermissions != 0 {
		return nil, nil, header{}, errors.New("unsafe cache entry")
	}
	k, err := fileKey(path)
	if err != nil {
		return nil, nil, header{}, err
	}
	// Stat alone does not check that the transcript can still be opened.
	original, err := os.Open(path)
	if err != nil {
		return nil, nil, header{}, fmt.Errorf("open transcript: %w", err)
	}
	original.Close()
	f, err := os.Open(cachePath)
	if err != nil {
		return nil, nil, header{}, fmt.Errorf("open cache entry: %w", err)
	}
	dec := gob.NewDecoder(f)
	var h header
	if err = dec.Decode(&h); err != nil || h.Key != k || h.Length < 0 || h.Length > maxEntry {
		f.Close()
		return nil, nil, header{}, errors.New("cache miss")
	}
	if rejectBad && h.Bad > 0 {
		f.Close()
		return nil, nil, header{}, errBadPrefilter
	}
	return f, dec, h, nil
}

func recentlyModified(path string) bool {
	info, err := os.Stat(path)
	return err == nil && time.Since(info.ModTime()) < liveAge
}

// writable checks actual creation, including ACLs and a read-only filesystem.
func (c *Cache) writable() bool {
	if c == nil || c.dir == "" || os.MkdirAll(c.dir, privateMode) != nil || !privateDir(c.dir) {
		return false
	}
	f, err := os.CreateTemp(c.dir, ".probe-*")
	if err != nil { //nolint:error-masked-as-false-bool // Ошибка пробы означает отказ от кэша и чтение оригинала.
		return false
	}
	name := f.Name()
	closeErr := f.Close()
	removeErr := os.Remove(name)
	return closeErr == nil && removeErr == nil
}

// Valid checks the header only, for estimating admission cost.
func (c *Cache) Valid(path string, rejectBad bool) bool {
	if rejectBad && recentlyModified(path) {
		return false
	}
	f, _, h, err := c.open(path, rejectBad)
	if err != nil {
		return false
	}
	f.Close()
	return h.CompleteFile
}

func (c *Cache) loadRecord(path string, rejectBad bool) (record, header, error) {
	f, dec, h, err := c.open(path, rejectBad)
	if err != nil {
		return record{}, header{}, err
	}
	defer f.Close()
	var payload []byte
	if err := dec.Decode(&payload); err != nil {
		return record{}, header{}, fmt.Errorf("decode cache: %w", err)
	}
	if int64(len(payload)) != h.Length || sha256.Sum256(payload) != h.Sum {
		return record{}, header{}, errors.New("invalid cache checksum")
	}
	var rec record
	if err := gob.NewDecoder(bytes.NewReader(payload)).Decode(&rec); err != nil {
		return record{}, header{}, fmt.Errorf("decode cache payload: %w", err)
	}
	if rec.Bad != h.Bad || rec.CompleteFile != h.CompleteFile {
		return record{}, header{}, errors.New("invalid cache diagnostics")
	}
	if !validCoverage(rec.Coverage) {
		return record{}, header{}, errors.New("invalid cache coverage")
	}
	if k, err := fileKey(path); err != nil || k != h.Key {
		return record{}, header{}, errors.New("transcript changed")
	}
	return rec, h, nil
}

// loadRecordAtPath validates a cache entry without requiring its source key to match
// the current file. ActivityIndex uses it to verify a safe append from the previous
// inode and indexed prefix.
func (c *Cache) loadRecordAtPath(path string) (record, header, error) {
	if c == nil || c.dir == "" || !privateDir(c.dir) {
		return record{}, header{}, errors.New("no cache")
	}
	cachePath := c.path(path)
	info, err := os.Lstat(cachePath)
	if err != nil {
		return record{}, header{}, fmt.Errorf("stat cache entry: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&otherPermissions != 0 {
		return record{}, header{}, errors.New("unsafe cache entry")
	}
	f, err := os.Open(cachePath)
	if err != nil {
		return record{}, header{}, fmt.Errorf("open cache entry: %w", err)
	}
	defer f.Close()
	dec := gob.NewDecoder(f)
	var h header
	if err := dec.Decode(&h); err != nil || h.Key.Path != path || h.Length < 0 || h.Length > maxEntry {
		return record{}, header{}, errors.New("invalid cache header")
	}
	var payload []byte
	if err := dec.Decode(&payload); err != nil {
		return record{}, header{}, fmt.Errorf("decode cache: %w", err)
	}
	if int64(len(payload)) != h.Length || sha256.Sum256(payload) != h.Sum {
		return record{}, header{}, errors.New("invalid cache checksum")
	}
	var rec record
	if err := gob.NewDecoder(bytes.NewReader(payload)).Decode(&rec); err != nil {
		return record{}, header{}, fmt.Errorf("decode cache payload: %w", err)
	}
	if rec.Bad != h.Bad || rec.CompleteFile != h.CompleteFile || !validCoverage(rec.Coverage) {
		return record{}, header{}, errors.New("invalid cache diagnostics")
	}
	return rec, h, nil
}

// LoadActivityIndex returns a matching index, or an append point only when the
// source is the same inode, has grown, and the indexed prefix hash still matches.
func (c *Cache) LoadActivityIndex(path string) (ActivityState, bool, error) {
	current, err := fileKey(path)
	if err != nil {
		return ActivityState{}, false, err
	}
	state := ActivityState{Snapshot: Snapshot{key: current}}
	old, h, err := c.loadRecordAtPath(path)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
		return state, false, nil
	}
	if err != nil {
		return state, false, err
	}
	if !old.ActivityComplete || old.ActivityCode != c.activityCode || old.ActivitySize < 0 || old.ActivitySize > h.Key.Size || (!old.ActivityPartial && old.ActivitySize != h.Key.Size) {
		return state, false, nil
	}
	index := unpackActivityIndex(old)
	if h.Key == current {
		if latest, err := fileKey(path); err != nil || latest != current {
			return state, false, errors.New("transcript changed during activity lookup")
		}
		state.Index, state.Reusable = index, true
		if !index.Partial {
			state.ReadFrom = index.IndexedSize
		}
		return state, true, nil
	}
	index, readFrom, appendable, err := c.appendActivityIndex(path, current, h, old, index)
	if err != nil {
		return state, false, err
	}
	if !appendable {
		return state, false, nil
	}
	state.Index, state.ReadFrom, state.Reusable, state.Appendable = index, readFrom, true, true
	return state, true, nil
}

func (c *Cache) appendActivityIndex(path string, current key, h header, old record, index ActivityIndex) (ActivityIndex, int64, bool, error) {
	if current.Dev != h.Key.Dev || current.Inode != h.Key.Inode || current.Size <= h.Key.Size || old.ActivityPartial || old.ActivitySize != h.Key.Size {
		return ActivityIndex{}, 0, false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return ActivityIndex{}, 0, false, fmt.Errorf("open transcript for append check: %w", err)
	}
	actual, err := HashIndexedTail(f, old.ActivitySize)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return ActivityIndex{}, 0, false, fmt.Errorf("hash indexed transcript tail: %w", err)
	}
	if actual != old.ActivityTailHash {
		return ActivityIndex{}, 0, false, nil
	}
	if latest, err := fileKey(path); err != nil || latest != current {
		return ActivityIndex{}, 0, false, errors.New("transcript changed during activity lookup")
	}
	return index, index.IndexedSize, true, nil
}

// StoreActivityIndex atomically publishes the parser index for the exact source snapshot.
func (c *Cache) StoreActivityIndex(path string, state ActivityState, index ActivityIndex) string {
	if state.Snapshot.Reason != "" {
		return state.Snapshot.Reason
	}
	if c == nil || c.dir == "" || state.Snapshot.key.Path != path {
		return "activity cache unavailable or source mismatch"
	}
	current, err := fileKey(path)
	if err != nil {
		return fmt.Sprintf("restat source before activity cache write: %v", err)
	}
	if current != state.Snapshot.key || index.IndexedSize < 0 || index.IndexedSize > current.Size {
		return "source changed or invalid activity index before write"
	}
	rec := record{ActivityComplete: true, ActivityCode: c.activityCode}
	if old, h, err := c.loadRecord(path, false); err == nil && h.Key == current {
		rec = old
	}
	rec.ActivityComplete, rec.ActivityCode = true, c.activityCode
	rec.ActivityEvents = packEvents(index.Events)
	rec.ActivityDays = packActivityDays(index.Days)
	rec.ActivitySize, rec.ActivityTailHash = index.IndexedSize, index.TailHash
	rec.ActivityLastTimestamp = index.LastTimestamp
	rec.ActivityPartial, rec.ActivityUncertain, rec.ActivityUnordered = index.Partial, index.Uncertain, index.NonChronological
	if err := c.save(current, rec); err != nil {
		return fmt.Sprintf("save activity index: %v", err)
	}
	return ""
}

func unpackActivityIndex(rec record) ActivityIndex {
	return ActivityIndex{
		Events: unpackEvents(rec.ActivityEvents), Days: unpackActivityDays(rec.ActivityDays),
		IndexedSize: rec.ActivitySize, TailHash: rec.ActivityTailHash, LastTimestamp: rec.ActivityLastTimestamp,
		Partial: rec.ActivityPartial, Uncertain: rec.ActivityUncertain, NonChronological: rec.ActivityUnordered,
	}
}

func packActivityDays(days []ActivityDay) []activityDay {
	out := make([]activityDay, 0, len(days))
	for _, day := range days {
		out = append(out, activityDay{Day: day.Day, HasRecord: day.HasRecord, Bad: day.Bad, Uncertain: day.Uncertain})
	}
	return out
}

func unpackActivityDays(days []activityDay) []ActivityDay {
	out := make([]ActivityDay, 0, len(days))
	for _, day := range days {
		out = append(out, ActivityDay{Day: day.Day, HasRecord: day.HasRecord, Bad: day.Bad, Uncertain: day.Uncertain})
	}
	return out
}

// HashIndexedTail returns the SHA-256 of the final 4 KiB of an indexed byte range.
func HashIndexedTail(f *os.File, size int64) ([32]byte, error) {
	start := max(int64(0), size-(4<<10))
	data := make([]byte, size-start)
	if len(data) > 0 {
		if _, err := f.ReadAt(data, start); err != nil {
			return [32]byte{}, fmt.Errorf("read indexed tail: %w", err)
		}
	}
	return sha256.Sum256(data), nil
}

func validCoverage(ranges []coverageRange) bool {
	for i, current := range ranges {
		if current.Until < current.From {
			return false
		}
		if i > 0 && current.From <= ranges[i-1].Until {
			return false
		}
	}
	return true
}

// CachedEvents reads a query-independent event record without populating a missing cache.
func (c *Cache) CachedEvents(path string) (CachedTranscript, bool, error) {
	rec, _, err := c.loadRecord(path, false)
	if err != nil {
		return CachedTranscript{}, false, err
	}
	coverage := make([]CoverageRange, 0, len(rec.Coverage))
	for _, interval := range rec.Coverage {
		coverage = append(coverage, CoverageRange{From: time.Unix(0, interval.From).UTC(), Until: time.Unix(0, interval.Until).UTC()})
	}
	rawDays := make([]string, len(rec.Events))
	for i := range rec.Events {
		rawDays[i] = rec.Events[i].RawDay
	}
	events := rec.Events
	if len(events) == 0 && rec.ActivityComplete && rec.ActivityCode == c.activityCode {
		events = rec.ActivityEvents
	}
	return CachedTranscript{
		Events:                unpackEvents(events),
		ActivityEvents:        unpackEvents(rec.ActivityEvents),
		ActivityDays:          unpackActivityDays(rec.ActivityDays),
		ActivitySize:          rec.ActivitySize,
		ActivityTailHash:      rec.ActivityTailHash,
		ActivityLastTimestamp: rec.ActivityLastTimestamp,
		ActivityPartial:       rec.ActivityPartial,
		ActivityUncertain:     rec.ActivityUncertain,
		ActivityUnordered:     rec.ActivityUnordered,
		BadLines:              unpackBadLines(rec.BadLines),
		BadCoverageDays:       slices.Clone(rec.BadCoverageDays),
		RawDays:               rawDays,
		Meta:                  meta(rec),
		RecordBuckets:         rec.RecordBuckets,
		RecordTimes:           slices.Clone(rec.RecordTimes),
		ActivityComplete:      rec.ActivityComplete && rec.ActivityCode == c.activityCode,
		Coverage:              coverage,
		BadDays:               slices.Clone(rec.BadDays),
		BadGlobal:             rec.BadGlobal,
	}, true, nil
}

// StoreEvents publishes query-independent user and ask events plus record-time buckets.
func (c *Cache) StoreEvents(path string, snapshot Snapshot, events []transcript.Event, buckets []int64, badDays []string, badGlobal bool, from, until time.Time, m transcript.Meta) string {
	return c.StoreEventsWithBadLines(path, snapshot, events, buckets, badDays, badGlobal, nil, nil, from, until, m)
}

// StoreEventsWithBadLines adds query-filtered malformed-line details and the raw days
// for which those details are complete. Query callers still decide relevance at read time.
func (c *Cache) StoreEventsWithBadLines(path string, snapshot Snapshot, events []transcript.Event, buckets []int64, badDays []string, badGlobal bool, badLines []BadLine, badCoverageDays []string, from, until time.Time, m transcript.Meta) string {
	return c.StoreEventsWithBadLinesFromLookup(path, snapshot, events, buckets, badDays, badGlobal, badLines, badCoverageDays, from, until, m, true)
}

// StoreEventsWithBadLinesFromLookup merges an existing record only when the caller
// successfully loaded one before scanning. This avoids reopening the source and
// probing the cache a second time on a cold miss.
func (c *Cache) StoreEventsWithBadLinesFromLookup(path string, snapshot Snapshot, events []transcript.Event, buckets []int64, badDays []string, badGlobal bool, badLines []BadLine, badCoverageDays []string, from, until time.Time, m transcript.Meta, mergeExisting bool) string {
	reason := c.StoreEventsWithRecordTimesFromLookup(path, snapshot, events, buckets, nil, badDays, badGlobal, badLines, badCoverageDays, from, until, m, mergeExisting)
	if strings.HasPrefix(reason, "save cache record:") {
		return ""
	}
	return reason
}

// StoreEventsWithRecordTimesFromLookup stores activity timestamps with their original
// textual values as well as exact instants. Some readers use the source date before
// comparing timestamp instants, so UnixNano alone is not sufficient.
func (c *Cache) StoreEventsWithRecordTimesFromLookup(path string, snapshot Snapshot, events []transcript.Event, buckets []int64, recordTimes []RecordTime, badDays []string, badGlobal bool, badLines []BadLine, badCoverageDays []string, from, until time.Time, m transcript.Meta, mergeExisting bool) string {
	if snapshot.Reason != "" {
		return snapshot.Reason
	}
	if c == nil || c.dir == "" || snapshot.key.Path != path {
		return "event cache unavailable or source mismatch"
	}
	current, err := fileKey(path)
	if err != nil {
		return fmt.Sprintf("restat source before event cache write: %v", err)
	}
	if current != snapshot.key {
		return "source changed before event cache write"
	}
	rec := record{
		ActivityComplete: true, ActivityCode: c.activityCode, Name: m.Name, Bad: m.Bad,
		Events: packEvents(events), Coverage: []coverageRange{{From: from.UnixNano(), Until: until.UnixNano()}},
		BadDays: compactStrings(slices.Clone(badDays)), BadGlobal: badGlobal,
		BadLines: packBadLines(badLines), BadCoverageDays: compactStrings(slices.Clone(badCoverageDays)),
	}
	if m.FirstBad != nil {
		rec.FirstBad = m.FirstBad.Error()
	}
	var old record
	oldLoaded := false
	if mergeExisting {
		var err error
		old, _, err = c.loadRecord(path, false)
		oldLoaded = err == nil
	}
	mergeActivityData(&rec, old, oldLoaded, c.activityCode, buckets, recordTimes)
	// The TL whole-file cache and the activity index share this record. Keep
	// whole-file content owned by TL while refreshing activity data owned by the
	// current producer.
	preserveWholeFile(&rec, old, oldLoaded)
	slices.Sort(rec.RecordBuckets)
	rec.RecordBuckets = compactInt64(rec.RecordBuckets)
	slices.SortFunc(rec.RecordTimes, func(a, b RecordTime) int {
		if n := cmp.Compare(a.Seconds, b.Seconds); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Nanosecond, b.Nanosecond); n != 0 {
			return n
		}
		if n := strings.Compare(a.TextDay, b.TextDay); n != 0 {
			return n
		}
		if a.RecordDay == b.RecordDay {
			return 0
		}
		if a.RecordDay {
			return 1
		}
		return -1
	})
	rec.RecordTimes = slices.Compact(rec.RecordTimes)
	if err := c.save(snapshot.key, rec); err != nil {
		return fmt.Sprintf("save cache record: %v", err)
	}
	return ""
}

func mergeActivityData(rec *record, old record, oldLoaded bool, activityCode string, buckets []int64, times []RecordTime) {
	if oldLoaded && old.ActivityComplete && old.ActivityCode == activityCode {
		mergeActivityRecord(rec, old, buckets, times)
		return
	}
	rec.RecordBuckets = buckets
	rec.RecordTimes = slices.Clone(times)
}

func mergeActivityRecord(rec *record, old record, buckets []int64, times []RecordTime) {
	rec.Events = mergeEvents(old.Events, rec.Events)
	rec.RecordBuckets = append(old.RecordBuckets, buckets...)
	rec.RecordTimes = append(old.RecordTimes, times...)
	rec.Bad = max(rec.Bad, old.Bad)
	rec.BadDays = mergeStrings(old.BadDays, rec.BadDays)
	rec.BadLines = mergeBadLines(old.BadLines, rec.BadLines)
	rec.BadCoverageDays = mergeStrings(old.BadCoverageDays, rec.BadCoverageDays)
	rec.BadGlobal = rec.BadGlobal || old.BadGlobal
	if rec.FirstBad == "" {
		rec.FirstBad = old.FirstBad
	}
	rec.Name = firstNonEmpty(rec.Name, old.Name)
	rec.Coverage = mergeCoverage(old.Coverage, rec.Coverage)
}

func preserveWholeFile(rec *record, old record, oldLoaded bool) {
	if !oldLoaded || !old.CompleteFile {
		return
	}
	rec.Events = old.Events
	rec.CompleteFile = true
	rec.Name = firstNonEmpty(rec.Name, old.Name)
	rec.Bad = max(rec.Bad, old.Bad)
	if rec.FirstBad == "" {
		rec.FirstBad = old.FirstBad
	}
}

func packBadLines(lines []BadLine) []badLine {
	out := make([]badLine, 0, len(lines))
	for _, item := range lines {
		stored := badLine{Day: item.Day, Line: item.Line, Message: item.Message}
		if item.Time.IsZero() {
			stored.Zero = true
		} else {
			stored.Nano = item.Time.UnixNano()
		}
		out = append(out, stored)
	}
	return mergeBadLines(nil, out)
}

func unpackBadLines(lines []badLine) []BadLine {
	out := make([]BadLine, 0, len(lines))
	for _, item := range lines {
		stamp := time.Time{}
		if !item.Zero {
			stamp = time.Unix(0, item.Nano).UTC()
		}
		out = append(out, BadLine{Time: stamp, Day: item.Day, Line: item.Line, Message: item.Message})
	}
	return out
}

func mergeBadLines(old, current []badLine) []badLine {
	seen := make(map[badLine]bool, len(old)+len(current))
	out := make([]badLine, 0, len(old)+len(current))
	for _, list := range [][]badLine{old, current} {
		for _, item := range list {
			if seen[item] {
				continue
			}
			seen[item] = true
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		return out[i].Message < out[j].Message
	})
	return out
}

func mergeCoverage(old, current []coverageRange) []coverageRange {
	ranges := append(slices.Clone(old), current...)
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].From != ranges[j].From {
			return ranges[i].From < ranges[j].From
		}
		return ranges[i].Until < ranges[j].Until
	})
	if len(ranges) < 2 {
		return ranges
	}
	merged := ranges[:1]
	for _, next := range ranges[1:] {
		last := &merged[len(merged)-1]
		if next.From <= last.Until {
			last.Until = max(last.Until, next.Until)
			continue
		}
		merged = append(merged, next)
	}
	return merged
}

func mergeEvents(old, current []event) []event {
	seen := make(map[event]bool, len(old)+len(current))
	merged := make([]event, 0, len(old)+len(current))
	for _, list := range [][]event{old, current} {
		for i := range list {
			item := list[i]
			if seen[item] {
				continue
			}
			seen[item] = true
			merged = append(merged, item)
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Nano != merged[j].Nano {
			return merged[i].Nano < merged[j].Nano
		}
		return merged[i].Line < merged[j].Line
	})
	return merged
}

func firstNonEmpty(first, second string) string {
	if first != "" {
		return first
	}
	return second
}

func compactStrings(values []string) []string {
	slices.Sort(values)
	return slices.Compact(values)
}

func mergeStrings(old, current []string) []string {
	return compactStrings(append(slices.Clone(old), current...))
}

// Events returns a complete record, or usable=false for bad lines with a query-dependent prefilter.
// Read errors are returned so the caller can use its original reader and its diagnostics.
func (c *Cache) Events(path string, rejectBad bool) ([]transcript.Event, transcript.Meta, bool, error) {
	if rejectBad && recentlyModified(path) {
		return nil, transcript.Meta{}, false, nil
	}
	cachedRec, _, loadErr := c.loadRecord(path, rejectBad)
	if errors.Is(loadErr, errBadPrefilter) {
		return nil, transcript.Meta{}, false, nil
	}
	if loadErr == nil && cachedRec.CompleteFile {
		return unpackDisplayed(cachedRec), meta(cachedRec), true, nil
	}
	if c == nil {
		return nil, transcript.Meta{}, false, errors.New("cache unavailable")
	}
	if rejectBad && !c.writable() {
		return nil, transcript.Meta{}, false, nil
	}
	c.Misses.Add(1)
	before, err := fileKey(path)
	if err != nil {
		return nil, transcript.Meta{}, false, err
	}
	var rec record
	m, err := transcript.Read(path, nil, func(ev transcript.Event) {
		if ev.Role == transcript.User || ev.Role == transcript.Ask {
			rec.Events = append(rec.Events, packEvent(ev))
		}
	})
	if err != nil {
		return nil, transcript.Meta{}, false, err
	}
	rec.Name, rec.Bad = m.Name, m.Bad
	rec.CompleteFile = true
	if m.FirstBad != nil {
		rec.FirstBad = m.FirstBad.Error()
	}
	after, statErr := fileKey(path)
	if statErr == nil && before == after {
		// Preserve query-independent activity metadata written by user-word.
		old, _, oldErr := c.loadRecord(path, rejectBad)
		if oldErr == nil && old.ActivityComplete {
			rec.ActivityComplete = old.ActivityComplete
			rec.ActivityCode = old.ActivityCode
			rec.Coverage = old.Coverage
			rec.RecordBuckets = old.RecordBuckets
			rec.RecordTimes = old.RecordTimes
			rec.BadLines = old.BadLines
			rec.BadCoverageDays = old.BadCoverageDays
			rec.BadDays = old.BadDays
			rec.BadGlobal = old.BadGlobal
			rec.ActivityEvents = old.ActivityEvents
			rec.ActivityDays = old.ActivityDays
			rec.ActivitySize = old.ActivitySize
			rec.ActivityTailHash = old.ActivityTailHash
			rec.ActivityLastTimestamp = old.ActivityLastTimestamp
			rec.ActivityPartial = old.ActivityPartial
			rec.ActivityUncertain = old.ActivityUncertain
			rec.ActivityUnordered = old.ActivityUnordered
		}
		c.saveEventRecord(before, rec)
	}
	if rejectBad && rec.Bad > 0 {
		return nil, transcript.Meta{}, false, nil
	}
	return unpackDisplayed(rec), meta(rec), true, nil
}

func (c *Cache) saveEventRecord(before key, rec record) {
	if err := c.save(before, rec); err != nil {
		c.NoteIssue(fmt.Sprintf("save event cache record: %v", err))
	}
}

func unpackRaw(rec record) []transcript.Event {
	return unpackEvents(rec.Events)
}

func unpackEvents(packed []event) []transcript.Event {
	events := make([]transcript.Event, 0, len(packed))
	for i := range packed {
		ev := &packed[i]
		var at time.Time
		if !ev.Zero {
			at = time.Unix(0, ev.Nano).UTC()
		}
		var restored transcript.Event
		restored.Time, restored.Line, restored.Role = at, ev.Line, ev.Role
		restored.Who, restored.Subtype, restored.Text = ev.Who, ev.Subtype, ev.Text
		restored.Answer, restored.Images = ev.Answer, ev.Images
		if ev.Raw != "" {
			restored.Raw = []byte(ev.Raw)
		}
		events = append(events, restored)
	}
	return events
}

func unpackDisplayed(rec record) []transcript.Event {
	events := unpackRaw(rec)
	for i := range events {
		events[i].Text = events[i].Shown()
		events[i].Images = 0
		events[i].Answer = ""
		events[i].Raw = nil
	}
	return events
}

func packEvents(events []transcript.Event) []event {
	out := make([]event, 0, len(events))
	for i := range events {
		ev := &events[i]
		if ev.Role != transcript.User && ev.Role != transcript.Ask {
			continue
		}
		out = append(out, packEvent(*ev))
	}
	return out
}

func packEvent(ev transcript.Event) event {
	raw := string(ev.Raw)
	rawDay := ""
	if _, stamp, ok := bytes.Cut(ev.Raw, []byte(`"timestamp":"`)); ok && len(stamp) >= len("2006-01-02") {
		rawDay = string(stamp[:len("2006-01-02")])
	}
	return event{
		Nano: ev.Time.UnixNano(), Zero: ev.Time.IsZero(), Line: ev.Line, Role: ev.Role,
		Who: ev.Who, Subtype: ev.Subtype, Text: ev.Text, Answer: ev.Answer, Images: ev.Images,
		Raw: raw, RawDay: rawDay,
	}
}

func meta(rec record) transcript.Meta {
	m := transcript.Meta{Name: rec.Name, Bad: rec.Bad}
	if rec.FirstBad != "" {
		m.FirstBad = errors.New(rec.FirstBad)
	}
	return m
}

func (c *Cache) save(k key, rec record) error {
	if c.dir == "" {
		return errors.New("cache directory unavailable")
	}
	var payload bytes.Buffer
	if err := gob.NewEncoder(&payload).Encode(rec); err != nil {
		return fmt.Errorf("encode cache record: %w", err)
	}
	if payload.Len() > maxEntry {
		return fmt.Errorf("cache record exceeds %d bytes", maxEntry)
	}
	if err := os.MkdirAll(c.dir, privateMode); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}
	if !privateDir(c.dir) {
		return errors.New("cache directory is not private")
	}
	f, err := os.CreateTemp(c.dir, ".entry-*")
	if err != nil {
		return fmt.Errorf("create temporary cache entry: %w", err)
	}
	defer os.Remove(f.Name())
	h := header{Key: k, Bad: rec.Bad, CompleteFile: rec.CompleteFile, Length: int64(payload.Len()), Sum: sha256.Sum256(payload.Bytes())}
	enc := gob.NewEncoder(f)
	err = enc.Encode(h)
	if err == nil {
		err = enc.Encode(payload.Bytes())
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write cache entry: %w", err)
	}
	if err := os.Rename(f.Name(), c.path(k.Path)); err != nil {
		return fmt.Errorf("publish cache entry: %w", err)
	}
	return nil
}

// cleanupInfo позволяет проверить ошибку метаданных исчезнувшей записи.
var cleanupInfo = func(entry os.DirEntry) (os.FileInfo, error) { return entry.Info() }

// Cleanup removes obsolete entries, other code versions and excess bytes.
func (c *Cache) Cleanup(now time.Time) {
	if c == nil || c.root == "" {
		return
	}
	dirs, err := os.ReadDir(c.root)
	if err != nil { //nolint:silent-error-handling // Уборка кэша повторится позже.
		return
	}
	type item struct {
		path     string
		size     int64
		modified time.Time
	}
	var items []item
	var total int64
	for _, dir := range dirs {
		path := filepath.Join(c.root, dir.Name())
		if !dir.IsDir() {
			continue
		}
		if path != c.dir {
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil { //nolint:silent-error-handling // Недоступный каталог пропускается при уборке.
			continue
		}
		for _, entry := range entries {
			p := filepath.Join(path, entry.Name())
			info, err := cleanupInfo(entry)
			if err != nil { //nolint:fallback-return,silent-error-handling // Исчезнувшая запись не мешает уборке остальных.
				continue
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if now.Sub(info.ModTime()) > maxAge {
				_ = os.Remove(p) //nolint:ignored-error // Неудалённая запись останется до следующей уборки.
				continue
			}
			if strings.HasPrefix(entry.Name(), ".entry-") || strings.HasPrefix(entry.Name(), ".probe-") {
				continue
			}
			f, err := os.Open(p)
			if err != nil { //nolint:silent-error-handling // Исчезнувшая или недоступная запись пропускается.
				continue
			}
			var h header
			err = gob.NewDecoder(f).Decode(&h)
			f.Close()
			if err != nil { //nolint:silent-error-handling // Битую запись можно удалить и восстановить из оригинала.
				_ = os.Remove(p) //nolint:ignored-error // Неудалённая запись останется до следующей уборки.
				continue
			}
			if _, err := os.Stat(h.Key.Path); errors.Is(err, os.ErrNotExist) {
				_ = os.Remove(p) //nolint:ignored-error // Неудалённая запись останется до следующей уборки.
				continue
			}
			total += info.Size()
			items = append(items, item{p, info.Size(), info.ModTime()})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].modified.Before(items[j].modified) })
	for _, it := range items {
		if total <= maxCache {
			break
		}
		if os.Remove(it.path) == nil {
			total -= it.size
		}
	}
}
