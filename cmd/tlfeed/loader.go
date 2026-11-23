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
	"maps"
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
	lock            string // folder of the lock files; "" — no lock (tests)
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
	parsers       map[*file]bool
	ids           map[uint64]bool
	dirs          *dirs
	lockFile      *os.File // the lock of the database, held for the life of the process
	st            *status
	batch         batch
	buf           []byte
	ver           uint64
	sweep         int
	run           string // id of this run in the query_id of its writes
	seq           int
	needsFullList bool // a full listing failed, so later incremental passes cannot advance db_through
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
	l.run = fmt.Sprintf("%x", time.Now().UnixNano())
	return l
}

// queryPrefix — the start of the query_id of every write of the loaders of this database; a run adds its own id.
func (l *loader) queryPrefix() string { return "tlfeed-" + l.cfg.db + "-" }

// queryID — a new query_id of a write of this run.
func (l *loader) queryID() string {
	l.seq++
	return fmt.Sprintf("%s%s-%d", l.queryPrefix(), l.run, l.seq)
}

// write runs a statement that changes tables (a DELETE, an INSERT ... SELECT) under a query_id of this run.
func (l *loader) write(what, q string, settings url.Values) error {
	return l.retry(what, func() error {
		s := url.Values{"query_id": {l.queryID()}}
		maps.Copy(s, settings)
		return l.srv.post(context.Background(), q, nil, s, nil, 10*time.Minute)
	})
}

// genIs — the condition that the file's latest files row still has the generation f.gen. A write of a stopped
// loader that the server runs late, after the next start moved the file to a new generation, changes nothing.
func (l *loader) genIs(files ...*file) string {
	var b strings.Builder
	for i, f := range files {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%d,%d)", f.id, f.gen)
	}
	return fmt.Sprintf("file_id IN (SELECT file_id FROM %s.files FINAL WHERE (file_id, gen) IN (%s))", l.cfg.db, b.String())
}

// killOthers stops the writes of other runs still on the server (a loader killed in the middle of a request) and
// waits for them to end: after it, nothing of theirs comes into the tables but what genIs turns away.
func (l *loader) killOthers() error {
	p := l.queryPrefix()
	q := fmt.Sprintf("KILL QUERY WHERE startsWith(query_id, '%s') AND NOT startsWith(query_id, '%s%s-') SYNC", p, p, l.run)
	return l.exec("stop the writes of a stopped loader", q)
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
	var metadata []*file
	for _, r := range rows {
		f, ver, d, err := parseFileRow(l.cfg.home, r)
		if err != nil {
			return err
		}
		l.ver = max(l.ver, ver)
		// the stored size can run past the loaded lines: a loader stopped after a files row took the new size
		// and before the lines came; the first look reads the file past ingested
		f.complete = f.size
		if f.src.Description == "" {
			if src, e := transcript.SourceAt(f.src.Path); e == nil && src.Description != "" {
				f.src.Description = src.Description
				metadata = append(metadata, f)
			}
		}
		l.add(f)
		if d {
			dirty = append(dirty, f)
		}
	}
	if len(dirty) > 0 {
		// the new generation first, then no write of the stopped loader is running, then its rows go
		was := make([]uint64, len(dirty))
		for i, f := range dirty {
			was[i], f.gen = f.gen, l.nextGen()
		}
		if err := l.storeFiles(dirty, true, was); err != nil {
			return err
		}
		if err := l.killOthers(); err != nil {
			return err
		}
		for _, f := range dirty {
			if err := l.purge(f, fmt.Sprintf("stopped while sending lines past %d", f.lines), false); err != nil {
				return err
			}
		}
	}
	if len(metadata) > 0 {
		gens := make([]uint64, len(metadata))
		for i, f := range metadata {
			gens[i] = f.gen
		}
		if err := l.storeFilesKeepSize(metadata, gens); err != nil {
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
		l.dirs.addFileDir(f.src.Path, f.mtime)
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
		l.dirs.addFileDir(s.Path, time.Time{})
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
	if st.Size() == f.size && st.ModTime().Unix() == f.mtime.Unix() && transcript.Inode(st) == f.inode && f.complete <= f.ingested && f.evLines >= f.lines && !f.stale {
		return nil
	}
	if err := l.checkTail(f, st.Size()); err != nil {
		return err
	}
	changed := f.inode != transcript.Inode(st) || f.mtime.Unix() != st.ModTime().Unix() || f.size != st.Size()
	f.size, f.mtime, f.inode = st.Size(), st.ModTime(), transcript.Inode(st)
	if changed && l.now().Sub(f.mtime) < recentTask {
		l.dirs.activate(path) // a worker task written to again: its session folders are watched from now
	}
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
		if f = l.renamed(src, transcript.Inode(st), st.Size()); f == nil {
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
		l.dirs.addFileDir(path, f.mtime)
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
	return transcript.Inode(st) == ino
}

// rewind — the file is shorter than what is loaded, or the loaded bytes changed: its rows go and it is loaded
// again.
func (l *loader) rewind(f *file, why string) error {
	if err := l.flush(); err != nil {
		return err
	}
	f.lines, f.evLines, f.ingested, f.complete, f.tail = 0, 0, 0, 0, 0
	f.first, f.last, f.bad, f.badBase, f.name, f.follow, f.parsed = time.Time{}, time.Time{}, 0, 0, "", nil, 0
	return l.truncate(f, "rewind ("+why+")", true)
}

// reloadEvents — a rollout began to write message events, so the events of its earlier lines change (see
// transcript.ErrFollowRestart): they are deleted and parsed again from the first line; raw stays.
func (l *loader) reloadEvents(f *file) error {
	if err := l.flush(); err != nil {
		return err
	}
	f.evLines, f.follow, f.parsed, f.first, f.last, f.badBase = 0, nil, 0, time.Time{}, time.Time{}, 0
	return l.truncate(f, "message events appeared", false)
}

// truncate makes the tables hold the file up to its lines (raw) and evLines (events) and no more. The rows past
// them are deleted under a dirty files row of a new generation: the tokens of the next inserts differ from those
// of the deleted rows, which the server still remembers, and a loader stopped in the middle does it again at its
// next start.
func (l *loader) truncate(f *file, why string, clearUsage bool) error {
	if err := l.flush(); err != nil {
		return err
	}
	was := f.gen
	f.gen = l.nextGen()
	if err := l.storeFiles([]*file{f}, true, []uint64{was}); err != nil {
		return err
	}
	return l.purge(f, why, clearUsage)
}

// purge deletes the rows of a file of the new generation past its lines and evLines, then stores it clean. usage
// has rows merged over the parts of a reply, so rows inside 89 days are made again from events; older rows stay when
// the transcript is unchanged because their events may have been removed by TTL. A raw-file rewind clears all usage
// because replies removed from the transcript can no longer be matched after TTL removes their events. Every statement
// holds only while the file keeps this generation.
func (l *loader) purge(f *file, why string, clearUsage bool) error {
	l.logf("%s: %s: rows of id %d past line %d (events past %d) deleted", f.src.Path, why, f.id, f.lines, f.evLines)
	db, gen := l.cfg.db, l.genIs(f)
	usageCutoff := l.now().UTC().Truncate(time.Second)
	withinRetention := eventWithinRetentionAt("ts", usageCutoff, eventUsageRebuildDays)
	usageDelete := fmt.Sprintf("DELETE FROM %s.usage WHERE file_id = %d AND %s AND %s", db, f.id, withinRetention, gen)
	if clearUsage {
		usageDelete = fmt.Sprintf("DELETE FROM %s.usage WHERE file_id = %d AND %s", db, f.id, gen)
	}
	for _, q := range []string{
		fmt.Sprintf("DELETE FROM %s.events WHERE file_id = %d AND line > %d AND %s", db, f.id, f.evLines, gen),
		fmt.Sprintf("DELETE FROM %s.replies WHERE file_id = %d AND line > %d AND %s", db, f.id, f.evLines, gen),
		usageDelete,
		fmt.Sprintf("DELETE FROM %s.raw WHERE file_id = %d AND line > %d AND %s", db, f.id, f.lines, gen),
	} {
		if err := l.write("delete rows past the stored lines", q, nil); err != nil {
			return err
		}
	}
	if f.evLines > 0 {
		q := fmt.Sprintf("INSERT INTO %s.usage (%s) SELECT %s FROM %s.events WHERE file_id = %d AND reply_id != '' AND %s AND %s",
			db, usageColumns, usageColumns, db, f.id, withinRetention, gen)
		settings := url.Values{"insert_deduplication_token": {fmt.Sprintf("usage:%d:g%d", f.id, f.gen)}, "async_insert": {"0"}}
		if err := l.write("usage of the kept events", q, settings); err != nil {
			return err
		}
	}
	// the statements above did nothing if the file has a newer generation; then the clean row is turned away too
	if err := l.storeFiles([]*file{f}, false, []uint64{f.gen}); err != nil {
		return err
	}
	f.commit()
	return nil
}

// storeFiles writes the rows of files now, outside a batch; their size is 0, so a start before the next batch
// looks at them again. was[i] — the generation the stored row of fs[i] must have for its new row to be written.
func (l *loader) storeFiles(fs []*file, dirty bool, was []uint64) error {
	return l.writeFiles(fs, dirty, was, false)
}

// storeFilesKeepSize rewrites metadata on a known row without making the next startup rescan the transcript.
func (l *loader) storeFilesKeepSize(fs []*file, was []uint64) error {
	return l.writeFiles(fs, false, was, true)
}

func (l *loader) writeFiles(fs []*file, dirty bool, was []uint64, keepSize bool) error {
	var b bytes.Buffer
	ver := l.nextVer()
	var stored []*file
	var gens []uint64
	for i, f := range fs {
		if f.id == 0 {
			continue
		}
		c := *f
		if !keepSize {
			c.size = 0
		}
		fileRow(&b, &c, ver, dirty)
		stored, gens = append(stored, f), append(gens, was[i])
	}
	if len(stored) == 0 {
		return nil
	}
	return l.insert("files", fileColumns, &b, len(stored), fmt.Sprintf("files:v%d", ver), l.filesHeld(stored, gens))
}

// filesHeld — the condition for files rows: the file has no row yet, or its row has the generation gens[i] this
// loader knows. A loader whose file another one has taken on with a new generation writes no row for it.
func (l *loader) filesHeld(fs []*file, gens []uint64) string {
	var ids, pairs strings.Builder
	for i, f := range fs {
		if i > 0 {
			ids.WriteByte(',')
			pairs.WriteByte(',')
		}
		fmt.Fprintf(&ids, "%d", f.id)
		fmt.Fprintf(&pairs, "(%d,%d)", f.id, gens[i])
	}
	return fmt.Sprintf("file_id NOT IN (SELECT file_id FROM %s.files FINAL WHERE file_id IN (%s) AND (file_id, gen) NOT IN (%s))",
		l.cfg.db, ids.String(), pairs.String())
}

// gensOf — the generations the loader knows of the files.
func gensOf(fs []*file) []uint64 {
	out := make([]uint64, len(fs))
	for i, f := range fs {
		out[i] = f.gen
	}
	return out
}

// nextGen — a new generation of a file: from the write order, so it is above every earlier one, and two loaders
// that take one at once from the same stored row get different ones (the same only at the same nanosecond); the
// generation filter then lets only the writes of the one whose row is the latest through.
func (l *loader) nextGen() uint64 {
	return l.nextVer()
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

// newParser builds the parser of a file from its start. It learns from all lines it will get which events the file
// writes, and it only keeps the state over the lines whose events are already in the table. A rollout that began
// to write message events after those lines had them made the other way: ErrFollowRestart, its events are made
// again. A file that cannot be read now is left without a parser.
func (l *loader) newParser(f *file) error {
	path := f.src.Path
	fl := transcript.NewFollow()
	// the flags as of the end of line evLines, the last whose events are stored
	var before [2]bool
	lines := 0
	if _, err := pieces(path, 0, f.size, l.cfg.batch, &l.buf, func(data []byte, _ int64) error {
		if left := f.evLines - lines; left > 0 {
			cut, n := 0, 0
			for n < left && cut < len(data) {
				cut += bytes.IndexByte(data[cut:], '\n') + 1
				n++
			}
			lines += n
			if err := fl.Prescan(data[:cut]); err != nil {
				return err
			}
			before[0], before[1] = fl.MessageEvents()
			data = data[cut:]
		}
		return fl.Prescan(data)
	}); err != nil {
		l.fileErr(path, err)
		f.stale = true
		return nil
	}
	if f.evLines > 0 {
		if user, agent := fl.MessageEvents(); before != [2]bool{user, agent} {
			l.logf("%s: message events began past the stored events", path)
			return transcript.ErrFollowRestart
		}
	}
	if err := fl.SkipThrough(f.evLines); err != nil {
		return err
	}
	f.follow, f.parsed, f.badBase = fl, 0, f.bad
	if f.evLines == 0 {
		f.badBase = 0
	}
	l.parsers[f] = true
	return nil
}

func (l *loader) loadOnce(f *file) error {
	path, size := f.src.Path, f.size
	if f.follow == nil && f.evLines >= f.lines {
		has, err := transcript.LineEndsAfter(path, f.ingested, size)
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
		if err := l.newParser(f); err != nil {
			return err
		}
		if f.follow == nil {
			return nil // unreadable now: the next pass tries again
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
	if b.raw.Len()+b.events.Len() >= l.cfg.batch || len(b.touched) >= maxTouched {
		return l.flush()
	}
	return nil
}

// maxTouched — files in one insert at most: the insert names each with its generation in the query string, which
// the server takes up to http_max_uri_size (1 MiB by default).
const maxTouched = 2000

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
		if err := l.insert("files", fileColumns, &dirty, len(b.touched), fmt.Sprintf("files:v%d", ver), l.filesHeld(b.touched, gensOf(b.touched))); err != nil {
			return err
		}
	}
	if b.rawRows > 0 {
		if err := l.insert("raw", rawColumns, &b.raw, b.rawRows, token(b.rawTok), l.genIs(b.touched...)); err != nil {
			return err
		}
	}
	if b.evRows > 0 {
		if err := l.insert("events", eventColumns, &b.events, b.evRows, token(b.evTok), l.genIs(b.touched...)); err != nil {
			return err
		}
	}
	// the token of a files insert is its ver: a row of the same lines is new when the path or the size changed
	var files bytes.Buffer
	ver := l.nextVer()
	for _, f := range b.touched {
		fileRow(&files, f, ver, false)
	}
	// the clean rows only for files that still have this generation: the batch is in the tables only then
	if err := l.insert("files", fileColumns, &files, len(b.touched), fmt.Sprintf("files:v%d", ver), l.filesHeld(b.touched, gensOf(b.touched))); err != nil {
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

// insert sends rows in TSV under a dedup token; the server keeps only the rows where holds (genIs, filesHeld): a
// late insert of a stopped loader adds nothing after the next start's cleanup, nor the insert of a loader whose
// files another one has taken on.
func (l *loader) insert(table, columns string, body *bytes.Buffer, rows int, tok, where string) error {
	q := fmt.Sprintf("INSERT INTO %s.%s (%s) SELECT * FROM input() WHERE %s FORMAT TabSeparated", l.cfg.db, table, columns, where)
	// wait_end_of_query: the answer, with its summary of written rows, comes once the insert is over
	settings := url.Values{"insert_deduplication_token": {tok}, "async_insert": {"0"}, "wait_end_of_query": {"1"}}
	if l.cfg.once && table == "events" {
		// the text index of a first load is built afterwards in one pass (--materialize): building it during
		// a large insert does not fit the server's memory
		settings.Set("exclude_materialize_skip_indexes_on_insert", "hay_ng")
	}
	data := body.Bytes()
	var written int64
	if err := l.retry("insert "+table, func() error {
		settings.Set("query_id", l.queryID())
		var err error
		written, err = l.srv.insert(context.Background(), q, bytes.NewReader(data), settings, 10*time.Minute)
		return err
	}); err != nil {
		return err
	}
	// written_rows counts the rows the materialized views write too: only raw and files have none
	if table != "events" && written != int64(rows) {
		// rows the generation filter turned away: another loader has taken the files on; nothing more is
		// written, so no clean files row claims them
		return fmt.Errorf("insert %s: the server wrote %d of %d rows: a file of the batch has a newer generation, "+
			"another loader works on %s", table, written, rows, l.cfg.db)
	}
	return nil
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
