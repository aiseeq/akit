package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	if opts.all {
		if len(opts.args) > 0 {
			return nil, errors.New("--all and SESSION together; pick one")
		}
		since := opts.since
		if since.IsZero() {
			since = e.now.Add(-sinceDefault)
		}
		return transcript.AllSourcesWindow(e.home, since, opts.until, opts.project, opts.codex, !opts.noSubagents)
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
		path, err := find(a, e.home)
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
	if strings.HasPrefix(arg, "~/") {
		arg = filepath.Join(home, arg[2:])
	}
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
	paths, err := transcriptPaths(home)
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

var refArg = regexp.MustCompile(`^(.+):([1-9][0-9]{0,8})$`)
var sourceUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func sourceFullID(src source) string {
	base := filepath.Base(src.Path)
	if id := sourceUUID.FindString(base); id != "" {
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
	projects := session.ProjectsDir(home)
	patterns := []string{
		filepath.Join(projects, "*", "*.jsonl"),
		filepath.Join(projects, "*", "*", "subagents", "agent-*.jsonl"),
		filepath.Join(transcript.WorkerSessions(home), "*", "*", "*", "rollout-*.jsonl"),
	}
	var paths []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("glob transcript names: %w", err)
		}
		paths = append(paths, matches...)
	}
	sort.Strings(paths)
	return paths, nil
}

// sourceLinkIDs assigns each selected path the shortest prefix unique in find's full search area.
func sourceLinkIDs(sources []source, allPaths []string, home string) map[string]string {
	fullIDs := make(map[string]string, len(allPaths)+len(sources))
	for _, path := range allPaths {
		fullIDs[path] = sourceFullID(source{Path: path, ID: path})
	}
	for _, src := range sources {
		fullIDs[src.Path] = sourceFullID(src)
	}
	counts := make(map[string]int)
	for _, id := range fullIDs {
		for length := 1; length <= len(id); length++ {
			counts[id[:length]]++
		}
	}
	ids := make(map[string]string, len(sources))
	for _, src := range sources {
		path, id := src.Path, fullIDs[src.Path]
		base := filepath.Base(path)
		if src.ID == path && !sourceUUID.MatchString(base) && !strings.HasPrefix(base, "agent-") {
			ids[path] = tilde(path, home)
			continue
		}
		if counts[id] > 1 {
			ids[path] = tilde(path, home)
			continue
		}
		length := min(8, len(id))
		for length < len(id) && counts[id[:length]] > 1 {
			length++
		}
		ids[path] = id[:length]
	}
	return ids
}

type eventRef struct {
	id   string
	line int
}

// parseRef узнаёт аргумент вида «id:строка» или «id:<id>:строка» (форма из документации); не он — ok=false.
func parseRef(arg string) (string, int, bool, error) {
	m := refArg.FindStringSubmatch(arg)
	if m == nil {
		return "", 0, false, nil
	}
	id := strings.TrimPrefix(m[1], "id:")
	if id == "" {
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
