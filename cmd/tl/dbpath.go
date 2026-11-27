package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"runtime/pprof"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aiseeq/akit/price"
	"github.com/aiseeq/akit/session"
	"github.com/aiseeq/akit/transcript"
	"github.com/aiseeq/akit/transcriptdb"
)

// The database path answers only from the tables the transcript loader fills — files, events and raw.

// backendDB and backendFiles are the values of TL_BACKEND; files is the default.
const (
	backendFiles = "files"
	backendDB    = "db"
)

// dbQueryTimeout bounds one call through the database; past it the call reads the files.
const dbQueryTimeout = 60 * time.Second

var errLiveSessionNotLoaded = errors.New("live session is not loaded in the database")
var errNonRegularTranscript = errors.New("transcript is not a regular file")

// filesTimeout bounds the first query, the list of files: it takes well under a second, so a server that hangs
// sends the call to the files in seconds rather than at dbQueryTimeout.
var filesTimeout = 10 * time.Second

// rawSettings is the profile of a search in the raw lines: lines of json reach megabytes, and with the default
// block of 65536 rows each of the 8 threads holds blocks of hundreds of megabytes and the query passes the 2 GiB
// limit; blocks of 1024 rows or 16 MB keep it under 200 MiB.
var rawSettings = url.Values{"max_threads": {"8"}, "max_block_size": {"1024"}, "preferred_block_size_bytes": {"16000000"}}

// dbForm tells whether the database path answers this call; the other forms read files.
func dbForm(opts options) bool {
	return !opts.version && !opts.worktime && !opts.openQuestions && opts.waitProg == "" && !opts.waitAll && opts.context == 0
}

// dbFile is one transcript's metadata in files FINAL.
type dbFile struct {
	id      uint64
	path    string
	session string
	parent  string
	kind    string
	project string
	cwd     string
	name    string
	desc    string
	mtime   time.Time
	first   time.Time
	lines   int
	evLines int
	loaded  time.Time // when the loader wrote this row
	bad     int       // lines the loader could not parse
}

type dbPath struct {
	c     *chClient
	ctx   context.Context
	opts  options
	e     env
	f     *filter
	files map[string]*dbFile
	trace io.Writer
	// traceMu keeps the trace lines of the queries that run at once whole.
	traceMu sync.Mutex
}

// runDB answers the call through the database into stdout and stderr; an error means no answer was printed and
// the caller reads the files.
func runDB(ctx context.Context, c *chClient, opts options, e env, refs []eventRef, stdout, stderr io.Writer) (int, error) {
	if profilePath := os.Getenv("TL_DB_PPROF"); profilePath != "" {
		profile, err := os.Create(profilePath)
		if err != nil {
			return 0, fmt.Errorf("create database CPU profile: %w", err)
		}
		if err := pprof.StartCPUProfile(profile); err != nil {
			_ = profile.Close()
			return 0, fmt.Errorf("start database CPU profile: %w", err)
		}
		defer func() {
			pprof.StopCPUProfile()
			_ = profile.Close()
		}()
	}
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
		return 0, err
	}
	if dbRetentionExceeded(opts, sources, d.files, d.e.now) {
		return 0, errors.New("requested data may predate retained events")
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

func dbRetentionExceeded(opts options, sources []source, files map[string]*dbFile, now time.Time) bool {
	cutoff := now.Add(-transcript.EventRetention + 24*time.Hour)
	if !opts.since.IsZero() && opts.since.Before(cutoff) && (opts.codex || opts.codexExt || !rolesKeptForever(opts, "claude")) {
		return true
	}
	for _, src := range sources {
		kind := "claude"
		if isCodexRollout(src.Path) {
			kind = "codex"
		}
		if rolesKeptForever(opts, kind) {
			continue
		}
		if f := files[src.Path]; f != nil && !f.first.IsZero() && f.first.Before(cutoff) {
			return true
		}
	}
	return false
}

func (d *dbPath) tracef(what string, start time.Time, n int) {
	if d.trace != nil {
		d.traceMu.Lock()
		defer d.traceMu.Unlock()
		fmt.Fprintf(d.trace, "tl: db %s: %d ms, %d\n", what, time.Since(start).Milliseconds(), n)
	}
}

func (d *dbPath) query(q chQuery) ([][]string, error) { return d.queryIn(d.ctx, q) }

func (d *dbPath) queryIn(ctx context.Context, q chQuery) ([][]string, error) {
	start := time.Now()
	rows, err := d.c.SelectTSV(ctx, q)
	if d.trace != nil {
		d.traceMu.Lock()
		defer d.traceMu.Unlock()
		fmt.Fprintf(d.trace, "tl: db %s %d ms, %d rows, err %v\n", q.Name, time.Since(start).Milliseconds(), len(rows), err)
	}
	return rows, err
}

func (d *dbPath) loadFiles() error {
	ctx, cancel := context.WithTimeout(d.ctx, filesTimeout)
	defer cancel()
	where, err := d.filePredicate()
	if err != nil {
		return err
	}
	rows, err := d.queryIn(ctx, chQuery{Name: "files", SQL: "SELECT file_id, path, name, description, lines, ev_lines, " +
		"intDiv(ver, 1000000), bad, toUnixTimestamp64Milli(toDateTime64(mtime, 3, 'UTC')), " +
		"toUnixTimestamp64Milli(first_ts), session, parent, kind, project, cwd FROM files FINAL WHERE " + where.sql,
		Params: where.params})
	if err != nil {
		return err
	}
	d.files = make(map[string]*dbFile, len(rows))
	for _, row := range rows {
		if len(row) != 15 {
			return fmt.Errorf("files: %d fields in a row, want 15", len(row))
		}
		p := fields{row: row}
		f := &dbFile{id: p.uint(0), path: row[1], name: row[2], desc: row[3], lines: p.int(4), evLines: p.int(5),
			loaded: time.UnixMilli(int64(p.uint(6))), bad: p.int(7), mtime: time.UnixMilli(int64(p.uint(8))),
			session: row[10], parent: row[11], kind: row[12], project: row[13], cwd: row[14]}
		if first := p.uint(9); first != 0 {
			f.first = time.UnixMilli(int64(first))
		}
		if p.err != nil {
			return fmt.Errorf("files: %w", p.err)
		}
		// A path written again after its file was deleted gets a new id: the later row is the file there now.
		if old := d.files[f.path]; old != nil && old.loaded.After(f.loaded) {
			continue
		}
		d.files[f.path] = f
	}
	return nil
}

func (d *dbPath) filePredicate() (where, error) {
	if d.opts.all {
		return d.allFilePredicate(), nil
	}
	return d.sessionFilePredicate()
}

func (d *dbPath) allFilePredicate() where {
	w := where{params: map[string]string{}}
	since := d.opts.since
	if since.IsZero() {
		since = d.e.now.Add(-sinceDefault)
	}
	w.params["file_since"] = chTime(since)
	conds := []string{"mtime >= {file_since:DateTime64(3, 'UTC')}"}
	if !d.opts.until.IsZero() {
		w.params["file_until"] = chTime(d.opts.until)
		conds = append(conds, "first_ts <= {file_until:DateTime64(3, 'UTC')}")
	}
	if !d.opts.codex {
		conds = append(conds, "kind != 'codex'")
	}
	if !d.opts.codexExt {
		conds = append(conds, "kind != '"+transcriptdb.KindCodexExt+"'")
	}
	if d.opts.noSubagents {
		conds = append(conds, "kind != 'sub'")
	}
	if d.opts.project != "" {
		w.params["project"] = d.opts.project
		conds = append(conds, "((kind IN "+codexKindsSQL+" AND (position(path, {project:String}) > 0 OR position(cwd, {project:String}) > 0)) OR (kind NOT IN "+codexKindsSQL+" AND (position(path, {project:String}) > 0 OR position(project, {project:String}) > 0)) )")
	}
	w.sql = strings.Join(conds, " AND ")
	return w
}

func (d *dbPath) sessionFilePredicate() (where, error) {
	w := where{params: map[string]string{}}
	args := d.opts.args
	if len(args) == 0 && d.e.sessionID != "" {
		args = []string{d.e.sessionID}
	}
	if len(args) == 0 {
		return where{sql: "0", params: w.params}, nil
	}
	var ids, paths []string
	alive, aliveErr := session.ForHome(d.e.home).Alive()
	if aliveErr != nil && len(alive) == 0 {
		return where{}, fmt.Errorf("read live session names: %w", aliveErr)
	}
	if aliveErr != nil && d.trace != nil {
		fmt.Fprintf(d.trace, "tl: db live session registry: %v\n", aliveErr)
	}
	for _, arg := range args {
		if ref := refArg.FindStringSubmatch(arg); ref != nil {
			arg = ref[1]
		}
		arg = sessionArg(arg, d.e.home)
		if strings.HasSuffix(arg, ".jsonl") || strings.Contains(arg, "/") {
			paths = append(paths, arg)
			continue
		}
		if in, ok := alive[arg]; ok {
			ids = append(ids, in.SessionID)
		} else if len(arg) >= 6 {
			ids = append(ids, arg)
		}
	}
	var conds []string
	if len(paths) > 0 {
		w.params["find_paths"] = chStrings(paths)
		conds = append(conds, "path IN {find_paths:Array(String)}")
	}
	for i, id := range ids {
		key := fmt.Sprintf("find_id_%d", i)
		w.params[key] = id
		// a session id finds what the files find (transcriptPatterns): codex-ext only by its path
		conds = append(conds, "(kind != '"+transcriptdb.KindCodexExt+"' AND (startsWith(session, {"+key+":String}) OR position(path, {"+key+":String}) > 0))")
	}
	if len(conds) == 0 {
		return where{sql: "0", params: w.params}, nil
	}
	if d.opts.subagents {
		conds = append(conds, "parent IN (SELECT session FROM files FINAL WHERE "+strings.Join(conds, " OR ")+")")
	}
	w.sql = "(" + strings.Join(conds, " OR ") + ")"
	return w, nil
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

// resolve selects only names and metadata in files FINAL; an unlisted disk path is invisible here.
func (d *dbPath) resolve() ([]source, error) {
	if d.opts.all {
		return d.resolveAll()
	}
	return d.resolveSessions()
}

func (d *dbPath) resolveSessions() ([]source, error) {
	args := d.opts.args
	if len(args) == 0 {
		if d.e.sessionID == "" {
			return nil, errors.New("no SESSION and not inside a Claude Code session; name one or use --all")
		}
		args = []string{d.e.sessionID}
	}
	var out []source
	for _, arg := range args {
		path, err := d.find(arg)
		if err != nil {
			return nil, err
		}
		src, err := transcript.SourceAt(path)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
		if d.opts.subagents {
			subs, err := transcript.SubSources(path)
			if err != nil {
				return nil, err
			}
			out = append(out, subs...)
		}
	}
	return out, nil
}

func (d *dbPath) resolveAll() ([]source, error) {
	since := d.opts.since
	if since.IsZero() {
		since = d.e.now.Add(-sinceDefault)
	}
	var out []source
	for _, f := range d.files {
		if !d.selectedAll(f, since) {
			continue
		}
		out = append(out, source{Path: f.path, ID: f.session, ParentID: f.parent, Description: f.desc})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (d *dbPath) selectedAll(f *dbFile, since time.Time) bool {
	if f.mtime.Before(since) || !d.opts.until.IsZero() && f.first.After(d.opts.until) {
		return false
	}
	if f.kind == "codex" && !d.opts.codex || f.kind == transcriptdb.KindCodexExt && !d.opts.codexExt || f.kind == "sub" && d.opts.noSubagents {
		return false
	}
	if d.opts.project == "" {
		return true
	}
	if transcriptdb.IsCodexKind(f.kind) {
		return strings.Contains(f.path, d.opts.project) || strings.Contains(f.cwd, d.opts.project)
	}
	return strings.Contains(f.path, d.opts.project) || strings.Contains(f.project, d.opts.project)
}

func (d *dbPath) find(arg string) (string, error) {
	arg = sessionArg(arg, d.e.home)
	if strings.HasSuffix(arg, ".jsonl") || strings.Contains(arg, "/") {
		info, err := os.Stat(arg)
		if err != nil {
			return "", fmt.Errorf("transcript %s: %w", arg, err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("%w: %s", errNonRegularTranscript, arg)
		}
		if d.files[arg] != nil {
			return arg, nil
		}
		return "", fmt.Errorf("transcript %s is not in the database", arg)
	}
	// Match the file resolver: live names take precedence over transcript names and ids.
	alive, aliveErr := session.ForHome(d.e.home).Alive()
	return d.findAfterLive(arg, alive, aliveErr)
}

func (d *dbPath) findAfterLive(arg string, alive map[string]session.Info, aliveErr error) (string, error) {
	if in, ok := alive[arg]; ok {
		var path string
		var newest time.Time
		for p, f := range d.files {
			if sourceFullID(source{Path: p, ID: p}) == in.SessionID && dbTranscriptPresent(p) && (path == "" || f.mtime.After(newest)) {
				path, newest = p, f.mtime
			}
		}
		if path != "" {
			return path, nil
		}
		return "", fmt.Errorf("%w: %q", errLiveSessionNotLoaded, arg)
	}
	if len(arg) < 6 {
		return "", fmt.Errorf("%q: session id must be at least 6 characters", arg)
	}
	var matches []string
	for path, f := range d.files {
		if f.kind == transcriptdb.KindCodexExt {
			continue // named by its path only, as in the files
		}
		id := f.session
		if id == "" {
			id = sourceFullID(source{Path: path, ID: path})
		}
		if (strings.HasPrefix(id, arg) || strings.HasPrefix(sourceFullID(source{Path: path, ID: path}), arg)) && dbTranscriptPresent(path) {
			matches = append(matches, path)
		}
	}
	sort.Strings(matches)
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		if aliveErr != nil {
			return "", fmt.Errorf("%q: no transcript with this id; live sessions read with error: %w", arg, aliveErr)
		}
		return "", fmt.Errorf("%q: no transcript with this id in the database", arg)
	}
	return "", fmt.Errorf("%q is ambiguous: matches %d transcripts; give more of the id", arg, len(matches))
}

// dbTranscriptPresent excludes stale names for files the loader has already seen disappear. It only stats
// candidate rows from files FINAL; it never opens a transcript or walks its parent folders.
func dbTranscriptPresent(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, os.ErrNotExist)
}

// linkNames includes files outside this call's project and time window: short links
// must be unique in the same full search area as the file backend.
func (d *dbPath) linkNames() ([]string, error) {
	// codex-ext is outside the search area of the files (transcriptPatterns): its links are paths on both
	rows, err := d.query(chQuery{Name: "link_names", SQL: "SELECT path, session, kind FROM files FINAL WHERE kind != '" + transcriptdb.KindCodexExt + "'"})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		if len(row) != 3 {
			return nil, fmt.Errorf("link names: %d fields in a row, want 3", len(row))
		}
		names = append(names, row[0])
	}
	sort.Strings(names)
	return names, nil
}

// codexKindsSQL — the kinds of Codex rollouts as an SQL list: their project filter reads cwd.
const codexKindsSQL = "('codex', '" + transcriptdb.KindCodexExt + "')"

// kindOfSource is the kind column of the events of a source the database does not name: a rollout under a Codex
// root outside worker is codex-ext there, its files row tells (storedHits).
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

func (d *dbPath) where(ids []uint64, kinds []string) (where, error) {
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
	filter, err := eventFilter(opts, w.params)
	if err != nil {
		return w, err
	}
	conds = append(conds, filter...)
	if opts.raw != "" {
		cond, params, err := rawCondition(opts.raw)
		if err != nil {
			return w, err
		}
		conds = append(conds, "(file_id, line) IN (SELECT file_id, line "+rawFromSQL+cond+")")
		maps.Copy(w.params, params)
		maps.Copy(w.settings, rawSettings)
	}
	w.sql = strings.Join(conds, " AND ")
	return w, nil
}

// rawFromSQL is the raw lines of the files that pass the condition after it: the event query and the raw summary
// select the lines of --raw the same way.
const rawFromSQL = "FROM raw WHERE file_id IN {files:Array(UInt64)} AND "

// eventFilter is the conditions of --since, --until and --grep over the events table; their parameters go to params.
func eventFilter(opts options, params map[string]string) ([]string, error) {
	var conds []string
	if !opts.since.IsZero() {
		// ts holds milliseconds: an event at the same millisecond as since but before it in finer time is out
		since := opts.since.Truncate(time.Millisecond)
		if since.Before(opts.since) {
			since = since.Add(time.Millisecond)
		}
		params["since"] = chTime(since)
		conds = append(conds, "ts >= {since:DateTime64(3, 'UTC')}")
	}
	if !opts.until.IsZero() {
		params["until"] = chTime(opts.until)
		conds = append(conds, "ts <= {until:DateTime64(3, 'UTC')}")
	}
	if opts.grep != "" {
		cond, grepParams, err := grepCondition(opts.grep)
		if err != nil {
			return nil, err
		}
		conds = append(conds, cond)
		maps.Copy(params, grepParams)
	}
	return conds, nil
}

// stored is a source and what the database has of it.
type stored struct {
	src  source
	file *dbFile
}

// result is scan of the file path from the database: the events of the sources in the window, sorted the same
// way, with the link ids and names of the sources.
func (d *dbPath) result(sources []source) (result, error) {
	// The link ids and the events do not depend on each other: the ids are worked out while the server answers.
	links := make(chan result, 1)
	var linkErr error
	go func() {
		start := time.Now()
		names, err := d.linkNames()
		if err != nil {
			linkErr = err
			links <- result{}
			return
		}
		liveNames, liveErr := liveSessionNames(d.e.home)
		res := result{names: map[string]string{}, linkIDs: sourceLinkIDs(sources, names, d.e.home, liveNames),
			searchPaths: names, liveNames: liveNames, liveErr: liveErr, files: len(sources)}
		for path, f := range d.files {
			if f.name != "" {
				res.names[path] = f.name
			}
		}
		d.tracef("link ids", start, len(res.linkIDs))
		links <- res
	}()
	var inDB []stored
	var missing error
	for _, src := range sources {
		f := d.files[src.Path]
		if f == nil {
			missing = fmt.Errorf("transcript %s is not in the database", src.Path)
			break
		}
		inDB = append(inDB, stored{src: src, file: f})
	}
	var all []sortedHit
	var hitsErr error
	var rawDone <-chan rawSummary
	if missing == nil && len(inDB) > 0 {
		var cancelRaw context.CancelFunc
		rawDone, cancelRaw = d.startRawSummary(inDB)
		defer cancelRaw()
		all, hitsErr = d.storedHits(inDB)
	}
	res := <-links
	if linkErr != nil {
		return result{}, linkErr
	}
	if missing != nil {
		return res, missing
	}
	if hitsErr != nil {
		return res, hitsErr
	}
	if rawDone != nil {
		res.raw = <-rawDone
	}
	bad := map[string]badLines{}
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
	start := time.Now()
	sortHits(all)
	d.tracef("sort", start, len(all))
	res.hits = make([]hit, len(all))
	for i := range all {
		res.hits[i] = all[i].hit
	}
	return res, nil
}

// startRawSummary counts on the server, beside the event query, what --raw matched but the listing leaves out
// (rawSummary). The channel is nil when there is nothing to count: no --raw, or --usage, whose report has no such
// note. The cancel func stops the count once the call no longer waits for it.
func (d *dbPath) startRawSummary(inDB []stored) (<-chan rawSummary, context.CancelFunc) {
	if d.opts.raw == "" || d.opts.usage {
		return nil, func() {}
	}
	// a slow count drops only the note, so it gives up well before the call does
	ctx, cancel := context.WithTimeout(d.ctx, dbQueryTimeout/2)
	done := make(chan rawSummary, 1)
	go func() { done <- d.rawSummary(ctx, inDB) }()
	return done, cancel
}

func (d *dbPath) rawSummary(ctx context.Context, inDB []stored) rawSummary {
	q, err := d.rawSummaryQuery(inDB)
	if err != nil {
		return rawSummary{err: err}
	}
	rows, err := d.queryIn(ctx, q)
	if err != nil {
		return rawSummary{err: err}
	}
	return parseRawSummary(rows, d.opts.roles)
}

// rawSummaryQuery is rawSummarySQL over the files of inDB with the filters of the call.
func (d *dbPath) rawSummaryQuery(inDB []stored) (chQuery, error) {
	ids := make([]uint64, len(inDB))
	evLines := make([]uint64, len(inDB))
	kindSet := map[string]bool{}
	for i, s := range inDB {
		ids[i], evLines[i] = s.file.id, uint64(s.file.evLines)
		kindSet[storedKind(s)] = true
	}
	names := make([]string, len(transcript.Roles))
	for i, role := range transcript.Roles {
		names[i] = string(role)
	}
	params := map[string]string{"files": chUints(ids), "ev_lines": chUints(evLines),
		"kinds": chStrings(slices.Sorted(maps.Keys(kindSet))), "role_names": chStrings(names),
		"visible": strconv.FormatUint(uint64(roleBits(d.opts.roles)), 10)}
	rawCond, rawParams, err := rawCondition(d.opts.raw)
	if err != nil {
		return chQuery{}, err
	}
	maps.Copy(params, rawParams)
	filter, err := eventFilter(d.opts, params)
	if err != nil {
		return chQuery{}, err
	}
	return chQuery{Name: "raw-hidden", SQL: rawSummarySQL(rawCond, filter), Params: params, Settings: rawSettings}, nil
}

// rawSummarySQL counts in one pass what --raw (rawCond over the raw lines) matched beyond the listing: the matched
// lines whose events that pass filter are all of hidden roles, and the bits of those roles (bit i is
// transcript.Roles[i]). A line counts only up to ev_lines of its file, as the event listing does. The raw lines are
// read once, under the condition of the event query (rawFromSQL); the events of the files are grouped by line
// without it, which costs far less than reading the raw lines again, and filter stays in WHERE, where the ngram
// index of --grep answers.
func rawSummarySQL(rawCond string, filter []string) string {
	passing := ""
	if len(filter) > 0 {
		passing = " AND " + strings.Join(filter, " AND ")
	}
	const hiddenOnly = "kept AND roles != 0 AND bitAnd(roles, {visible:UInt16}) = 0"
	return "SELECT countIf(" + hiddenOnly + "), groupBitOrIf(roles, " + hiddenOnly + ") " +
		"FROM (SELECT max(raw_match) AND line <= transform(file_id, {files:Array(UInt64)}, {ev_lines:Array(UInt64)}, toUInt64(0)) AS kept, " +
		"groupBitOr(role_bit) AS roles FROM (" +
		"SELECT file_id, line, true AS raw_match, toUInt16(0) AS role_bit " + rawFromSQL + rawCond +
		" UNION ALL SELECT file_id, line, false, bitShiftLeft(toUInt16(1), indexOf({role_names:Array(String)}, toString(role)) - 1) " +
		"FROM events WHERE file_id IN {files:Array(UInt64)} AND kind IN {kinds:Array(String)}" + passing + ") GROUP BY file_id, line)"
}

// roleBits is the set of roles as the bits of rawSummarySQL.
func roleBits(roles map[transcript.Role]bool) uint16 {
	var bits uint16
	for i, role := range transcript.Roles {
		if roles[role] {
			bits |= 1 << i
		}
	}
	return bits
}

// parseRawSummary reads the one row of rawSummarySQL; the roles it names are those visible does not show.
func parseRawSummary(rows [][]string, visible map[transcript.Role]bool) rawSummary {
	if len(rows) != 1 || len(rows[0]) != 2 {
		return rawSummary{err: fmt.Errorf("raw-hidden: got %d rows, want one row of 2 fields", len(rows))}
	}
	p := fields{row: rows[0]}
	out := rawSummary{hidden: p.int(0)}
	bits := p.uint(1)
	if p.err != nil {
		return rawSummary{err: fmt.Errorf("raw-hidden: %w", p.err)}
	}
	for i, role := range transcript.Roles {
		if bits&(1<<i) != 0 && !visible[role] {
			out.roles = append(out.roles, role)
		}
	}
	return out
}

// badLines are the unparsable lines of one file that pass the active raw prefilter.
type badLines struct {
	n     int
	first error
}

type storedRawLine struct {
	n int
	b []byte
}

// badLines replays the stored raw lines to count the bad ones that pass the active filter. Rollouts need a
// prescan of their complete contents before Feed, as the file reader does, to recognize message event records.
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
	rows, err := d.query(chQuery{Name: "bad", SQL: "SELECT file_id, line, raw FROM raw WHERE file_id IN {files:Array(UInt64)} " +
		"ORDER BY file_id, line", Params: map[string]string{"files": chUints(ids)}, Settings: rawSettings})
	if err != nil {
		return err
	}
	all := make(map[uint64][]storedRawLine, len(byID))
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
		if !ok || line > s.file.lines {
			continue
		}
		all[id] = append(all[id], storedRawLine{n: line, b: []byte(row[2])})
	}
	for id, s := range byID {
		b, err := d.storedBadLines(s, all[id])
		if err != nil {
			return err
		}
		out[s.src.Path] = b
	}
	return nil
}

func (d *dbPath) storedBadLines(s stored, lines []storedRawLine) (badLines, error) {
	fl := transcript.NewFollow()
	codex := isCodexRollout(s.src.Path)
	if codex {
		for _, line := range lines {
			if err := fl.Prescan(append(bytes.Clone(line.b), '\n')); err != nil {
				return badLines{}, fmt.Errorf("bad: prescan stored line %d: %w", line.n, err)
			}
		}
	}
	messageUser, messageAgent := fl.MessageEvents()
	var bad badLines
	for _, line := range lines {
		before := fl.Meta()
		if err := fl.Feed(append(bytes.Clone(line.b), '\n'), func(transcript.Event) {}); err != nil {
			return badLines{}, fmt.Errorf("bad: parse stored line %d: %w", line.n, err)
		}
		if fl.Meta().Bad <= before.Bad || !d.f.keepsBad(line.b) {
			continue
		}
		bad.n++
		if bad.first == nil {
			first, err := storedBadLineError(line, codex, messageUser, messageAgent)
			if err != nil {
				return badLines{}, err
			}
			bad.first = first
		}
	}
	return bad, nil
}

func storedBadLineError(line storedRawLine, codex, messageUser, messageAgent bool) (error, error) {
	candidate := transcript.NewFollow()
	if codex {
		seed := []byte(`{"type":"session_meta"`)
		if messageUser {
			seed = append(seed, []byte(`,"type":"event_msg","payload":{"type":"user_message"}`)...)
		}
		if messageAgent {
			seed = append(seed, []byte(`,"type":"event_msg","payload":{"type":"agent_message"}`)...)
		}
		seed = append(seed, '}', '\n')
		if err := candidate.Prescan(seed); err != nil {
			return nil, err
		}
	}
	if err := candidate.Feed(append(bytes.Clone(line.b), '\n'), func(transcript.Event) {}); err != nil {
		return nil, fmt.Errorf("bad: parse stored line %d: %w", line.n, err)
	}
	cause := "unparsable line"
	if first := candidate.Meta().FirstBad; first != nil {
		_, cause, _ = strings.Cut(first.Error(), ": ")
	}
	return fmt.Errorf("line %d: %s", line.n, cause), nil
}

// sortedHit is a hit with the stored sequence and deterministic tie-breakers for events of one line.
type sortedHit struct {
	hit
	seq  uint64
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
// them. It sorts indices: a hit is large, and a stable sort in place moves every one of them many times.
func sortHits(hits []sortedHit) {
	order := make([]int, len(hits))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(i, j int) int {
		a, b := &hits[i], &hits[j]
		if c := a.ev.Time.Compare(b.ev.Time); c != 0 {
			return c
		}
		return cmp.Or(cmp.Compare(a.src.Path, b.src.Path), cmp.Compare(a.ev.Line, b.ev.Line), cmp.Compare(a.seq, b.seq), cmp.Compare(a.rank, b.rank),
			cmp.Compare(a.part, b.part), cmp.Compare(a.off, b.off), cmp.Compare(i, j))
	})
	sorted := make([]sortedHit, len(hits))
	for to, from := range order {
		sorted[to] = hits[from]
	}
	copy(hits, sorted)
}

// dbEvent is an event from its columns in the events table; text is Shown() there. The fields set here are the
// columns the table has: a new field of Event reaches this path with a new column, not with a change here.
func dbEvent(line int, ms int64, role, who, text string) transcript.Event {
	return transcript.Event{ //nolint:scattered-construction // only the columns of the events table exist here
		Line: line, Time: msToTime(ms), Role: transcript.Role(role), Who: who, Text: text}
}

// storedKind is the kind of a stored file, from its source when the row has none.
func storedKind(s stored) string {
	if s.file.kind != "" {
		return s.file.kind
	}
	return kindOfSource(s.src)
}

func (d *dbPath) storedHits(inDB []stored) ([]sortedHit, error) {
	ids := make([]uint64, 0, len(inDB))
	kindSet := map[string]bool{}
	byID := make(map[uint64]stored, len(inDB))
	for _, s := range inDB {
		ids = append(ids, s.file.id)
		kindSet[storedKind(s)] = true
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
	return hits, nil
}

// textCut is how much of a long text a listing reads: the first cut of maskedShort and its first widening.
func textCut(width int) int { return 2 * (width + maskWindow) }

// wholeText is the longest text, in characters, a listing reads whole and cuts itself when its row has no
// flat_len: one loaded before the column, until it is recounted. The server counts the flattened length of such a
// text with regular expressions at tens of MB/s, slower than sending the text and counting it here; only a longer
// text comes as its start and that count.
var wholeText = 1 << 16

// eventTextSQL are the text and the flattened length of an event in a listing: a text up to the cut comes whole
// with 0, a longer one as its start and the length transcript.Short gives it. The loader stores that length in
// flat_len; a row without it (0 there) comes whole up to wholeText, a longer one gets it counted (flatLenSQL).
const eventTextSQL = "if((lengthUTF8(text) <= {cut:UInt32} OR (flat_len = 0 AND lengthUTF8(text) <= {whole:UInt32})) AS whole_text, " +
	"text, substringUTF8(text, 1, {cut:UInt32})), if(whole_text, 0, if(flat_len > 0, flat_len, " + flatLenSQL + "))"

// flatLenSQL is the length of transcript.Short(text, 0) in characters: spaces as unicode.IsSpace has them,
// trimmed, a line break as " ⏎ ", runs of spaces as one.
const flatLenSQL = `lengthUTF8(arrayStringConcat(arrayFilter(x -> x != '', splitByRegexp({ws:String}, ` +
	`replaceAll(replaceRegexpAll(text, {trim:String}, ''), '\n', ' ⏎ '))), ' '))`

const spaceClass = `[\t\n\v\f\r \x{85}\x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]`

func (d *dbPath) eventRows(ids []uint64, kinds []string, byID map[uint64]stored) ([]sortedHit, error) {
	width := d.opts.width
	text := "text, 0"
	if width > 0 {
		text = eventTextSQL
	}
	rows, err := d.selectEvents("events", func(w where) chQuery {
		if width > 0 {
			w.params["cut"] = strconv.Itoa(textCut(width))
			w.params["whole"] = strconv.Itoa(max(wholeText, textCut(width)))
			w.params["ws"] = chString(spaceClass + "+")
			w.params["trim"] = chString("^" + spaceClass + "+|" + spaceClass + "+$")
		}
		return chQuery{SQL: "SELECT file_id, line, seq, toUnixTimestamp64Milli(ts), role, who, subtype, " + text + ", _part, _part_offset FROM events WHERE " + w.sql,
			Params: w.params, Settings: w.settings}
	}, ids, kinds)
	if err != nil {
		return nil, err
	}
	hits := make([]sortedHit, 0, len(rows))
	// rows that came as their start: the hit and the flattened length of its whole text, cut below
	type start struct{ at, flat int }
	var starts []start
	for _, row := range rows {
		if len(row) != 11 {
			return nil, fmt.Errorf("events: %d fields in a row, want 11", len(row))
		}
		p := fields{row: row}
		id, line, seq, ms, flat, off := p.uint(0), p.int(1), p.uint(2), int64(p.uint(3)), p.int(8), p.uint(10)
		if p.err != nil {
			return nil, fmt.Errorf("events: %w", p.err)
		}
		s, ok := byID[id]
		if !ok || line > s.file.evLines {
			continue // rows beyond evLines are absent from the event table until the loader indexes them
		}
		h := sortedHit{hit: hit{src: s.src, ev: dbEvent(line, ms, row[4], row[5], row[7])}, seq: seq, rank: roleRank[transcript.Role(row[4])], part: row[9],
			off: off, fileID: id}
		h.ev.Subtype = row[6]
		if flat > 0 {
			starts = append(starts, start{at: len(hits), flat: flat})
		}
		hits = append(hits, h)
	}
	// masking the starts is most of the time of a long listing, and the starts do not depend on each other
	cutStart := time.Now()
	cuts := make([]string, len(starts))
	cutOK := make([]bool, len(starts))
	eachParallel(len(starts), func(i int) {
		if s := starts[i]; s.flat > width {
			flat, _ := flatten(hits[s.at].ev.Text, -1)
			cuts[i], cutOK[i] = maskedCut(flat, s.flat, width)
		}
	})
	d.tracef("cuts", cutStart, len(starts))
	var long []int // rows whose start does not decide the cut
	for i, s := range starts {
		if cutOK[i] {
			hits[s.at].cut = &cuts[i]
		} else {
			long = append(long, s.at)
		}
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
	rows, err := d.query(chQuery{Name: "texts", SQL: "SELECT file_id, line, role, _part, _part_offset, text FROM events WHERE file_id IN {files:Array(UInt64)} " +
		"AND (file_id, line) IN {pairs:Array(Tuple(UInt64, UInt32))}", Params: map[string]string{"files": chUints(idList), "pairs": pairs.String()}})
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

// selectEvents runs a query over the events of the files the call filters.
func (d *dbPath) selectEvents(name string, build func(where) chQuery, ids []uint64, kinds []string) ([][]string, error) {
	w, err := d.where(ids, kinds)
	if err != nil {
		return nil, err
	}
	q := build(w)
	q.Name = name
	return d.query(q)
}

func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// usageEventSQL keeps the events of a reply with usage: it has an id or a model. A Codex token report passes without
// them too: one loaded before the usage columns existed has none, and it must not be missed in silence.
const usageEventSQL = "(reply_id != '' OR request_id != '' OR model != '' OR (kind IN " + codexKindsSQL + " AND who = 'token_count'))"

// usageVersionSQL are the columns of one version of the usage of a reply. Within a file only the first event of a
// version decides what usageReplies counts, the rest repeat it; a reply without ids is counted at every event.
const usageVersionSQL = "model, reply_id, request_id, in_tok, out_tok, cache_read, cache_write, cache_write_5m, cache_write_1h, fast, geo"

// usageHits reads the usage of the replies from their events in the window; usageReplies counts them as it counts
// the events of the files. The usage table has one row of a reply, the event with the most output: a window that
// starts or ends between the events of a reply cannot be answered from it.
func (d *dbPath) usageHits(ids []uint64, kinds []string, byID map[uint64]stored) ([]sortedHit, error) {
	rows, err := d.selectEvents("usage", func(w where) chQuery {
		return chQuery{SQL: "SELECT file_id, usage_session = '' AS legacy, " + usageVersionSQL + ", toUnixTimestamp64Milli((min((ts, line)) AS first).1), first.2 FROM events WHERE " +
			w.sql + " AND " + usageEventSQL + " GROUP BY file_id, legacy, " + usageVersionSQL + ", if(reply_id = '' AND request_id = '', (line, seq), (0, 0))",
			Params: w.params, Settings: w.settings}
	}, ids, kinds)
	if err != nil {
		return nil, err
	}
	hits := make([]sortedHit, 0, len(rows))
	for _, row := range rows {
		if len(row) != 15 {
			return nil, fmt.Errorf("usage: %d fields in a row, want 15", len(row))
		}
		p := fields{row: row}
		id, legacy, ms, line := p.uint(0), p.uint(1), int64(p.uint(13)), p.int(14)
		in, out, read, write, w5, w1 := p.uint(5), p.uint(6), p.uint(7), p.uint(8), p.uint(9), p.uint(10)
		if p.err != nil {
			return nil, fmt.Errorf("usage: %w", p.err)
		}
		if legacy != 0 {
			return nil, errors.New("usage session metadata is incomplete")
		}
		reply := &transcript.Reply{Model: row[2], ID: row[3], RequestID: row[4]}
		s, ok := byID[id]
		if !ok || reply.Model == "" && reply.ID == "" && reply.RequestID == "" {
			continue // not a file of this call, or a token report that is no call
		}
		u := price.NewUsage(int64(in), int64(out), int64(read), int64(write), int64(w5), int64(w1), row[11] == "true", row[12])
		reply.Usage = &u
		h := sortedHit{hit: hit{src: s.src, ev: dbEvent(line, ms, string(transcript.Agent), "", "")}, fileID: id}
		h.ev.Reply = reply
		hits = append(hits, h)
	}
	return hits, nil
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
	f := d.files[path]
	var found []sortedHit
	if f == nil {
		fmt.Fprintf(stderr, "tl: transcript %s is not in the database\n", path)
		return exitNone, nil
	}
	src := source{Path: path, ID: f.session, ParentID: f.parent}
	if ref.line > f.evLines {
		// Event references may read one requested line from disk; they are not a search path.
		evs, err := eventsAt(path, ref.line)
		if err != nil {
			fmt.Fprintf(stderr, "tl: %v\n", err)
			return exitUsage, nil
		}
		for i := range evs {
			found = append(found, sortedHit{hit: hit{src: src, ev: evs[i]}, off: uint64(i)})
		}
	} else {
		rows, err := d.query(chQuery{Name: "event", SQL: "SELECT toUnixTimestamp64Milli(ts), seq, role, who, text, _part, _part_offset FROM events " +
			"WHERE file_id = {file:UInt64} AND line = {line:UInt32}", Params: map[string]string{"file": strconv.FormatUint(f.id, 10), "line": strconv.Itoa(ref.line)}})
		if err != nil {
			return 0, err
		}
		for _, row := range rows {
			if len(row) != 7 {
				return 0, fmt.Errorf("event: %d fields in a row, want 7", len(row))
			}
			p := fields{row: row}
			ms, seq, off := int64(p.uint(0)), p.uint(1), p.uint(6)
			if p.err != nil {
				return 0, fmt.Errorf("event: %w", p.err)
			}
			found = append(found, sortedHit{hit: hit{src: src, ev: dbEvent(ref.line, ms, row[2], row[3], row[4])}, seq: seq, rank: roleRank[transcript.Role(row[2])],
				part: row[5], off: off})
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
	ref = strings.TrimPrefix(ref, idPrefix)
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

// rolesKeptForever tells whether every role the search asks for is one the loader keeps past the event retention
// in transcripts of this kind; a search without a role filter asks for all of them.
func rolesKeptForever(opts options, kind string) bool {
	asked := 0
	for role, on := range opts.roles {
		if !on {
			continue
		}
		if !transcript.EventKeptForever(kind, role) {
			return false
		}
		asked++
	}
	return asked > 0
}
