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

// parseFileRow reads one row of the files table (columns in fileColumns order).
func parseFileRow(home string, r []string) (*file, uint64, error) {
	if len(r) != strings.Count(fileColumns, ",")+1 {
		return nil, 0, fmt.Errorf("files row with %d fields", len(r))
	}
	p := &fields{row: r}
	f := &file{id: p.uint(0), name: r[6], size: int64(p.uint(7)), ingested: int64(p.uint(8)), lines: p.int(9), mtime: p.time(10),
		inode: p.uint(11), first: p.time(12), last: p.time(13), bad: p.int(14), tail: p.uint(15), gen: uint32(p.uint(16))}
	ver := p.uint(len(r) - 1) // ver is the last column
	if p.err != nil {
		return nil, 0, fmt.Errorf("files row: %w", p.err)
	}
	f.describe(home, transcript.Source{Path: r[1], ID: r[4], ParentID: r[5]})
	f.evLines, f.acked, f.complete = f.lines, f.ingested, f.ingested
	return f, ver, nil
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
	for _, d := range l.dirs.hot(now, l.cfg.idle, l.held(want)) {
		for _, p := range l.dirs.poll(d, now) {
			want[p] = true
		}
	}
	l.sweepSlice(want)
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
	for _, f := range l.files {
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
	for p, f := range l.files {
		if f.follow == nil {
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

// sweepSlice adds the next known files to want, so that every one is looked at once per sweep period.
func (l *loader) sweepSlice(want map[string]bool) {
	n := len(l.all)
	if n == 0 {
		return
	}
	per := int(int64(n)*int64(l.cfg.tick)/int64(max(l.cfg.sweep, l.cfg.tick))) + 1
	for i := 0; i < per && i < n; i++ {
		l.sweep = (l.sweep + 1) % n
		if f := l.all[l.sweep]; !f.gone {
			want[f.src.Path] = true
		}
	}
}
