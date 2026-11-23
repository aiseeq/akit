package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/price"
	"github.com/aiseeq/akit/transcript"
)

// The database path answers from the tables the transcript loader fills — files, events and raw — and prints
// what the file path prints: the same window (files by modification time, events by their time), the same
// filters and the same masking. Lines the loader has not stored yet are read from the file itself.

// backendDB and backendFiles are the values of TL_BACKEND; files is the default.
const (
	backendFiles = "files"
	backendDB    = "db"
)

// dbQueryTimeout bounds one call through the database; past it the call reads the files.
const dbQueryTimeout = 60 * time.Second

// mtimeSlack widens the database's modification times of files: the loader stats a file when it reads it, so
// its time may lag the file's own; the file's own time decides afterwards.
const mtimeSlack = time.Hour

// tailMargin — how long after a line's event its record may still be written: a file the loader read past until
// plus this has no unread line with an event inside the window.
const tailMargin = 5 * time.Minute

// rawSettings is the profile of a search in the raw lines: lines of json reach megabytes, and with the default
// block of 65536 rows each of the 8 threads holds blocks of hundreds of megabytes and the query passes the 2 GiB
// limit; blocks of 1024 rows or 16 MB keep it under 200 MiB.
var rawSettings = url.Values{"max_threads": {"8"}, "max_block_size": {"1024"}, "preferred_block_size_bytes": {"16000000"}}

// dbForm tells whether the database path answers this call; the other forms read files.
func dbForm(opts options) bool {
	return !opts.version && !opts.worktime && !opts.openQuestions && opts.waitProg == "" && !opts.waitAll && opts.context == 0
}

// dbFile is what the files table knows of one transcript.
type dbFile struct {
	id       uint64
	path     string
	name     string
	ingested int64
	lines    int
	evLines  int
	mtime    time.Time
	loaded   time.Time // when the loader wrote this row
	bad      int       // lines the loader could not parse
}

type dbPath struct {
	c     *chClient
	ctx   context.Context
	opts  options
	e     env
	f     *filter
	files map[string]*dbFile
	names []string // transcript names find knows, from the files table
	trace io.Writer
}

// runDB answers the call through the database into stdout and stderr; an error means no answer was printed and
// the caller reads the files.
func runDB(ctx context.Context, c *chClient, opts options, e env, refs []eventRef, stdout, stderr io.Writer) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, dbQueryTimeout)
	defer cancel()
	d := &dbPath{c: c, ctx: ctx, opts: opts, e: e, trace: e.dbTrace}
	f, err := newFilter(opts)
	if err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage, nil
	}
	d.f = f
	if err := d.loadFiles(); err != nil {
		return 0, err
	}
	if len(refs) > 0 {
		if e.collect != nil {
			e.collect.filled = true
		}
		return d.refs(refs, stdout, stderr)
	}
	start := time.Now()
	sources, err := d.resolve()
	if err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage, nil
	}
	d.tracef("sources", start, len(sources))
	start = time.Now()
	res, err := d.result(sources)
	if err != nil {
		return 0, err
	}
	d.tracef("result", start, len(res.hits))
	if e.collect != nil {
		e.collect.res, e.collect.filled = &res, true
	}
	start = time.Now()
	defer d.tracef("report", start, len(res.hits))
	if opts.usage {
		return reportUsage(res, sources, opts, e, stdout), nil
	}
	return report(res, opts, e, stdout), nil
}

func (d *dbPath) tracef(what string, start time.Time, n int) {
	if d.trace != nil {
		fmt.Fprintf(d.trace, "tl: db %s: %d ms, %d\n", what, time.Since(start).Milliseconds(), n)
	}
}

func (d *dbPath) query(q chQuery) ([][]string, error) {
	start := time.Now()
	rows, err := d.c.selectTSV(d.ctx, q)
	if d.trace != nil {
		fmt.Fprintf(d.trace, "tl: db %s %d ms, %d rows, err %v\n", q.name, time.Since(start).Milliseconds(), len(rows), err)
	}
	return rows, err
}

func (d *dbPath) loadFiles() error {
	rows, err := d.query(chQuery{name: "files", sql: "SELECT file_id, path, name, ingested, lines, ev_lines, toUnixTimestamp(mtime), " +
		"intDiv(ver, 1000000), bad FROM files FINAL"})
	if err != nil {
		return err
	}
	d.files = make(map[string]*dbFile, len(rows))
	patterns := transcriptPatterns(d.e.home)
	for _, row := range rows {
		if len(row) != 9 {
			return fmt.Errorf("files: %d fields in a row, want 9", len(row))
		}
		p := fields{row: row}
		f := &dbFile{id: p.uint(0), path: row[1], name: row[2], ingested: int64(p.uint(3)), lines: p.int(4), evLines: p.int(5),
			mtime: time.Unix(int64(p.uint(6)), 0), loaded: time.UnixMilli(int64(p.uint(7))), bad: p.int(8)}
		if p.err != nil {
			return fmt.Errorf("files: %w", p.err)
		}
		// A path written again after its file was deleted gets a new id: the later row is the file there now.
		if old := d.files[f.path]; old != nil && old.loaded.After(f.loaded) {
			continue
		}
		if d.files[f.path] == nil {
			ok, err := matchesAny(patterns, f.path)
			if err != nil {
				return err
			}
			if ok {
				d.names = append(d.names, f.path)
			}
		}
		d.files[f.path] = f
	}
	slices.Sort(d.names)
	return nil
}

func matchesAny(patterns []string, path string) (bool, error) {
	for _, pattern := range patterns {
		ok, err := filepath.Match(pattern, path)
		if err != nil {
			return false, fmt.Errorf("transcript pattern %s: %w", pattern, err)
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// fields reads typed fields of one answer row; the first that does not parse is kept in err.
type fields struct {
	row []string
	err error
}

func (p *fields) uint(i int) uint64 {
	n, err := strconv.ParseUint(p.row[i], 10, 64)
	if err != nil && p.err == nil {
		p.err = fmt.Errorf("field %d %q: %w", i+1, p.row[i], err)
	}
	return n
}

func (p *fields) int(i int) int { return int(p.uint(i)) }

// resolve is resolve of the file path over the names in the files table.
func (d *dbPath) resolve() ([]source, error) {
	return resolveWith(d.opts, d.e, d.window, d.find)
}

// window is the choice of AllSourcesWindow among the files the table knows, in the same order: by pattern, sorted
// inside each. A file's time there may lag its own (mtimeSlack); the file decides.
func (d *dbPath) window(since time.Time) ([]source, error) {
	var candidates []string
	for _, pattern := range transcript.SourcePatterns(d.e.home, d.opts.codex, !d.opts.noSubagents) {
		for _, path := range d.names {
			if d.files[path].mtime.Before(since.Add(-mtimeSlack)) {
				continue
			}
			ok, err := filepath.Match(pattern, path)
			if err != nil {
				return nil, fmt.Errorf("list transcripts %s: %w", pattern, err)
			}
			if ok {
				candidates = append(candidates, path)
			}
		}
	}
	sources, err := transcript.SelectSources(candidates, since, d.opts.until, d.opts.project)
	if err != nil {
		return nil, fmt.Errorf("list transcripts: %w", err)
	}
	return sources, nil
}

func (d *dbPath) find(arg string) (string, error) {
	return findIn(arg, d.e.home, func() ([]string, error) { return d.names, nil })
}

// kindOfSource is the kind column of the events of a source.
func kindOfSource(src source) string {
	switch {
	case isCodexRollout(src.Path):
		return "codex"
	case src.ParentID != "":
		return "sub"
	}
	return "claude"
}

// where is the condition of the events of the call in the files ids, with its parameters and settings.
type where struct {
	sql      string
	params   map[string]string
	settings url.Values
}

// grepMode — how --grep is asked: through hyperscan over the lower-case text, through RE2 over it when hyperscan
// does not take the pattern, through RE2 over the text as written when the pattern has case-sensitive parts.
type grepMode int

const (
	grepFast grepMode = iota
	grepRE2
	grepAsWritten
)

func (d *dbPath) where(ids []uint64, kinds []string, mode grepMode) (where, error) {
	opts := d.opts
	w := where{params: map[string]string{}, settings: url.Values{}}
	roles := make([]string, 0, len(opts.roles))
	for _, r := range transcript.Roles {
		if opts.roles[r] {
			roles = append(roles, string(r))
		}
	}
	w.params["files"] = chUints(ids)
	w.params["kinds"] = chStrings(kinds)
	w.params["roles"] = chStrings(roles)
	conds := []string{"file_id IN {files:Array(UInt64)}", "kind IN {kinds:Array(String)}", "role IN {roles:Array(String)}"}
	if !opts.since.IsZero() {
		// ts holds milliseconds: an event at the same millisecond as since but before it in finer time is out
		since := opts.since.Truncate(time.Millisecond)
		if since.Before(opts.since) {
			since = since.Add(time.Millisecond)
		}
		w.params["since"] = chTime(since)
		conds = append(conds, "ts >= {since:DateTime64(3, 'UTC')}")
	}
	if !opts.until.IsZero() {
		w.params["until"] = chTime(opts.until)
		conds = append(conds, "ts <= {until:DateTime64(3, 'UTC')}")
	}
	if opts.grep != "" {
		cond, params, err := grepCondition(opts.grep, mode)
		if err != nil {
			return w, err
		}
		conds = append(conds, cond)
		maps.Copy(w.params, params)
	}
	if opts.raw != "" {
		cond, params, err := rawCondition(opts.raw)
		if err != nil {
			return w, err
		}
		conds = append(conds, "(file_id, line) IN (SELECT file_id, line FROM raw WHERE file_id IN {files:Array(UInt64)} AND "+cond+")")
		maps.Copy(w.params, params)
		maps.Copy(w.settings, rawSettings)
	}
	w.sql = strings.Join(conds, " AND ")
	return w, nil
}

// stored is a source and what the database has of it.
type stored struct {
	src  source
	file *dbFile
}

// result is scan of the file path from the database: the events of the sources in the window, sorted the same
// way, with the link ids and names of the sources.
func (d *dbPath) result(sources []source) (result, error) {
	start := time.Now()
	liveNames, liveErr := liveSessionNames(d.e.home)
	d.tracef("live names", start, len(liveNames))
	start = time.Now()
	res := result{names: map[string]string{}, linkIDs: sourceLinkIDs(sources, d.names, d.e.home, liveNames), searchPaths: d.names,
		liveNames: liveNames, liveErr: liveErr, files: len(sources)}
	d.tracef("link ids", start, len(res.linkIDs))
	var (
		inDB  []stored
		local []source // not in the files table yet: read as the file path does
	)
	for _, src := range sources {
		f := d.files[src.Path]
		if f == nil {
			local = append(local, src)
			continue
		}
		inDB = append(inDB, stored{src: src, file: f})
		if f.name != "" {
			res.names[src.Path] = f.name
		}
	}
	var all []sortedHit
	if len(inDB) > 0 {
		hits, err := d.storedHits(inDB)
		if err != nil {
			return res, err
		}
		all = hits
	}
	bad := map[string]badLines{}
	for _, src := range local {
		hits, _, _, meta, err := d.f.file(src)
		bad[src.Path] = badLines{n: meta.Bad, first: meta.FirstBad}
		for i := range hits {
			all = append(all, sortedHit{hit: hits[i], part: "~", off: uint64(i)})
		}
		if meta.Name != "" {
			res.names[src.Path] = meta.Name
		}
		if err != nil {
			res.errs = append(res.errs, err)
		}
	}
	if err := d.badLines(inDB, bad); err != nil {
		return res, err
	}
	for _, src := range sources {
		b := bad[src.Path]
		res.bad += b.n
		if b.first != nil && res.firstBad == nil {
			res.firstBad = fmt.Errorf("%s %w", res.linkIDs[src.Path], b.first)
		}
	}
	start = time.Now()
	sortHits(all)
	d.tracef("sort", start, len(all))
	res.hits = make([]hit, len(all))
	for i := range all {
		res.hits[i] = all[i].hit
	}
	return res, nil
}

// badLines are the lines of one file the parse skipped, as Meta has them.
type badLines struct {
	n     int
	first error
}

// badLines counts the unparsable lines of the stored files the way a read with this filter counts them: a line
// the prefilter drops is not parsed, so it is not counted. The lines that are not JSON come from raw; a line that
// is JSON but not a record (a bad time, a wrong shape) is only in the count of the files table, so it is counted
// without a place.
func (d *dbPath) badLines(inDB []stored, out map[string]badLines) error {
	var ids []uint64
	byID := map[uint64]stored{}
	for _, s := range inDB {
		if s.file.bad > 0 {
			ids = append(ids, s.file.id)
			byID[s.file.id] = s
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := d.query(chQuery{name: "bad", sql: "SELECT file_id, line, raw FROM raw WHERE file_id IN {files:Array(UInt64)} AND NOT isValidJSON(raw) " +
		"ORDER BY file_id, line", params: map[string]string{"files": chUints(ids)}, settings: rawSettings})
	if err != nil {
		return err
	}
	invalid := map[uint64]int{}
	for _, row := range rows {
		if len(row) != 3 {
			return fmt.Errorf("bad: %d fields in a row, want 3", len(row))
		}
		p := fields{row: row}
		id, line := p.uint(0), p.int(1)
		if p.err != nil {
			return fmt.Errorf("bad: %w", p.err)
		}
		s, ok := byID[id]
		if !ok || line > s.file.evLines {
			continue
		}
		var v any
		jerr := json.Unmarshal([]byte(row[2]), &v)
		if jerr == nil {
			continue
		}
		invalid[id]++
		if !d.f.keepsBad([]byte(row[2])) {
			continue
		}
		b := out[s.src.Path]
		b.n++
		if b.first == nil {
			b.first = fmt.Errorf("line %d: not a JSON record: %w", line, jerr)
		}
		out[s.src.Path] = b
	}
	for id, s := range byID {
		if rest := s.file.bad - invalid[id]; rest > 0 {
			b := out[s.src.Path]
			b.n += rest
			out[s.src.Path] = b
		}
	}
	return nil
}

// sortedHit is a hit with what orders the events of one line: the role, then the place of the row in its part
// (the loader inserts the events of a line in the order the file has them).
type sortedHit struct {
	hit
	rank int
	part string
	off  uint64
	// key identifies a row of the database for a second read of it
	fileID uint64
}

// roleRank orders the roles of one line as a record writes them: thinking, text, then the tool calls.
var roleRank = map[transcript.Role]int{transcript.User: 0, transcript.Ask: 1, transcript.Peer: 2, transcript.Think: 3, transcript.Agent: 4,
	transcript.Cmd: 5, transcript.Out: 6, transcript.Attach: 7, transcript.Note: 8}

// sortHits is the order of scan: by time, then path, then line; the events of one line in the order the file has
// them.
func sortHits(hits []sortedHit) {
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := &hits[i], &hits[j]
		if c := a.ev.Time.Compare(b.ev.Time); c != 0 {
			return c < 0
		}
		return cmp.Or(cmp.Compare(a.src.Path, b.src.Path), cmp.Compare(a.ev.Line, b.ev.Line), cmp.Compare(a.rank, b.rank),
			cmp.Compare(a.part, b.part), cmp.Compare(a.off, b.off)) < 0
	})
}

// dbEvent is an event from its columns in the events table; text is Shown() there. The fields set here are the
// columns the table has: a new field of Event reaches this path with a new column, not with a change here.
func dbEvent(line int, ms int64, role, who, text string) transcript.Event {
	return transcript.Event{ //nolint:scattered-construction // only the columns of the events table exist here
		Line: line, Time: msToTime(ms), Role: transcript.Role(role), Who: who, Text: text}
}

func (d *dbPath) storedHits(inDB []stored) ([]sortedHit, error) {
	ids := make([]uint64, 0, len(inDB))
	kindSet := map[string]bool{}
	byID := make(map[uint64]stored, len(inDB))
	for _, s := range inDB {
		ids = append(ids, s.file.id)
		kindSet[kindOfSource(s.src)] = true
		byID[s.file.id] = s
	}
	kinds := slices.Sorted(maps.Keys(kindSet))
	var (
		hits []sortedHit
		err  error
	)
	if d.opts.usage {
		hits, err = d.usageHits(ids, kinds, byID)
	} else {
		hits, err = d.eventRows(ids, kinds, byID)
	}
	if err != nil {
		return nil, err
	}
	return d.withTails(hits, inDB)
}

// withTails adds the events of the lines the loader has not stored, read from the files.
func (d *dbPath) withTails(hits []sortedHit, inDB []stored) ([]sortedHit, error) {
	for _, s := range inDB {
		if !d.needTail(s.file) {
			continue
		}
		var tail []sortedHit
		start := time.Now()
		whole, err := readTail(s.src.Path, s.file.evLines, func(raw []byte, ev transcript.Event) {
			if !d.f.inWindow(ev) || !d.f.match(ev) || d.f.rawRE != nil && !d.f.rawRE.Match(raw) {
				return
			}
			if d.opts.usage && (ev.Reply == nil || ev.Reply.Usage == nil) {
				return
			}
			tail = append(tail, sortedHit{hit: hit{src: s.src, ev: ev}, part: "~", off: uint64(len(tail))})
		})
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read the unloaded lines of %s: %w", s.src.Path, err)
		}
		if d.trace != nil {
			fmt.Fprintf(d.trace, "tl: db tail of %s past line %d: %d ms, %d event(s), whole %v\n", s.src.ID, s.file.evLines,
				time.Since(start).Milliseconds(), len(tail), whole)
		}
		if whole {
			kept := hits[:0]
			for i := range hits {
				if hits[i].src.Path != s.src.Path {
					kept = append(kept, hits[i])
				}
			}
			hits = kept
		}
		hits = append(hits, tail...)
	}
	return hits, nil
}

// needTail tells whether the file may have lines past those in the events table with events in the window.
func (d *dbPath) needTail(f *dbFile) bool {
	if f.evLines < f.lines {
		return true
	}
	if !d.opts.until.IsZero() && f.loaded.After(d.opts.until.Add(tailMargin)) {
		return false
	}
	st, err := os.Stat(f.path)
	if err != nil || st.Size() <= f.ingested {
		return false
	}
	has, err := transcript.LineEndsAfter(f.path, f.ingested, st.Size())
	return err != nil || has
}

// textCut is how much of a long text a listing reads: the first cut of maskedShort and its first widening.
func textCut(width int) int { return 2 * (width + maskWindow) }

// flatLenSQL is the length of transcript.Short(text, 0) in characters: spaces as unicode.IsSpace has them,
// trimmed, a line break as " ⏎ ", runs of spaces as one.
const flatLenSQL = `lengthUTF8(arrayStringConcat(arrayFilter(x -> x != '', splitByRegexp({ws:String}, ` +
	`replaceAll(replaceRegexpAll(text, {trim:String}, ''), '\n', ' ⏎ '))), ' '))`

const spaceClass = `[\t\n\v\f\r \x{85}\x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]`

func (d *dbPath) eventRows(ids []uint64, kinds []string, byID map[uint64]stored) ([]sortedHit, error) {
	width := d.opts.width
	text := "text, 0"
	if width > 0 {
		text = "substringUTF8(text, 1, {cut:UInt32}), if(lengthUTF8(text) <= {cut:UInt32}, 0, " + flatLenSQL + ")"
	}
	rows, err := d.selectEvents("events", func(w where) chQuery {
		if width > 0 {
			w.params["cut"] = strconv.Itoa(textCut(width))
			w.params["ws"] = chString(spaceClass + "+")
			w.params["trim"] = chString("^" + spaceClass + "+|" + spaceClass + "+$")
		}
		return chQuery{sql: "SELECT file_id, line, toUnixTimestamp64Milli(ts), role, who, subtype, " + text + ", _part, _part_offset FROM events WHERE " + w.sql,
			params: w.params, settings: w.settings}
	}, ids, kinds)
	if err != nil {
		return nil, err
	}
	hits := make([]sortedHit, 0, len(rows))
	var long []int // rows whose start does not decide the cut
	for _, row := range rows {
		if len(row) != 10 {
			return nil, fmt.Errorf("events: %d fields in a row, want 10", len(row))
		}
		p := fields{row: row}
		id, line, ms, flat, off := p.uint(0), p.int(1), int64(p.uint(2)), p.int(7), p.uint(9)
		if p.err != nil {
			return nil, fmt.Errorf("events: %w", p.err)
		}
		s, ok := byID[id]
		if !ok || line > s.file.evLines {
			continue // a row past what the files table says is stored: the tail read gives it
		}
		h := sortedHit{hit: hit{src: s.src, ev: dbEvent(line, ms, row[3], row[4], row[6])}, rank: roleRank[transcript.Role(row[3])], part: row[8],
			off: off, fileID: id}
		h.ev.Subtype = row[5]
		if flat > 0 {
			cut, ok := "", false
			if flat > width {
				cut, ok = maskedCut([]rune(transcript.Short(row[6], 0)), flat, width)
			}
			if ok {
				h.cut = &cut
			} else {
				long = append(long, len(hits))
			}
		}
		hits = append(hits, h)
	}
	if len(long) > 0 {
		if err := d.wholeTexts(hits, long); err != nil {
			return nil, err
		}
	}
	return hits, nil
}

// wholeTexts reads the whole text of the events at idx: masking shrank their start below the cut.
func (d *dbPath) wholeTexts(hits []sortedHit, idx []int) error {
	type rowKey struct {
		file uint64
		line int
		role transcript.Role
		part string
		off  uint64
	}
	var pairs strings.Builder
	pairs.WriteByte('[')
	ids := map[uint64]bool{}
	for i, j := range idx {
		if i > 0 {
			pairs.WriteByte(',')
		}
		fmt.Fprintf(&pairs, "(%d,%d)", hits[j].fileID, hits[j].ev.Line)
		ids[hits[j].fileID] = true
	}
	pairs.WriteByte(']')
	idList := make([]uint64, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	rows, err := d.query(chQuery{name: "texts", sql: "SELECT file_id, line, role, _part, _part_offset, text FROM events WHERE file_id IN {files:Array(UInt64)} " +
		"AND (file_id, line) IN {pairs:Array(Tuple(UInt64, UInt32))}", params: map[string]string{"files": chUints(idList), "pairs": pairs.String()}})
	if err != nil {
		return err
	}
	texts := map[rowKey]string{}
	for _, row := range rows {
		if len(row) != 6 {
			return fmt.Errorf("texts: %d fields in a row, want 6", len(row))
		}
		p := fields{row: row}
		k := rowKey{file: p.uint(0), line: p.int(1), role: transcript.Role(row[2]), part: row[3], off: p.uint(4)}
		if p.err != nil {
			return fmt.Errorf("texts: %w", p.err)
		}
		texts[k] = row[5]
	}
	for _, j := range idx {
		h := &hits[j]
		text, ok := texts[rowKey{file: h.fileID, line: h.ev.Line, role: h.ev.Role, part: h.part, off: h.off}]
		if !ok {
			// merged between the two reads: the rows of the line moved
			return fmt.Errorf("texts: the event %s:%d is gone between two reads", h.src.ID, h.ev.Line)
		}
		h.ev.Text, h.cut = text, nil
	}
	return nil
}

// selectEvents runs a query over the events of the files, asking --grep in the fastest form the server takes.
func (d *dbPath) selectEvents(name string, build func(where) chQuery, ids []uint64, kinds []string) ([][]string, error) {
	mode := grepFast
	if d.opts.grep != "" {
		sensitive, err := caseSensitiveParts(d.opts.grep)
		if err != nil {
			return nil, err
		}
		if sensitive {
			mode = grepAsWritten
		}
	}
	for {
		w, err := d.where(ids, kinds, mode)
		if err != nil {
			return nil, err
		}
		q := build(w)
		q.name = name
		rows, err := d.query(q)
		var serverErr *chServerError
		if err != nil && mode == grepFast && d.opts.grep != "" && errors.As(err, &serverErr) {
			// hyperscan refuses some patterns RE2 takes: too large, or a construct it lacks
			mode = grepRE2
			continue
		}
		return rows, err
	}
}

func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// usageHits are the events of model replies with their usage. The token columns of the events table decide which
// record of a reply counts, as in usageReplies; the usage of that record — with the split of cache writes, speed
// and route the price needs — is read from its raw line.
func (d *dbPath) usageHits(ids []uint64, kinds []string, byID map[uint64]stored) ([]sortedHit, error) {
	const cond = " AND (reply_id != '' OR request_id != '')"
	rows, err := d.selectEvents("usage", func(w where) chQuery {
		return chQuery{sql: "SELECT file_id, line, toUnixTimestamp64Milli(ts), role, model, reply_id, request_id, _part, _part_offset FROM events WHERE " +
			w.sql + cond, params: w.params, settings: w.settings}
	}, ids, kinds)
	if err != nil {
		return nil, err
	}
	type replyKey struct {
		file uint64
		key  string
	}
	hits := make([]sortedHit, 0, len(rows))
	keyOf := map[uint64]map[int]string{} // file → line → reply key
	for _, row := range rows {
		if len(row) != 9 {
			return nil, fmt.Errorf("usage: %d fields in a row, want 9", len(row))
		}
		p := fields{row: row}
		id, line, ms, off := p.uint(0), p.int(1), int64(p.uint(2)), p.uint(8)
		if p.err != nil {
			return nil, fmt.Errorf("usage: %w", p.err)
		}
		s, ok := byID[id]
		if !ok || line > s.file.evLines {
			continue
		}
		// the usage comes from the raw line below
		reply := &transcript.Reply{Model: row[4], ID: row[5], RequestID: row[6]}
		h := sortedHit{hit: hit{src: s.src, ev: dbEvent(line, ms, row[3], "", "")}, rank: roleRank[transcript.Role(row[3])], part: row[7], off: off, fileID: id}
		h.ev.Reply = reply
		hits = append(hits, h)
		if keyOf[id] == nil {
			keyOf[id] = map[int]string{}
		}
		keyOf[id][line] = cmp.Or(reply.ID, reply.RequestID)
	}
	if len(hits) == 0 {
		return hits, nil
	}
	// the record with the most output of each reply, as the server picks it; its usage line from raw
	raw, err := d.selectEvents("usage-raw", func(w where) chQuery {
		settings := maps.Clone(rawSettings)
		return chQuery{sql: "SELECT file_id, line, JSONExtractRaw(raw, 'message', 'usage') FROM raw WHERE file_id IN {files:Array(UInt64)} AND " +
			"(file_id, line) IN (SELECT file_id, argMax(line, out_tok) FROM events WHERE " + w.sql + cond +
			" GROUP BY file_id, if(reply_id != '', reply_id, request_id))", params: w.params, settings: settings}
	}, ids, kinds)
	if err != nil {
		return nil, err
	}
	usages := map[replyKey]*price.Usage{}
	for _, row := range raw {
		if len(row) != 3 {
			return nil, fmt.Errorf("usage-raw: %d fields in a row, want 3", len(row))
		}
		p := fields{row: row}
		id, line := p.uint(0), p.int(1)
		if p.err != nil {
			return nil, fmt.Errorf("usage-raw: %w", p.err)
		}
		key, ok := keyOf[id][line]
		if !ok {
			continue
		}
		var u *price.Usage
		if row[2] != "" {
			if u, err = transcript.ParseUsage([]byte(row[2])); err != nil {
				return nil, fmt.Errorf("usage-raw %d:%d: %w", id, line, err)
			}
		}
		usages[replyKey{id, key}] = u
	}
	kept := hits[:0]
	for i := range hits {
		h := &hits[i]
		u, ok := usages[replyKey{h.fileID, cmp.Or(h.ev.Reply.ID, h.ev.Reply.RequestID)}]
		if !ok || u == nil {
			continue // a record without usage: the file path skips it too
		}
		// every record of the reply carries the usage of its largest one: the count takes one per reply
		h.ev.Reply.Usage = u
		kept = append(kept, *h)
	}
	return kept, nil
}

// refs prints events in full, as showRef does.
func (d *dbPath) refs(refs []eventRef, stdout, stderr io.Writer) (int, error) {
	width := 0
	if d.opts.widthSet {
		width = d.opts.width
	}
	var out, errs bytes.Buffer
	code := 0
	for _, ref := range refs {
		result, err := d.showRef(ref, width, &out, &errs)
		if err != nil {
			return 0, err
		}
		code = max(code, result)
	}
	if _, err := io.Copy(stdout, &out); err != nil {
		return 0, fmt.Errorf("write output: %w", err)
	}
	if _, err := io.Copy(stderr, &errs); err != nil {
		return 0, fmt.Errorf("write output: %w", err)
	}
	return code, nil
}

func (d *dbPath) showRef(ref eventRef, width int, w, stderr io.Writer) (int, error) {
	path, err := d.find(ref.id)
	if err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage, nil
	}
	src, err := transcript.SourceAt(path)
	if err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage, nil
	}
	f := d.files[path]
	var found []sortedHit
	if f == nil || ref.line > f.evLines {
		// not stored yet: the file has it
		evs, err := eventsAt(path, ref.line)
		if err != nil {
			fmt.Fprintf(stderr, "tl: %v\n", err)
			return exitUsage, nil
		}
		for i := range evs {
			found = append(found, sortedHit{hit: hit{src: src, ev: evs[i]}, off: uint64(i)})
		}
	} else {
		rows, err := d.query(chQuery{name: "event", sql: "SELECT toUnixTimestamp64Milli(ts), role, who, text, _part, _part_offset FROM events " +
			"WHERE file_id = {file:UInt64} AND line = {line:UInt32}", params: map[string]string{"file": strconv.FormatUint(f.id, 10), "line": strconv.Itoa(ref.line)}})
		if err != nil {
			return 0, err
		}
		for _, row := range rows {
			if len(row) != 6 {
				return 0, fmt.Errorf("event: %d fields in a row, want 6", len(row))
			}
			p := fields{row: row}
			ms, off := int64(p.uint(0)), p.uint(5)
			if p.err != nil {
				return 0, fmt.Errorf("event: %w", p.err)
			}
			found = append(found, sortedHit{hit: hit{src: src, ev: dbEvent(ref.line, ms, row[1], row[2], row[3])}, rank: roleRank[transcript.Role(row[1])],
				part: row[4], off: off})
		}
		sortHits(found)
	}
	if d.e.collect != nil {
		for i := range found {
			d.e.collect.refs = append(d.e.collect.refs, found[i].hit)
		}
	}
	return printRef(ref.id, ref.line, width, found, w), nil
}

// printRef prints the events of one line in full, the way showRef does.
func printRef(ref string, lineNo, width int, found []sortedHit, w io.Writer) int {
	if len(found) == 0 {
		fmt.Fprintf(w, "tl: no event at %s:%d (service record, unparsable line or out of range)\n", ref, lineNo)
		return exitNone
	}
	for i := range found {
		ev := &found[i].ev
		who := ""
		if ev.Who != "" {
			who = " " + ev.Who
		}
		text := maskEventText(ev.Shown())
		if width > 0 && len([]rune(text)) > width {
			text = maskedShort(text, width)
		}
		fmt.Fprintf(w, "--- %s %s:%d %s%s\n%s\n", ev.Time.Local().Format("02.01.2006 15:04:05 MST"), ref, ev.Line, ev.Role, who, text)
	}
	return 0
}

// eventsAt reads the events of one line from the file, as showRef does.
func eventsAt(path string, lineNo int) ([]transcript.Event, error) {
	var found []transcript.Event
	_, err := transcript.Read(path, nil, func(ev transcript.Event) {
		if ev.Line == lineNo {
			found = append(found, ev)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return found, nil
}
