// Package replycache stores the user and ask events of individual transcripts.
package replycache

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"

	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aiseeq/akit/transcript"
)

const version = 1
const maxEntry = 256 << 20
const maxCache = 512 << 20
const maxAge = 30 * 24 * time.Hour

type key struct {
	Path               string
	Size, Mtime, Ctime int64
	Dev, Inode         uint64
	Version            int
}

type header struct {
	Key    key
	Bad    int
	Length int64
	Sum    [32]byte
}

type event struct {
	Nano               int64
	Zero               bool
	Line               int
	Role               transcript.Role
	Who, Subtype, Text string
}

type record struct {
	Events   []event
	Name     string
	Bad      int
	FirstBad string
}

// Cache is safe for concurrent use. Misses counts original transcript reads.
type Cache struct {
	root, dir string
	Misses    atomic.Int64
}

// New uses the standard user cache directory. An unavailable cache is harmless.
func New() *Cache {
	root, err := os.UserCacheDir()
	if err != nil {
		return &Cache{}
	}
	root = filepath.Join(root, "akit", "transcript-replies")
	code := cacheCode + "-" + runtime.Version() + "-" + runtime.GOOS + "-" + runtime.GOARCH
	return &Cache{root: root, dir: filepath.Join(root, code)}
}

func fileKey(path string) (key, error) {
	info, err := os.Stat(path)
	if err != nil {
		return key{}, err
	}
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

func privateDir(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid())
}

func (c *Cache) open(path string, rejectBad bool) (*os.File, *gob.Decoder, header, error) {
	if c == nil || c.dir == "" || !privateDir(c.dir) {
		return nil, nil, header{}, errors.New("no cache")
	}
	k, err := fileKey(path)
	if err != nil {
		return nil, nil, header{}, err
	}
	// Stat alone does not check that the transcript can still be opened.
	original, err := os.Open(path)
	if err != nil {
		return nil, nil, header{}, err
	}
	original.Close()
	info, err := os.Lstat(c.path(path))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, nil, header{}, errors.New("unsafe cache entry")
	}
	f, err := os.Open(c.path(path))
	if err != nil {
		return nil, nil, header{}, err
	}
	dec := gob.NewDecoder(f)
	var h header
	if err = dec.Decode(&h); err != nil || h.Key != k || h.Length < 0 || h.Length > maxEntry || (rejectBad && h.Bad != 0) {
		f.Close()
		return nil, nil, header{}, errors.New("cache miss")
	}
	return f, dec, h, nil
}

// Valid checks the header only, for estimating admission cost.
func (c *Cache) Valid(path string, rejectBad bool) bool {
	f, _, _, err := c.open(path, rejectBad)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// Events returns a complete record, or usable=false for bad lines with a query-dependent prefilter.
// Read errors are returned so the caller can use its original reader and its diagnostics.
func (c *Cache) Events(path string, rejectBad bool) ([]transcript.Event, transcript.Meta, bool, error) {
	if f, dec, h, err := c.open(path, rejectBad); err == nil {
		var payload []byte
		err = dec.Decode(&payload)
		f.Close()
		if err == nil && int64(len(payload)) == h.Length && sha256.Sum256(payload) == h.Sum {
			var rec record
			if gob.NewDecoder(bytes.NewReader(payload)).Decode(&rec) == nil && rec.Bad == h.Bad {
				if k, err := fileKey(path); err == nil && k == h.Key {
					return unpack(rec), meta(rec), true, nil
				}
			}
		}
	}
	if c == nil {
		return nil, transcript.Meta{}, false, errors.New("cache unavailable")
	}
	c.Misses.Add(1)
	before, err := fileKey(path)
	if err != nil {
		return nil, transcript.Meta{}, false, err
	}
	var rec record
	m, err := transcript.Read(path, nil, func(ev transcript.Event) {
		if ev.Role == transcript.User || ev.Role == transcript.Ask {
			rec.Events = append(rec.Events, event{Nano: ev.Time.UnixNano(), Zero: ev.Time.IsZero(), Line: ev.Line, Role: ev.Role, Who: ev.Who, Subtype: ev.Subtype, Text: ev.Shown()})
		}
	})
	if err != nil {
		return nil, transcript.Meta{}, false, err
	}
	rec.Name, rec.Bad = m.Name, m.Bad
	if m.FirstBad != nil {
		rec.FirstBad = m.FirstBad.Error()
	}
	after, statErr := fileKey(path)
	if statErr == nil && before == after {
		c.save(before, rec)
	}
	if rejectBad && rec.Bad > 0 {
		return nil, transcript.Meta{}, false, nil
	}
	return unpack(rec), meta(rec), true, nil
}

func unpack(rec record) []transcript.Event {
	events := make([]transcript.Event, 0, len(rec.Events))
	for _, ev := range rec.Events {
		var at time.Time
		if !ev.Zero {
			at = time.Unix(0, ev.Nano).UTC()
		}
		// Restore only the display projection: the other Event fields are deliberately absent from the cache.
		var restored transcript.Event
		restored.Time, restored.Line, restored.Role = at, ev.Line, ev.Role
		restored.Who, restored.Subtype, restored.Text = ev.Who, ev.Subtype, ev.Text
		events = append(events, restored)
	}
	return events
}

func meta(rec record) transcript.Meta {
	m := transcript.Meta{Name: rec.Name, Bad: rec.Bad}
	if rec.FirstBad != "" {
		m.FirstBad = errors.New(rec.FirstBad)
	}
	return m
}

func (c *Cache) save(k key, rec record) {
	if c.dir == "" {
		return
	}
	var payload bytes.Buffer
	if gob.NewEncoder(&payload).Encode(rec) != nil || payload.Len() > maxEntry {
		return
	}
	if os.MkdirAll(c.dir, 0700) != nil || !privateDir(c.dir) {
		return
	}
	f, err := os.CreateTemp(c.dir, ".entry-*")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	h := header{Key: k, Bad: rec.Bad, Length: int64(payload.Len()), Sum: sha256.Sum256(payload.Bytes())}
	enc := gob.NewEncoder(f)
	err = enc.Encode(h)
	if err == nil {
		err = enc.Encode(payload.Bytes())
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		_ = os.Rename(f.Name(), c.path(k.Path))
	}
}

// Cleanup occasionally removes obsolete entries, other code versions and excess bytes.
func (c *Cache) Cleanup(now time.Time) {
	if c == nil || c.root == "" || now.UnixNano()%1024 != 0 {
		return
	}
	dirs, err := os.ReadDir(c.root)
	if err != nil {
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
			_ = os.RemoveAll(path)
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			p := filepath.Join(path, entry.Name())
			info, err := entry.Info()
			if err != nil {
				return
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if now.Sub(info.ModTime()) > maxAge {
				_ = os.Remove(p)
				continue
			}
			f, err := os.Open(p)
			if err != nil {
				continue
			}
			var h header
			err = gob.NewDecoder(f).Decode(&h)
			f.Close()
			if err != nil {
				_ = os.Remove(p)
				continue
			}
			if _, err := os.Stat(h.Key.Path); errors.Is(err, os.ErrNotExist) {
				_ = os.Remove(p)
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
