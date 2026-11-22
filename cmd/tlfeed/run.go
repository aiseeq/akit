package main

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aiseeq/akit/transcript"
)

// fileCol — the position of each column of fileColumns.
var fileCol = func() map[string]int {
	m := map[string]int{}
	for i, name := range strings.Split(fileColumns, ",") {
		m[strings.TrimSpace(name)] = i
	}
	return m
}()

// parseFileRow reads one row of the files table (columns in fileColumns order); dirty — rows past its lines may
// be in the tables.
func parseFileRow(home string, r []string) (f *file, ver uint64, dirty bool, err error) {
	if len(r) != strings.Count(fileColumns, ",")+1 {
		return nil, 0, false, fmt.Errorf("files row with %d fields", len(r))
	}
	p := &fields{row: r}
	c := func(name string) int { return fileCol[name] }
	f = &file{id: p.uint(c("file_id")), name: r[c("name")], size: int64(p.uint(c("size"))), ingested: int64(p.uint(c("ingested"))),
		lines: p.int(c("lines")), evLines: p.int(c("ev_lines")), mtime: p.time(c("mtime")), inode: p.uint(c("inode")),
		first: p.time(c("first_ts")), last: p.time(c("last_ts")), bad: p.int(c("bad")), tail: p.uint(c("tail")),
		gen: uint32(p.uint(c("gen")))}
	dirty = p.uint(c("dirty")) != 0
	ver = p.uint(c("ver"))
	if p.err != nil {
		return nil, 0, false, fmt.Errorf("files row: %w", p.err)
	}
	f.describe(home, transcript.Source{Path: r[c("path")], ID: r[c("session")], ParentID: r[c("parent")]})
	f.complete = f.ingested
	f.commit()
	return f, ver, dirty, nil
}

// start brings the loader to the state of the tables and lists every transcript; it returns the paths to look at.
func (l *loader) start() ([]string, error) {
	if err := l.ensureSchema(); err != nil {
		return nil, err
	}
	if err := l.loadKnown(); err != nil {
		return nil, err
	}
	paths, err := l.listAll()
	if err != nil {
		return nil, err
	}
	l.dirs.watchClaude()
	seen := map[string]bool{}
	for _, p := range paths {
		seen[p] = true
	}
	for _, f := range l.all {
		if !seen[f.src.Path] {
			paths = append(paths, f.src.Path) // known but not listed: gone, or renamed while the loader was down
		}
	}
	return paths, nil
}

// once loads everything there is and returns.
func (l *loader) once() error {
	paths, err := l.start()
	if err != nil {
		return err
	}
	l.logf("first pass over %d files (%d known in %s.files)", len(paths), len(l.all), l.cfg.db)
	err = l.visitAll(paths)
	l.report()
	return err
}

// follow loads new lines every tick until stopped.
func (l *loader) follow() error {
	paths, err := l.start()
	if err != nil {
		return err
	}
	l.logf("following %d files (%d known in %s.files), inotify %t", len(paths), len(l.all), l.cfg.db, l.dirs.w.active())
	if err := l.visitAll(paths); err != nil {
		return err
	}
	l.report()
	next := l.now().Add(l.cfg.rescan)
	t := time.NewTicker(l.cfg.tick)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return nil
		case <-t.C:
		}
		full := false
		if !l.now().Before(next) {
			full, next = true, l.now().Add(l.cfg.rescan)
		}
		if err := l.pass(full); err != nil {
			return err
		}
	}
}

// recentDirs — how long a folder whose mtime moved stays polled every pass.
const recentDirs = 15 * time.Minute

// pass looks at what may have changed: watched and recently changed folders, files with a parser, a slice of the
// rest (the sweep) and, when full, the whole listing; then it releases the parsers of files that went quiet.
func (l *loader) pass(full bool) error {
	now := l.now()
	want := map[string]bool{}
	events, lost := l.dirs.w.drain()
	for _, e := range events {
		for _, p := range l.dirs.event(e, now) {
			want[p] = true
		}
	}
	for _, d := range l.dirs.hot(now, min(l.cfg.idle, recentDirs), l.held(want)) {
		for _, p := range l.dirs.poll(d, now) {
			want[p] = true
		}
	}
	l.sweepSlice(want, now)
	if full || lost {
		if paths, err := l.listAll(); err != nil {
			l.logf("list transcripts: %v", err)
		} else {
			for _, p := range paths {
				want[p] = true
			}
		}
	}
	if err := l.visitAll(slices.Sorted(maps.Keys(want))); err != nil {
		return err
	}
	for f := range l.parsers {
		if f.follow != nil && now.Sub(f.grown) > l.cfg.idle {
			f.follow, f.parsed = nil, 0
		}
	}
	l.report()
	return nil
}

// held adds the files with a parser to want and returns the folders where their growth and their subagents show.
func (l *loader) held(want map[string]bool) []string {
	var extra []string
	for f := range l.parsers {
		p := f.src.Path
		if f.follow == nil || l.files[p] != f {
			delete(l.parsers, f)
			continue
		}
		want[p] = true
		extra = append(extra, filepath.Dir(p))
		if f.kind == "claude" {
			extra = append(extra, strings.TrimSuffix(p, ".jsonl"), filepath.Join(strings.TrimSuffix(p, ".jsonl"), "subagents"))
		}
	}
	sort.Strings(extra)
	return extra
}

// sweepSlice adds the next known files and folders to want, so that every file is looked at once per sweep period;
// the folders too, or once per rescan period when inotify watches every one of them.
func (l *loader) sweepSlice(want map[string]bool, now time.Time) {
	part := func(n int, period time.Duration) int {
		return int(int64(n)*int64(l.cfg.tick)/int64(max(period, l.cfg.tick))) + 1
	}
	period := l.cfg.sweep
	if l.dirs.w.complete() {
		period = max(l.cfg.rescan, l.cfg.sweep)
	}
	for _, p := range l.dirs.sweep(part(len(l.dirs.order), period), now) {
		want[p] = true
	}
	n := len(l.all)
	for i := 0; i < part(n, l.cfg.sweep) && i < n; i++ {
		l.sweep = (l.sweep + 1) % n
		if f := l.all[l.sweep]; !f.gone {
			want[f.src.Path] = true
		}
	}
}
