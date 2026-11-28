package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// runReports indexes and searches the reports under dir with the cache, and returns line or error per report.
func runReports(t *testing.T, dir string, words ...string) map[string]runFileResult {
	t.Helper()
	q, err := newQuery(words)
	if err != nil {
		t.Fatal(err)
	}
	files, errs := runMarkdownFiles(dir)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	got := make(map[string]runFileResult)
	for i, result := range matchRunFilesCached(q, files) {
		got[files[i]] = result
	}
	return got
}

// A report the index rules out is still reported when it can no longer be read, as an uncached read would.
func TestUnreadableRejectedReportReportsError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads any file")
	}
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	dir := t.TempDir()
	path := filepath.Join(dir, "2026-01-01", "a.md")
	write(t, path, "nothing relevant here\n")
	runReports(t, dir, "needle")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	got := runReports(t, dir, "needle")[path]
	want := "отчёт не прочитался: open " + path + ": permission denied"
	if got.err == nil || got.err.Error() != want {
		t.Fatalf("unreadable report: %+v, want error %q", got, want)
	}
}

// Two reports with the same content share one read. When the report being read changes meanwhile, the line
// found in its new content must not be handed to the other report.
func TestReportChangedDuringReadKeepsItsResult(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, path string)
		want   string
	}{
		{"append", func(t *testing.T, path string) {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString("word\n"); err != nil {
				t.Fatal(err)
			}
		}, "word"},
		{"same size rewrite", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("word or\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(path, later, later); err != nil {
				t.Fatal(err)
			}
		}, "word or"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
			dir := t.TempDir()
			a, b := filepath.Join(dir, "d", "a.md"), filepath.Join(dir, "d", "b.md")
			for _, path := range []string{a, b} {
				write(t, path, "wor ord\n") // the bloom passes "word", the text has no match
			}
			runReports(t, dir, "word")
			var once sync.Once
			changed := ""
			runEntryReadHook = func(path string) {
				once.Do(func() {
					changed = path
					tc.change(t, path)
				})
			}
			t.Cleanup(func() { runEntryReadHook = nil })
			got := runReports(t, dir, "word")
			runEntryReadHook = nil
			if changed == "" {
				t.Fatal("no report was read")
			}
			for _, path := range []string{a, b} {
				want := ""
				if path == changed {
					want = tc.want
				}
				if got[path].err != nil || got[path].line != want {
					t.Errorf("%s: %+v, want line %q", filepath.Base(path), got[path], want)
				}
			}
			if again := runReports(t, dir, "word"); !mapsEqualResults(again, got) {
				t.Errorf("next call %+v, want %+v", again, got)
			}
		})
	}
}

func mapsEqualResults(a, b map[string]runFileResult) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w, ok := b[k]
		if !ok || v.line != w.line || (v.err == nil) != (w.err == nil) {
			return false
		}
	}
	return true
}

// A report indexed without the word is found once the word appears, however the file was changed.
func TestRunIndexNoticesWordAddedLater(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, path string, mtime time.Time)
	}{
		{"append", func(t *testing.T, path string, _ time.Time) {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString("needle\n"); err != nil {
				t.Fatal(err)
			}
		}},
		{"same size, new mtime", func(t *testing.T, path string, mtime time.Time) {
			if err := os.WriteFile(path, []byte("needle\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			later := mtime.Add(time.Second)
			if err := os.Chtimes(path, later, later); err != nil {
				t.Fatal(err)
			}
		}},
		{"replaced, same size and mtime", func(t *testing.T, path string, mtime time.Time) {
			tmp := path + ".tmp"
			if err := os.WriteFile(tmp, []byte("needle\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(tmp, mtime, mtime); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
			dir := t.TempDir()
			path := filepath.Join(dir, "d", "a.md")
			write(t, path, "zzzzzz\n")
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := runReports(t, dir, "needle")[path]; got.line != "" || got.err != nil {
				t.Fatalf("before the change: %+v", got)
			}
			tc.change(t, path, info.ModTime())
			if got := runReports(t, dir, "needle")[path]; got.line != "needle" || got.err != nil {
				t.Fatalf("after the change: %+v, want line %q", got, "needle")
			}
		})
	}
}

// A directory larger than one getdents buffer is listed in full.
func TestRunMarkdownFilesListsLargeDirectory(t *testing.T) {
	dir := t.TempDir()
	day := filepath.Join(dir, "2026-01-01")
	if err := os.Mkdir(day, 0o700); err != nil {
		t.Fatal(err)
	}
	var want []string
	for i := range 3000 {
		path := filepath.Join(day, fmt.Sprintf("%04d-%s.md", i, strings.Repeat("x", 100)))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		want = append(want, path)
	}
	got, errs := runMarkdownFiles(dir)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("listed %d reports, want %d", len(got), len(want))
	}
}

// Old cache layouts are removed by the first call that writes the current index, and only then: an older
// build working next to this one may write its files again without either build deleting the other's work
// on every call.
func TestOldLayoutCacheFilesRemovedOnce(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "cache")
	t.Setenv("AKIT_PRIOR_CACHE_DIR", cache)
	hash := strings.Repeat("0a", 32)
	legacy := []string{
		"run-" + hash + ".bin", "replies-" + hash + ".bin", "runs-index-v2.json", "runs-index-v3.bin",
		"runs-index-v12.bin", "runs-index-v13.bin", "runs-v1.gob", "runs-data-v1.bin",
	}
	kept := []string{
		"transcript-" + hash + ".json", "transcripts-manifest.json", "reply-prewarm.json", "run-notes.bin",
		"run-" + hash + ".txt", "replies-" + hash[:10] + ".bin", "runs-index-v1.bin", "runs-index-v99.bin", "other.bin",
	}
	for _, name := range append(slices.Clone(legacy), kept...) {
		write(t, filepath.Join(cache, name), "x")
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "d", "a.md"), "text\n")
	runReports(t, dir, "needle")
	for _, name := range legacy {
		if _, err := os.Stat(filepath.Join(cache, name)); !os.IsNotExist(err) {
			t.Errorf("%s was not removed: %v", name, err)
		}
	}
	for _, name := range kept {
		if _, err := os.Stat(filepath.Join(cache, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
	if _, err := os.Stat(cachePath(runIndexName)); err != nil {
		t.Fatalf("no index: %v", err)
	}
	old := filepath.Join(cache, legacy[0])
	write(t, old, "x")
	write(t, filepath.Join(dir, "d", "b.md"), "more\n") // the index is written again
	runReports(t, dir, "needle")
	if _, err := os.Stat(old); err != nil {
		t.Errorf("a file written after the first cleanup was removed: %v", err)
	}
}

// Loading the index does not build an entry per report: its cost stays a single read as reports accumulate.
func TestRunIndexLoadDoesNotAllocatePerReport(t *testing.T) {
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	dir := t.TempDir()
	for i := range 2000 {
		write(t, filepath.Join(dir, "d", fmt.Sprintf("%04d.md", i)), fmt.Sprintf("report %d\n", i))
	}
	runReports(t, dir, "needle")
	allocs := testing.AllocsPerRun(5, func() {
		if _, err := loadRunIndex(); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 100 {
		t.Fatalf("loading an index of 2000 reports allocates %.0f times", allocs)
	}
	got := runReports(t, dir, "report")
	if len(got) != 2000 {
		t.Fatalf("%d results", len(got))
	}
	for path, result := range got {
		if result.err != nil || !strings.HasPrefix(result.line, "report ") {
			t.Fatalf("%s: %+v", path, result)
		}
	}
}

// direntRecord encodes one linux_dirent64 record padded to 8 bytes.
func direntRecord(inode uint64, typ byte, name string) []byte {
	size := (19 + len(name) + 1 + 7) &^ 7
	record := make([]byte, size)
	binary.NativeEndian.PutUint64(record[:8], inode)
	binary.NativeEndian.PutUint16(record[16:18], uint16(size))
	record[18] = typ
	copy(record[19:], name)
	return record
}

func TestAppendDirentsTypes(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "unknown-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "unknown.md"), "")
	var buf []byte
	for _, record := range []struct {
		inode uint64
		typ   byte
		name  string
	}{
		{1, syscall.DT_REG, "a.md"}, {2, syscall.DT_DIR, "day"}, {3, syscall.DT_LNK, "link"},
		{0, syscall.DT_REG, "removed.md"}, {4, syscall.DT_DIR, "."}, {5, syscall.DT_DIR, ".."},
		{6, syscall.DT_UNKNOWN, "unknown-dir"}, {7, syscall.DT_UNKNOWN, "unknown.md"}, {8, syscall.DT_UNKNOWN, "gone"},
	} {
		buf = append(buf, direntRecord(record.inode, record.typ, record.name)...)
	}
	got, errs := appendDirents(nil, dir, buf)
	want := []dirEntry{{"a.md", false}, {"day", true}, {"link", false}, {"unknown-dir", true}, {"unknown.md", false}}
	if len(errs) != 0 || !slices.Equal(got, want) {
		t.Fatalf("entries %+v, errors %v; want %+v", got, errs, want)
	}
}

// A record whose bloom fails its checksum is treated as absent: the report is read and the index rewritten.
func TestRunIndexDamagedRecordIsRead(t *testing.T) {
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	dir := t.TempDir()
	path := filepath.Join(dir, "d", "a.md")
	write(t, path, "needle here\n")
	runReports(t, dir, "needle")
	data, err := os.ReadFile(cachePath(runIndexName))
	if err != nil {
		t.Fatal(err)
	}
	data[16+runIndexKeyBytes+runIndexMetadataBytes] ^= 0xff
	if err := os.WriteFile(cachePath(runIndexName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := loadRunIndex()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.lookup(path); ok {
		t.Fatal("a damaged record was used")
	}
	if got := runReports(t, dir, "needle")[path]; got.line != "needle here" || got.err != nil {
		t.Fatalf("%+v", got)
	}
	if index, err = loadRunIndex(); err != nil {
		t.Fatal(err)
	}
	if _, ok := index.lookup(path); !ok {
		t.Fatal("the record was not rewritten")
	}
}

// A report that grows after it was read for a new index entry is not indexed with the shorter text: the next
// call reads it again and finds the appended word.
func TestReportGrownAfterReadIsNotIndexed(t *testing.T) {
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	dir := t.TempDir()
	path := filepath.Join(dir, "d", "a.md")
	write(t, path, "zzzzzz\n")
	var once sync.Once
	runSourceReadHook = func(got string) {
		once.Do(func() {
			f, err := os.OpenFile(got, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			if _, err := f.WriteString("needle\n"); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(func() { runSourceReadHook = nil })
	if got := runReports(t, dir, "needle")[path]; got.line != "" || got.err != nil {
		t.Fatalf("first call: %+v", got)
	}
	runSourceReadHook = nil
	if got := runReports(t, dir, "needle")[path]; got.line != "needle" || got.err != nil {
		t.Fatalf("after the report grew: %+v, want line %q", got, "needle")
	}
}

// A report that is not a regular file is read as it is and never indexed.
func TestIrregularReportIsNotIndexed(t *testing.T) {
	t.Setenv("AKIT_PRIOR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	dir := t.TempDir()
	path := filepath.Join(dir, "d", "device.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(os.DevNull, path); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "d", "a.md"), "needle\n")
	if got := runReports(t, dir, "needle")[path]; got.line != "" || got.err != nil {
		t.Fatalf("%+v", got)
	}
	index, err := loadRunIndex()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.lookup(path); ok {
		t.Fatal("a device was indexed")
	}
}
