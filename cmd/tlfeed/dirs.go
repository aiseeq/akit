package main

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/akit/transcript"
)

// dirs — the folders where transcripts appear. A folder's mtime changes when an entry is created, removed or
// renamed in it, so a folder whose mtime moved is listed again; inotify, when there is one, tells the same at once.
type dirs struct {
	pats   [][]string // segments of transcript.SourcePatterns
	roots  []string
	claude string
	known  map[string]time.Time // folder → mtime when last listed; zero — not stat'ed yet
	recent map[string]time.Time // folders whose mtime moved lately: polled every pass
	w      watcher
	logf   func(string, ...any)
}

func newDirs(home string, logf func(string, ...any)) *dirs {
	d := &dirs{known: map[string]time.Time{}, recent: map[string]time.Time{}, logf: logf}
	for _, p := range transcript.SourcePatterns(home, true, true) {
		d.pats = append(d.pats, strings.Split(p, string(filepath.Separator)))
	}
	d.claude = filepath.Join(home, ".claude", "projects")
	d.roots = []string{d.claude, filepath.Join(home, ".local", "state", "worker")}
	return d
}

// match: full — path is a transcript name; prefix — a folder on the way to one.
func (d *dirs) match(path string) (full, prefix bool) {
	segs := strings.Split(path, string(filepath.Separator))
	for _, p := range d.pats {
		if len(segs) > len(p) {
			continue
		}
		ok := true
		for i, s := range segs {
			if m, err := filepath.Match(p[i], s); err != nil || !m {
				ok = false
				break
			}
		}
		if ok && len(segs) == len(p) {
			full = true
		} else if ok {
			prefix = true
		}
	}
	return full, prefix
}

// addFileDir registers the folders above a known transcript, up to its root; they are not listed now: the
// transcripts in them are already known.
func (d *dirs) addFileDir(path string) {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if _, ok := d.known[dir]; ok {
			break
		}
		if _, prefix := d.match(dir); !prefix {
			break
		}
		// the mtime now: a change after the listing that found the file is seen by the next poll; a folder that
		// cannot be stat'ed stays zero and is listed by the first poll that can
		st, err := os.Stat(dir)
		if err != nil {
			d.failed(err)
			d.known[dir] = time.Time{}
		} else {
			d.known[dir] = st.ModTime()
		}
		if d.watched(dir, path) {
			d.w.add(dir)
		}
	}
}

// watched — folders inotify watches: the roots, the Claude project tree (a few thousand folders: new sessions and
// subagents appear there) and the folder of every transcript (appends to it). The worker tree above the rollout
// folders is watched only for folders that appear while the loader runs.
func (d *dirs) watched(dir, file string) bool {
	return dir == filepath.Dir(file) || strings.HasPrefix(dir+string(filepath.Separator), d.claude+string(filepath.Separator)) || d.isRoot(dir)
}

func (d *dirs) isRoot(dir string) bool { return slices.Contains(d.roots, dir) }

// failed logs a folder that cannot be read; one that is gone is not an error: transcripts come and go.
func (d *dirs) failed(err error) {
	if !errors.Is(err, fs.ErrNotExist) {
		d.logf("%v", err)
	}
}

// glob lists the entries of a pattern below the Claude tree; an error (a home with pattern characters) is logged,
// and the listing of the sources finds the transcripts all the same.
func (d *dirs) glob(pat string) []string {
	found, err := filepath.Glob(filepath.Join(d.claude, pat))
	if err != nil {
		d.logf("list %s: %v", filepath.Join(d.claude, pat), err)
		return nil
	}
	return found
}

// watchClaude watches the folders of the Claude project tree that exist now.
func (d *dirs) watchClaude() {
	for _, pat := range []string{"*", filepath.Join("*", "*"), filepath.Join("*", "*", "subagents")} {
		for _, p := range d.glob(pat) {
			if _, ok := d.known[p]; ok {
				continue
			}
			st, err := os.Stat(p)
			if err != nil {
				d.failed(err)
				continue
			}
			if st.IsDir() {
				d.known[p] = st.ModTime()
				d.w.add(p)
			}
		}
	}
	for _, r := range d.roots {
		if _, ok := d.known[r]; !ok {
			d.known[r] = time.Time{}
		}
		d.w.add(r)
	}
}

// poll stats a folder; when its mtime moved since it was last listed, it is listed again and the transcripts in it
// are returned. A folder seen the first time is only stat'ed.
func (d *dirs) poll(dir string, now time.Time) []string {
	st, err := os.Stat(dir)
	if err != nil {
		d.failed(err)
		delete(d.recent, dir)
		return nil
	}
	if !st.IsDir() {
		delete(d.recent, dir)
		return nil
	}
	was, ok := d.known[dir]
	d.known[dir] = st.ModTime()
	if ok && was.Equal(st.ModTime()) || ok && was.IsZero() {
		return nil
	}
	if now.Sub(st.ModTime()) < time.Hour {
		d.recent[dir] = st.ModTime()
	}
	return d.scan(dir, now)
}

// scan lists a folder: its transcripts, and those in its subfolders on the way to transcripts that were not known.
func (d *dirs) scan(dir string, now time.Time) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		d.failed(err)
		return nil
	}
	var out []string
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		full, prefix := d.match(p)
		switch {
		case e.IsDir() && prefix:
			if _, ok := d.known[p]; !ok {
				out = append(out, d.appeared(p, now)...)
			}
		case !e.IsDir() && full:
			out = append(out, p)
		}
	}
	return out
}

// appeared — a new folder on the way to transcripts: watched and listed now, as its entries may be there before
// the watch.
func (d *dirs) appeared(dir string, now time.Time) []string {
	st, err := os.Stat(dir)
	if err != nil {
		d.failed(err)
		return nil
	}
	d.known[dir] = st.ModTime()
	d.recent[dir] = st.ModTime()
	d.w.add(dir)
	return d.scan(dir, now)
}

// event turns one inotify event into transcripts to look at.
func (d *dirs) event(e watchEvent, now time.Time) []string {
	p := filepath.Join(e.dir, e.name)
	full, prefix := d.match(p)
	if e.isDir {
		if _, ok := d.known[p]; prefix && !ok {
			return d.appeared(p, now)
		}
		return nil
	}
	if full {
		return []string{p}
	}
	return nil
}

// hot — folders polled every pass: the roots, those whose mtime moved within idle, and the extra ones the
// caller names (folders of active transcripts). Without inotify the Claude project folders are polled too, so a
// new session in a quiet project is found within a pass.
func (d *dirs) hot(now time.Time, idle time.Duration, extra []string) []string {
	out := append([]string{}, d.roots...)
	for _, dir := range slices.Sorted(maps.Keys(d.recent)) {
		if now.Sub(d.recent[dir]) > idle {
			delete(d.recent, dir)
			continue
		}
		out = append(out, dir)
	}
	out = append(out, extra...)
	if !d.w.active() {
		out = append(out, d.glob("*")...)
	}
	return out
}
