package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/session"
	"github.com/aiseeq/akit/transcript"
)

type source = transcript.Source

// sinceDefault — окно --all без --since: за неделю транскриптов около 2 ГБ, за всё время — больше 10.
const sinceDefault = 7 * 24 * time.Hour

func resolve(opts options, e env) ([]source, error) {
	return resolveWith(opts, e, func(since time.Time) ([]source, error) {
		sources, err := transcript.AllSourcesWindow(e.home, since, opts.until, opts.project, opts.codex, !opts.noSubagents)
		if err != nil || !opts.codexExt {
			return sources, err
		}
		roots, err := transcript.CodexRoots(e.home)
		if err != nil {
			return nil, err
		}
		ext, err := transcript.CodexRootSources(roots, since, opts.until, opts.project)
		if err != nil {
			return nil, err
		}
		sources = append(sources, ext...)
		sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
		return sources, nil
	}, func(arg string) (string, error) { return find(arg, e.home) })
}

// resolveWith is resolve over a listing of the --all window and a find of a named session: the folders or the
// database's list of files give them.
func resolveWith(opts options, e env, window func(since time.Time) ([]source, error), find func(string) (string, error)) ([]source, error) {
	if opts.all {
		if len(opts.args) > 0 {
			return nil, errors.New("--all and SESSION together; pick one")
		}
		since := opts.since
		if since.IsZero() {
			since = e.now.Add(-sinceDefault)
		}
		return window(since)
	}
	args := opts.args
	if len(args) == 0 {
		if e.sessionID == "" {
			return nil, errors.New("no SESSION and not inside a Claude Code session; name one or use --all")
		}
		args = []string{e.sessionID}
	}
	var out []source
	for _, a := range args {
		path, err := find(a)
		if err != nil {
			return nil, err
		}
		src, err := transcript.SourceAt(path)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
		if opts.subagents {
			subs, err := transcript.SubSources(path)
			if err != nil {
				return nil, err
			}
			out = append(out, subs...)
		}
	}
	return out, nil
}

// find — файл транскрипта по пути, имени живой сессии или id (префиксу id).
func find(arg, home string) (string, error) {
	return findIn(arg, home, func() ([]string, error) { return transcriptPaths(home) })
}

// findIn is find over the transcript names that names lists: the folders, or the database's list of files.
func findIn(arg, home string, names func() ([]string, error)) (string, error) {
	arg = sessionArg(arg, home)
	if strings.HasSuffix(arg, ".jsonl") || strings.Contains(arg, "/") {
		if _, err := os.Stat(arg); err != nil {
			return "", fmt.Errorf("transcript %s: %w", arg, err)
		}
		return arg, nil
	}
	projects := session.ProjectsDir(home)
	// Реестр читается частично, если запись пишется прямо сейчас: его ошибка нужна, только если имя не нашлось.
	alive, aliveErr := session.ForHome(home).Alive()
	if in, ok := alive[arg]; ok {
		t, err := session.Describe(projects, in.SessionID)
		if err != nil {
			return "", fmt.Errorf("session %s: %w", arg, err)
		}
		return t.Path, nil
	}
	if len(arg) < 6 || strings.ContainsAny(arg, "*?[") {
		return "", fmt.Errorf("%q: not a live session name, and too short for a session id (6+ chars); find sessions with --grep or --all --project", arg)
	}
	paths, err := names()
	if err != nil {
		return "", fmt.Errorf("find %s: %w", arg, err)
	}
	matches := make([]string, 0)
	for _, path := range paths {
		if strings.HasPrefix(sourceFullID(source{Path: path, ID: path}), arg) {
			matches = append(matches, path)
		}
	}
	switch len(matches) {
	case 0:
		if aliveErr != nil {
			return "", fmt.Errorf("%q: no transcript with this id; live sessions read with error: %w", arg, aliveErr)
		}
		return "", fmt.Errorf("%q: no live session with this name and no transcript with this id; find sessions with --grep or --all --project", arg)
	case 1:
		return matches[0], nil
	}
	ids := make([]string, len(matches))
	counts := make(map[string]int, len(matches))
	for i, path := range matches {
		ids[i] = sourceFullID(source{Path: path, ID: path})
		counts[ids[i]]++
	}
	const maxCandidates = 10
	shown := make([]string, 0, min(len(matches), maxCandidates))
	for i, path := range matches[:min(len(matches), maxCandidates)] {
		candidate := ids[i]
		if counts[candidate] > 1 {
			candidate = tilde(path, home)
		}
		shown = append(shown, candidate)
	}
	more := len(matches) - len(shown)
	if more > 0 {
		shown = append(shown, fmt.Sprintf("… ещё %d", more))
	}
	return "", fmt.Errorf("%q is ambiguous: matches %d transcripts (%s); give more of the id", arg, len(matches), strings.Join(shown, ", "))
}

// sessionArg normalizes a session argument for both backends: it strips "id:", expands "~/", and turns a filesystem
// path into the name the transcript has under home (see transcriptName). parseRef removes a line suffix before this call.
func sessionArg(arg, home string) string {
	arg = strings.TrimPrefix(arg, idPrefix)
	if strings.HasPrefix(arg, "~/") {
		arg = filepath.Join(home, arg[2:])
	}
	if strings.HasSuffix(arg, ".jsonl") || strings.Contains(arg, string(filepath.Separator)) {
		arg = transcriptName(arg, home)
	}
	return arg
}

// transcriptName is the name a transcript path has in the search area: the path as it is spelled under home, the way
// the loader stores it, whatever symlinks the given path, home or the transcript roots go through, and a project link
// replaced by the project it names. A path that does not resolve is returned absolute and uncleaned: a lexical ".."
// after a symlink may name another, existing file.
func transcriptName(path, home string) string {
	if !filepath.IsAbs(path) {
		if cwd, err := os.Getwd(); err == nil {
			path = cwd + string(filepath.Separator) + path
		}
	}
	roots := transcriptRoots(home)
	if !hasDotDot(path) { // without ".." the lexical form names the same file as the kernel walk
		if clean := filepath.Clean(path); roots.lexical(clean) {
			return roots.canonicalProject(clean)
		}
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return roots.canonicalProject(roots.homeForm(resolved))
	}
	return path
}

func hasDotDot(path string) bool {
	return slices.Contains(strings.Split(filepath.ToSlash(path), "/"), "..")
}

// searchRoots are the transcript roots as spelled under home and as the kernel resolves them. workers is the folder
// above the per-worker codex sessions: their pattern holds a "*" no symlink resolves.
type searchRoots struct {
	projects, workers         string
	realProjects, realWorkers string
	links                     map[string]string // project link name → project, "" for a name that is not a link
}

func transcriptRoots(home string) searchRoots {
	workers, _, _ := strings.Cut(transcript.WorkerSessions(home), string(filepath.Separator)+"*")
	r := searchRoots{projects: absoluteRoot(session.ProjectsDir(home)), workers: absoluteRoot(workers), links: map[string]string{}}
	r.realProjects, r.realWorkers = realRoot(r.projects), realRoot(r.workers)
	return r
}

func absoluteRoot(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		return absolute
	}
	return path
}

func realRoot(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

func (r searchRoots) lexical(path string) bool {
	return underDir(path, r.projects) || underDir(path, r.workers)
}

// homeForm spells a resolved path under the transcript root it lies in.
func (r searchRoots) homeForm(path string) string {
	for _, root := range [][2]string{{r.realProjects, r.projects}, {r.realWorkers, r.workers}} {
		if rest, ok := strings.CutPrefix(path, root[0]+string(filepath.Separator)); ok {
			return filepath.Join(root[1], rest)
		}
	}
	return path
}

// canonicalProject replaces a project link in a path under the projects root by the project it names.
func (r searchRoots) canonicalProject(path string) string {
	relative, ok := strings.CutPrefix(path, r.projects+string(filepath.Separator))
	if !ok {
		return path
	}
	project, rest, nested := strings.Cut(relative, string(filepath.Separator))
	target, ok := r.projectLink(project)
	if !ok {
		return path
	}
	if !nested {
		return target
	}
	return filepath.Join(target, rest)
}

// projectLink is the project a link in the projects root names, spelled under the root; a link out of the root, a
// dangling or a looping one names none.
func (r searchRoots) projectLink(name string) (string, bool) {
	target, seen := r.links[name]
	if !seen {
		target = r.linkTarget(name)
		r.links[name] = target
	}
	return target, target != ""
}

func (r searchRoots) linkTarget(name string) string {
	path := filepath.Join(r.projects, name)
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			if target := r.homeForm(resolved); filepath.Dir(target) == r.projects && target != path {
				return target
			}
		}
	}
	return "" // not a link, or a link out of the root, dangling or looping
}

func underDir(path, dir string) bool {
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

const idPrefix = "id:"

var refArg = regexp.MustCompile(`^(.+):([1-9][0-9]{0,8})$`)

// findUUID is the leftmost lower-case UUID in s, as the regexp [0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}
// finds it, or "". The link ids take it from every name of the search area, tens of thousands per call, where the
// regexp costs tens of milliseconds.
func findUUID(s string) string {
	const n = 36
	for i := 0; i+n <= len(s); i++ {
		if uuidAt(s[i : i+n]) {
			return s[i : i+n]
		}
	}
	return ""
}

// uuidAt tells whether s, 36 bytes, is a lower-case UUID.
func uuidAt(s string) bool {
	for j := range len(s) {
		c := s[j]
		if j == 8 || j == 13 || j == 18 || j == 23 {
			if c != '-' {
				return false
			}
		} else if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func sourceFullID(src source) string {
	base := filepath.Base(src.Path)
	if id := findUUID(base); id != "" {
		return id
	}
	if id, ok := strings.CutPrefix(base, "agent-"); ok {
		return strings.TrimSuffix(id, ".jsonl")
	}
	if src.ID != "" && src.ID != src.Path {
		return src.ID
	}
	if id, ok := strings.CutSuffix(base, ".jsonl"); ok {
		return id
	}
	return src.ID
}

// transcriptPaths lists only transcript names in the same roots searched by find.
func transcriptPaths(home string) ([]string, error) {
	var paths []string
	roots := transcriptRoots(home)
	for i, pattern := range transcriptPatterns(home) {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("glob transcript names: %w", err)
		}
		if i == 2 {
			paths = append(paths, matches...)
			continue
		}
		for _, match := range matches {
			paths = append(paths, roots.canonicalProject(match))
		}
	}
	sort.Strings(paths)
	return slices.Compact(paths), nil // a project link and its target name one transcript
}

// transcriptPatterns — the names find and the link ids know: Claude Code sessions, their subagents, worker rollouts.
func transcriptPatterns(home string) []string {
	roots := transcriptRoots(home)
	return []string{
		filepath.Join(roots.projects, "*", "*.jsonl"),
		filepath.Join(roots.projects, "*", "*", "subagents", "agent-*.jsonl"),
		filepath.Join(absoluteRoot(transcript.WorkerSessions(home)), "*", "*", "*", "rollout-*.jsonl"),
	}
}

// liveSessionNames returns the names readable now; the error names registry records skipped because they are being written.
func liveSessionNames(home string) (map[string]bool, error) {
	alive, err := session.ForHome(home).Alive()
	names := make(map[string]bool, len(alive))
	for name := range alive {
		names[name] = true
	}
	return names, err
}

// sourceLinkIDs assigns each selected path the shortest prefix unique in find's full search area.
func sourceLinkIDs(sources []source, allPaths []string, home string, liveNames map[string]bool) map[string]string {
	fullIDs := make(map[string]string, len(allPaths)+len(sources))
	searchable := make(map[string]bool, len(allPaths))
	for _, path := range allPaths {
		searchable[path] = true
		fullIDs[path] = sourceFullID(source{Path: path, ID: path})
	}
	for _, src := range sources {
		fullIDs[src.Path] = sourceFullID(src)
	}
	counts := make(map[string]int, len(fullIDs))
	unique := make([]string, 0, len(fullIDs))
	for _, id := range fullIDs {
		if counts[id] == 0 {
			unique = append(unique, id)
		}
		counts[id]++
	}
	sort.Strings(unique)
	ids := make(map[string]string, len(sources))
	for _, src := range sources {
		path, id := src.Path, fullIDs[src.Path]
		base := filepath.Base(path)
		if !searchable[path] {
			ids[path] = tilde(path, home)
			continue
		}
		if src.ID == path && findUUID(base) == "" && !strings.HasPrefix(base, "agent-") {
			ids[path] = tilde(path, home)
			continue
		}
		if counts[id] > 1 {
			ids[path] = tilde(path, home)
			continue
		}
		length := min(8, len(id))
		at := sort.SearchStrings(unique, id)
		common := 0
		if at > 0 {
			common = max(common, commonPrefix(id, unique[at-1]))
		}
		if at+1 < len(unique) {
			common = max(common, commonPrefix(id, unique[at+1]))
		}
		if common >= len(id) {
			ids[path] = tilde(path, home)
			continue
		}
		length = max(length, common+1)
		for length < len(id) && liveNames[id[:length]] {
			length++
		}
		if liveNames[id[:length]] {
			ids[path] = tilde(path, home)
		} else {
			ids[path] = id[:length]
		}
	}
	return ids
}

func commonPrefix(a, b string) int {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return i
}

type eventRef struct {
	id   string // as given, the "id:" prefix included: find takes it off
	line int
}

// parseRef узнаёт аргумент вида «id:строка» или «id:<id>:строка» (форма из документации) и отделяет строку; не он —
// ok=false.
func parseRef(arg string) (string, int, bool, error) {
	m := refArg.FindStringSubmatch(arg)
	if m == nil {
		return "", 0, false, nil
	}
	id := m[1]
	if id == idPrefix {
		return "", 0, true, fmt.Errorf("ref %s: empty id", arg)
	}
	line, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, true, fmt.Errorf("ref %s: %w", arg, err)
	}
	return id, line, true, nil
}

// splitRefs accepts either session arguments or event references, never a mixture.
func splitRefs(args []string) ([]eventRef, error) {
	refs := make([]eventRef, 0, len(args))
	matched := 0
	for _, arg := range args {
		id, line, ok, err := parseRef(arg)
		if err != nil {
			return nil, err
		}
		if ok {
			matched++
			refs = append(refs, eventRef{id: id, line: line})
		}
	}
	if matched == 0 {
		return nil, nil
	}
	if matched != len(args) {
		return nil, errors.New("REF cannot be combined with SESSION")
	}
	return refs, nil
}
