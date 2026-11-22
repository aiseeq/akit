package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aiseeq/akit/transcript"
)

// config — what the loader is told by its flags.
type config struct {
	home, db, state string
	once            bool
	tick, idle      time.Duration // pass period; a file that has not grown this long releases its parser
	rescan, sweep   time.Duration // full listing period; every known file and folder is looked at once per sweep
	batch           int           // raw bytes per insert
	inotify         bool
}

// loader keeps the files in the tables in step with the files on disk.
type loader struct {
	cfg   config
	srv   *server
	stop  <-chan struct{}
	log   io.Writer
	now   func() time.Time
	files map[string]*file // by path
	all   []*file          // every file known, gone ones too: ids, renames, the sweep
	// parsers — the files that got a parser; one that has let it go stays until the next pass looks
	parsers map[*file]bool
	ids     map[uint64]bool
	dirs    *dirs
	st      *status
	batch   batch
	buf     []byte
	ver     uint64
	sweep   int
	// errText is the last error of a retried call; it is cleared when a call succeeds
	errText string
	backoff time.Duration // first wait of a retry
	// totals of this run, for the --once summary
	rawRows, evRows int
	rawBytes        int64
}

func newLoader(cfg config, srv *server, stop <-chan struct{}, log io.Writer, st *status) *loader {
	l := &loader{cfg: cfg, srv: srv, stop: stop, log: log, now: time.Now, files: map[string]*file{}, parsers: map[*file]bool{}, ids: map[uint64]bool{}, st: st,
		backoff: time.Second}
	l.dirs = newDirs(cfg.home, l.logf)
	return l
}

func (l *loader) logf(format string, args ...any) {
	fmt.Fprintf(l.log, "tlfeed: "+format+"\n", args...)
}

// retry runs fn until it succeeds or the loader is stopped: a server that is down is waited for, not given up on.
func (l *loader) retry(what string, fn func() error) error {
	wait := l.backoff
	for {
		err := fn()
		if err == nil {
			if l.errText != "" {
				l.logf("%s: ok again", what)
				l.errText = ""
				l.st.setError("")
			}
			return nil
		}
		text := fmt.Sprintf("%s: %v", what, err)
		if text != l.errText {
			l.logf("%s (retrying)", text)
		}
		l.errText = text
		l.st.setError(text)
		l.report() // the state file shows the lag growing while the server is away
		select {
		case <-l.stop:
			return errStopped
		case <-time.After(wait):
		}
		wait = min(wait*2, 30*time.Second)
	}
}

func (l *loader) exec(what, q string) error {
	return l.retry(what, func() error {
		_, err := l.srv.query(context.Background(), q, 10*time.Minute)
		return err
	})
}

// ensureSchema creates the tables if they are not there.
func (l *loader) ensureSchema() error {
	for _, q := range statements(l.cfg.db) {
		if err := l.exec("create tables", q); err != nil {
			return err
		}
	}
	return nil
}

// loadKnown reads the state of every file from the files table. A file whose latest row is dirty was being sent
// when the loader stopped: rows past its stored lines may be in raw and events, in part; they are deleted and
// loaded again.
func (l *loader) loadKnown() error {
	var rows [][]string
	if err := l.retry("read files", func() error {
		data, err := l.srv.query(context.Background(), "SELECT "+fileColumns+" FROM "+l.cfg.db+".files FINAL FORMAT TabSeparated", 10*time.Minute)
		rows = tsvRows(data)
		return err
	}); err != nil {
		return err
	}
	var dirty []*file
	for _, r := range rows {
		f, ver, d, err := parseFileRow(l.cfg.home, r)
		if err != nil {
			return err
		}
		l.ver = max(l.ver, ver)
		l.add(f)
		if d {
			dirty = append(dirty, f)
		}
	}
	for _, f := range dirty {
		if err := l.truncate(f, fmt.Sprintf("stopped while sending lines past %d", f.lines)); err != nil {
			return err
		}
	}
	return nil
}

func (l *loader) add(f *file) {
	l.all = append(l.all, f)
	if f.id != 0 {
		l.ids[f.id] = true
	}
	if !f.gone {
		l.files[f.src.Path] = f
		l.dirs.addFileDir(f.src.Path)
	}
}

// listAll — every transcript tl --all --codex reads, with their folders.
func (l *loader) listAll() ([]string, error) {
	srcs, err := transcript.AllSources(l.cfg.home, time.Time{}, "", true, true)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, s.Path)
		l.dirs.addFileDir(s.Path)
	}
	return out, nil
}

// visit looks at one path and loads what is new in it.
func (l *loader) visit(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		l.fileErr(path, err)
		l.vanished(path)
		return nil
	}
	if !st.Mode().IsRegular() {
		l.vanished(path)
		return nil
	}
	f, err := l.lookup(path, st)
	if err != nil {
		l.logf("%v", err)
		return nil
	}
	if st.Size() == f.size && st.ModTime().Unix() == f.mtime.Unix() && inodeOf(st) == f.inode && f.complete <= f.ingested && f.evLines >= f.lines && !f.stale {
		return nil
	}
	if err := l.checkTail(f, st.Size()); err != nil {
		return err
	}
	changed := f.inode != inodeOf(st) || f.mtime.Unix() != st.ModTime().Unix() || f.size != st.Size()
	f.size, f.mtime, f.inode = st.Size(), st.ModTime(), inodeOf(st)
	if changed && f.id != 0 {
		l.batch.touch(f) // the file row records the new size, time and inode
	}
	return l.load(f)
}

// fileErr logs a transcript that cannot be read; one that is gone is not an error: transcripts come and go.
func (l *loader) fileErr(path string, err error) {
	if !errors.Is(err, fs.ErrNotExist) {
		l.logf("%s: %v", path, err)
	}
}

// vanished — the path is no longer a file: its parser goes, the file waits to come back or to be found renamed.
func (l *loader) vanished(path string) {
	if f := l.files[path]; f != nil {
		f.gone, f.follow, f.parsed, f.complete = true, nil, 0, f.ack.ingested
		delete(l.files, path)
	}
}

// lookup — the file followed at path: known there, renamed to it, or new.
func (l *loader) lookup(path string, st os.FileInfo) (*file, error) {
	f := l.files[path]
	if f == nil {
		src, err := transcript.SourceAt(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if f = l.renamed(src, inodeOf(st), st.Size()); f == nil {
			f = &file{}
			f.describe(l.cfg.home, src)
			l.add(f)
		}
	}
	if f.gone {
		f.gone = false
		l.files[path] = f
	}
	return f, nil
}

// checkTail compares the loaded bytes with the file: shorter than loaded, or the last loaded bytes changed — the
// file is loaded again.
func (l *loader) checkTail(f *file, size int64) error {
	if f.ingested == 0 {
		return nil
	}
	sum, ok, err := tailAt(f.src.Path, f.ingested)
	if err != nil {
		l.fileErr(f.src.Path, err) // gone between stat and read, or a read error: the next pass looks again
		f.stale = true
		return nil
	}
	if ok && sum == f.tail {
		return nil
	}
	why := "loaded bytes changed"
	if !ok {
		why = fmt.Sprintf("size %d below loaded %d", size, f.ingested)
	}
	return l.rewind(f, why)
}

// renamed finds a known file that now lives at src: the same inode, its old path gone, and the loaded bytes
// still there. A new file that got the inode of a deleted one does not have those bytes and gets its own id.
// The same goes for a deleted file that comes back at its path (rebuilt from raw): with its loaded bytes it keeps
// its id, without them it is a new file and the old rows stay.
func (l *loader) renamed(src transcript.Source, ino uint64, size int64) *file {
	path := src.Path
	for _, f := range l.all {
		if f.ingested == 0 || size < f.ingested {
			continue
		}
		if f.src.Path == path {
			if !f.gone {
				continue
			}
		} else if f.inode != ino || l.alive(f.src.Path, ino) {
			continue
		}
		sum, ok, err := tailAt(path, f.ingested)
		if err != nil {
			l.fileErr(path, err)
			return nil
		}
		if !ok || sum != f.tail {
			continue
		}
		if f.src.Path == path {
			l.logf("%s: back, id %d kept", path, f.id)
		} else {
			l.logf("%s: renamed from %s, id %d kept", path, f.src.Path, f.id)
		}
		if l.files[f.src.Path] == f {
			delete(l.files, f.src.Path)
		}
		f.describe(l.cfg.home, src)
		f.gone = false
		l.files[path] = f
		l.dirs.addFileDir(path)
		l.batch.touch(f)
		return f
	}
	return nil
}

// alive: the old path still has the inode — a second link, both paths are files of their own. An old path that
// cannot be stat'ed for another reason than being gone counts as alive: the new path then gets its own id.
func (l *loader) alive(old string, ino uint64) bool {
	st, err := os.Stat(old)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		l.logf("%s: %v", old, err)
		return true
	}
	return inodeOf(st) == ino
}

// rewind — the file is shorter than what is loaded, or the loaded bytes changed: its rows go and it is loaded
// again.
func (l *loader) rewind(f *file, why string) error {
	if err := l.flush(); err != nil {
		return err
	}
	f.lines, f.evLines, f.ingested, f.complete, f.tail = 0, 0, 0, 0, 0
	f.first, f.last, f.bad, f.badBase, f.name, f.follow, f.parsed = time.Time{}, time.Time{}, 0, 0, "", nil, 0
	return l.truncate(f, "rewind ("+why+")")
}

// reloadEvents — a rollout began to write message events, so the events of its earlier lines change (see
// transcript.ErrFollowRestart): they are deleted and parsed again from the first line; raw stays.
func (l *loader) reloadEvents(f *file) error {
	if err := l.flush(); err != nil {
		return err
	}
	f.evLines, f.follow, f.parsed, f.first, f.last, f.badBase = 0, nil, 0, time.Time{}, time.Time{}, 0
	return l.truncate(f, "message events appeared")
}

// truncate makes the tables hold the file up to its lines (raw) and evLines (events) and no more. The rows past
// them are deleted under a dirty files row of a new generation: the tokens of the next inserts differ from those
// of the deleted rows, which the server still remembers, and a loader stopped in the middle does it again at its
// next start. usage has rows merged over the parts of a reply, so the file's rows there are made again from events.
func (l *loader) truncate(f *file, why string) error {
	l.logf("%s: %s: rows of id %d past line %d (events past %d) deleted", f.src.Path, why, f.id, f.lines, f.evLines)
	if err := l.flush(); err != nil {
		return err
	}
	f.gen++
	if err := l.storeFile(f, true); err != nil {
		return err
	}
	db := l.cfg.db
	for _, q := range []string{
		fmt.Sprintf("DELETE FROM %s.events WHERE file_id = %d AND line > %d", db, f.id, f.evLines),
		fmt.Sprintf("DELETE FROM %s.replies WHERE file_id = %d AND line > %d", db, f.id, f.evLines),
		fmt.Sprintf("DELETE FROM %s.usage WHERE file_id = %d", db, f.id),
		fmt.Sprintf("DELETE FROM %s.raw WHERE file_id = %d AND line > %d", db, f.id, f.lines),
	} {
		if err := l.exec("delete rows past the stored lines", q); err != nil {
			return err
		}
	}
	if f.evLines > 0 {
		q := fmt.Sprintf("INSERT INTO %s.usage (%s) SELECT %s FROM %s.events WHERE file_id = %d AND reply_id != ''",
			db, usageColumns, usageColumns, db, f.id)
		if err := l.retry("usage of the kept events", func() error {
			settings := url.Values{"insert_deduplication_token": {fmt.Sprintf("usage:%d:g%d", f.id, f.gen)}, "async_insert": {"0"}}
			return l.srv.post(context.Background(), q, nil, settings, nil, 10*time.Minute)
		}); err != nil {
			return err
		}
	}
	if err := l.storeFile(f, false); err != nil {
		return err
	}
	f.commit()
	return nil
}

// storeFile writes the row of one file now, outside a batch; its size is 0, so a start before the next batch
// looks at the file again.
func (l *loader) storeFile(f *file, dirty bool) error {
	if f.id == 0 {
		return nil
	}
	var b bytes.Buffer
	c := *f
	c.size = 0
	ver := l.nextVer()
	fileRow(&b, &c, ver, dirty)
	return l.insert("files", fileColumns, &b, fmt.Sprintf("files:v%d", ver))
}

func (l *loader) nextVer() uint64 {
	l.ver = max(l.ver+1, uint64(l.now().UnixNano()))
	return l.ver
}

// load reads the complete lines of the file past what is loaded, parses them and adds their rows to the batch.
func (l *loader) load(f *file) error {
	if f.size <= f.ingested && f.evLines >= f.lines {
		f.complete = f.ingested
		return nil
	}
	f.grown = l.now()
	for {
		err := l.loadOnce(f)
		if !errors.Is(err, transcript.ErrFollowRestart) {
			return err
		}
		if err := l.reloadEvents(f); err != nil {
			return err
		}
	}
}

func (l *loader) loadOnce(f *file) error {
	path, size := f.src.Path, f.size
	if f.follow == nil && f.evLines >= f.lines {
		has, err := lineAfter(path, f.ingested, size)
		if err != nil {
			l.fileErr(path, err)
			f.stale = true
			return nil
		}
		if !has { // only an incomplete line was added: no parser is built for it
			f.complete = f.ingested
			return nil
		}
	}
	if f.follow == nil {
		// a new parser starts at line 1; it learns from all lines it will get which events the file writes, and
		// it only keeps the state over the lines whose events are already in the table
		f.follow, f.parsed, f.badBase = transcript.NewFollow(), 0, f.bad
		l.parsers[f] = true
		if _, err := pieces(path, 0, size, l.cfg.batch, &l.buf, func(data []byte, _ int64) error {
			return f.follow.Prescan(data)
		}); err != nil {
			l.fileErr(path, err)
			f.follow, f.stale = nil, true
			return nil // unreadable now: the next pass tries again
		}
		if err := f.follow.SkipThrough(f.evLines); err != nil {
			return err
		}
		if f.evLines == 0 {
			f.badBase = 0
		}
	}
	end, err := pieces(path, f.parsed, size, l.cfg.batch, &l.buf, func(data []byte, at int64) error {
		return l.feed(f, data, at)
	})
	if err != nil {
		if errors.Is(err, transcript.ErrFollowRestart) || errors.Is(err, errStopped) {
			return err
		}
		l.logf("%s: %v", path, err)
		f.follow, f.parsed, f.stale = nil, 0, true
		return nil
	}
	f.complete = max(end, f.ingested)
	f.stale = false
	return nil
}

// feed parses one piece of complete lines that starts at byte at; raw rows go for lines past f.lines and event
// rows for lines past f.evLines, so a parser that reads the file again from the start sends nothing twice.
func (l *loader) feed(f *file, data []byte, at int64) error {
	first := f.follow.Lines() + 1
	var evs []transcript.Event
	err := f.follow.Feed(data, func(e transcript.Event) {
		if e.Line > f.evLines {
			evs = append(evs, e)
		}
	})
	if err != nil {
		return err
	}
	f.parsed = at + int64(len(data))
	m := f.follow.Meta()
	f.name, f.bad = m.Name, f.badBase+m.Bad
	if f.id == 0 {
		// the first event with a time in the lines seen so far; none yet (a file cut before its first event) — mtime
		start := f.mtime
		for i := range evs {
			if !evs[i].Time.IsZero() {
				start = evs[i].Time
				break
			}
		}
		f.id = newID(f.src.Path, start, func(id uint64) bool { return l.ids[id] })
		l.ids[f.id] = true
	}
	b := &l.batch
	b.touch(f)
	line, off := first, at
	for rest := data; len(rest) > 0; line++ {
		raw, next, _ := bytes.Cut(rest, []byte{'\n'})
		if line > f.lines {
			b.rawPart(f, line)
			rawRow(&b.raw, f.id, line, off, raw)
			b.rawRows++
			b.rawBytes += int64(len(raw) + 1)
		}
		off += int64(len(raw) + 1)
		rest = next
	}
	for i := range evs {
		e := &evs[i]
		b.evPart(f, e.Line)
		eventRow(&b.events, f, e)
		b.evRows++
		if !e.Time.IsZero() && (f.first.IsZero() || e.Time.Before(f.first)) {
			f.first = e.Time
		}
		if e.Time.After(f.last) {
			f.last = e.Time
		}
	}
	if end := at + int64(len(data)); end > f.ingested {
		f.lines, f.ingested = line-1, end
		if len(data) >= tailBytes {
			f.tail = tailSum(data[len(data)-tailBytes:])
		} else {
			// a short piece: the sum takes bytes before it from the file; unreadable now — the sum stays wrong
			// and the next look at the file loads it again
			sum, _, err := tailAt(f.src.Path, end)
			if err != nil {
				l.fileErr(f.src.Path, err)
			}
			f.tail = sum
		}
	}
	f.evLines = max(f.evLines, line-1)
	if b.raw.Len()+b.events.Len() >= l.cfg.batch {
		return l.flush()
	}
	return nil
}

// batch — the rows of one insert per table, with the files they came from.
type batch struct {
	raw, events     bytes.Buffer
	rawRows, evRows int
	rawBytes        int64
	touched         []*file
	seen            map[*file]bool
	rawTok, evTok   []string
	rawSeen, evSeen map[*file]bool
}

func (b *batch) touch(f *file) {
	if b.seen == nil {
		b.seen, b.rawSeen, b.evSeen = map[*file]bool{}, map[*file]bool{}, map[*file]bool{}
	}
	if !b.seen[f] {
		b.seen[f] = true
		b.touched = append(b.touched, f)
	}
}

// part of the deduplication token: the file and the first line it has in this insert; after a rewind, its
// generation, so the lines loaded again are not taken for a repeat of the deleted ones
func part(f *file, line int) string {
	if f.gen == 0 {
		return fmt.Sprintf("%d:%d", f.id, line)
	}
	return fmt.Sprintf("%d:%d:g%d", f.id, line, f.gen)
}

func (b *batch) rawPart(f *file, line int) {
	if !b.rawSeen[f] {
		b.rawSeen[f] = true
		b.rawTok = append(b.rawTok, part(f, line))
	}
}

func (b *batch) evPart(f *file, line int) {
	if !b.evSeen[f] {
		b.evSeen[f] = true
		b.evTok = append(b.evTok, part(f, line))
	}
}

func (b *batch) reset() {
	*b = batch{}
}

// token joins the parts: an insert of one file's piece is "file_id:first line"; a longer list is hashed.
func token(parts []string) string {
	t := strings.Join(parts, ",")
	if len(t) <= 512 {
		return t
	}
	s := sha256.Sum256([]byte(t))
	return "sha256:" + hex.EncodeToString(s[:])
}

// flush sends the batch: raw, then events, then the files rows, each retried with the same token until the
// server takes it. A repeat of an insert the server has already applied is dropped by the token.
func (l *loader) flush() error {
	b := &l.batch
	if len(b.touched) == 0 {
		return nil
	}
	if b.rawRows > 0 || b.evRows > 0 {
		// the stored state of the files, marked dirty: a loader stopped before the clean rows below deletes
		// what went past it at its next start
		var dirty bytes.Buffer
		ver := l.nextVer()
		for _, f := range b.touched {
			fileRow(&dirty, f.committed(), ver, true)
		}
		if err := l.insert("files", fileColumns, &dirty, fmt.Sprintf("files:v%d", ver)); err != nil {
			return err
		}
	}
	if b.rawRows > 0 {
		if err := l.insert("raw", rawColumns, &b.raw, token(b.rawTok)); err != nil {
			return err
		}
	}
	if b.evRows > 0 {
		if err := l.insert("events", eventColumns, &b.events, token(b.evTok)); err != nil {
			return err
		}
	}
	// the token of a files insert is its ver: a row of the same lines is new when the path or the size changed
	var files bytes.Buffer
	ver := l.nextVer()
	for _, f := range b.touched {
		fileRow(&files, f, ver, false)
	}
	if err := l.insert("files", fileColumns, &files, fmt.Sprintf("files:v%d", ver)); err != nil {
		return err
	}
	for _, f := range b.touched {
		f.commit()
	}
	if b.rawRows > 0 {
		l.st.rowsLoaded(l.now())
	}
	l.rawRows += b.rawRows
	l.evRows += b.evRows
	l.rawBytes += b.rawBytes
	b.reset()
	return nil
}

func (l *loader) insert(table, columns string, body *bytes.Buffer, tok string) error {
	q := fmt.Sprintf("INSERT INTO %s.%s (%s) FORMAT TabSeparated", l.cfg.db, table, columns)
	settings := url.Values{"insert_deduplication_token": {tok}, "async_insert": {"0"}}
	if l.cfg.once && table == "events" {
		// the text index of a first load is built afterwards in one pass (--materialize): building it during
		// a large insert does not fit the server's memory
		settings.Set("exclude_materialize_skip_indexes_on_insert", "hay_ng")
	}
	data := body.Bytes()
	return l.retry("insert "+table, func() error {
		return l.srv.post(context.Background(), q, bytes.NewReader(data), settings, nil, 10*time.Minute)
	})
}

// lag — bytes of complete lines in known files that the server has not confirmed.
func (l *loader) lag() (lag int64, n int) {
	for _, f := range l.all {
		if f.gone {
			continue
		}
		n++
		lag += max(f.complete-f.ack.ingested, 0)
	}
	return lag, n
}

func (l *loader) report() {
	lag, n := l.lag()
	l.st.set(lag, n)
}

// visitAll visits paths in order; a stop request ends it between files.
func (l *loader) visitAll(paths []string) error {
	sort.Strings(paths)
	for _, p := range paths {
		select {
		case <-l.stop:
			return errStopped
		default:
		}
		if err := l.visit(p); err != nil {
			return err
		}
		if l.cfg.once {
			if f := l.files[p]; f != nil {
				f.follow, f.parsed = nil, 0 // a first load keeps no parser: memory stays at one file
			}
		}
	}
	return l.flush()
}
