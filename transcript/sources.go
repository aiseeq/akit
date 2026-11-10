package transcript

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Source describes a transcript file and, for a subagent, its parent.
type Source struct {
	Path, ID, ParentID, Description string
}

var sourceUUIDTail = regexp.MustCompile(`([0-9a-f]{8})-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.jsonl$`)

// NewSource describes a transcript path, extracting its session id when present.
func NewSource(path string) Source {
	if m := sourceUUIDTail.FindStringSubmatch(path); m != nil {
		return Source{Path: path, ID: m[1]}
	}
	return Source{Path: path, ID: path}
}

// WorkerSessions returns the glob root used for worker codex sessions.
func WorkerSessions(home string) string {
	return filepath.Join(home, ".local", "state", "worker", "*", "codex", "sessions")
}

// AllSources lists Claude and optionally worker codex transcripts modified since.
func AllSources(home string, since time.Time, project string, codex, includeSubs bool) ([]Source, error) {
	return AllSourcesWindow(home, since, time.Time{}, project, codex, includeSubs)
}

// AllSourcesWindow lists transcripts modified since, excluding sources that began after until.
func AllSourcesWindow(home string, since, until time.Time, project string, codex, includeSubs bool) ([]Source, error) {
	projects := filepath.Join(home, ".claude", "projects")
	paths, err := filepath.Glob(filepath.Join(projects, "*", "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("list transcripts: %w", err)
	}
	if includeSubs {
		subs, e := filepath.Glob(filepath.Join(projects, "*", "*", "subagents", "agent-*.jsonl"))
		if e != nil {
			return nil, fmt.Errorf("list subagent transcripts: %w", e)
		}
		paths = append(paths, subs...)
	}
	if codex {
		more, e := filepath.Glob(filepath.Join(WorkerSessions(home), "*", "*", "*", "rollout-*.jsonl"))
		if e != nil {
			return nil, fmt.Errorf("list codex transcripts: %w", e)
		}
		paths = append(paths, more...)
	}
	var out []Source
	for _, p := range paths {
		if project != "" && !strings.Contains(p, project) {
			continue
		}
		st, e := os.Stat(p)
		if errors.Is(e, fs.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, fmt.Errorf("transcript %s: %w", p, e)
		}
		if st.ModTime().Before(since) {
			continue
		}
		if !until.IsZero() {
			started, known, err := sourceStart(p)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("transcript %s: %w", p, err)
			}
			if known && started.After(until) {
				continue
			}
		}
		src, e := SourceAt(p)
		if e != nil {
			return nil, e
		}
		out = append(out, src)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// rolloutStamp is the start time codex writes into a rollout file name, in local time.
const rolloutStamp = "2006-01-02T15-04-05"

// sourceStart returns a transcript's start time when it can be read cheaply: from a
// rollout name, or the first timestamp among the first lines of the file. Unknown when
// neither tells, so the caller keeps the source.
func sourceStart(path string) (time.Time, bool, error) {
	base := filepath.Base(path)
	if stamp, ok := strings.CutPrefix(base, "rollout-"); ok {
		if len(stamp) >= len(rolloutStamp) {
			if at, err := time.ParseInLocation(rolloutStamp, stamp[:len(rolloutStamp)], time.Local); err == nil {
				return at, true, nil
			}
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("open transcript start: %w", err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 1<<20)
	for lines := 0; lines < 64 && s.Scan(); lines++ {
		var rec struct {
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(s.Bytes(), &rec) != nil || rec.Timestamp == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
		if err == nil {
			return at, true, nil
		}
	}
	if err := s.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return time.Time{}, false, fmt.Errorf("read transcript start: %w", err)
	}
	return time.Time{}, false, nil
}

// AllSourcesWalkOrder lists the same sources in filesystem traversal order.
func AllSourcesWalkOrder(home string, since time.Time, project string, codex, includeSubs bool) ([]Source, error) {
	sources, err := AllSources(home, since, project, codex, includeSubs)
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]Source, len(sources))
	for _, src := range sources {
		byPath[src.Path] = src
	}
	roots := []string{filepath.Join(home, ".claude", "projects")}
	if codex {
		roots = append(roots, filepath.Join(home, ".local", "state", "worker"))
	}
	out := make([]Source, 0, len(sources))
	for _, root := range roots {
		if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("stat transcript root %s: %w", root, err)
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || filepath.Ext(path) != ".jsonl" {
				return nil
			}
			if strings.Contains(root, filepath.Join(".local", "state", "worker")) &&
				(!strings.Contains(path, string(filepath.Separator)+"codex"+string(filepath.Separator)+"sessions"+string(filepath.Separator)) || !strings.HasPrefix(filepath.Base(path), "rollout-")) {
				return nil
			}
			if src, ok := byPath[path]; ok {
				out = append(out, src)
				delete(byPath, path)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk transcripts under %s: %w", root, err)
		}
	}
	return out, nil
}

// SourceAt identifies a regular or subagent transcript by path.
func SourceAt(path string) (Source, error) {
	if strings.Contains(path, string(filepath.Separator)+"subagents"+string(filepath.Separator)) {
		return SubSource(path)
	}
	return NewSource(path), nil
}

// SubSources lists subagent transcripts belonging to a parent transcript.
func SubSources(parent string) ([]Source, error) {
	dir := filepath.Join(strings.TrimSuffix(parent, filepath.Ext(parent)), "subagents")
	paths, err := filepath.Glob(filepath.Join(dir, "agent-*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("list subagents for %s: %w", parent, err)
	}
	out := make([]Source, 0, len(paths))
	for _, path := range paths {
		src, e := SubSource(path)
		if e != nil {
			return nil, e
		}
		out = append(out, src)
	}
	return out, nil
}

// SubSource describes a subagent transcript and its parent.
func SubSource(path string) (Source, error) {
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "agent-") {
		return Source{}, fmt.Errorf("invalid subagent transcript name %s", path)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(base, "agent-"), ".jsonl")
	parent := filepath.Base(filepath.Dir(filepath.Dir(path)))
	parentID := NewSource(parent + ".jsonl").ID
	if len(id) == 0 {
		return Source{}, fmt.Errorf("invalid subagent transcript name %s", path)
	}
	src := Source{Path: path, ID: id[:min(8, len(id))], ParentID: parentID}
	data, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + ".meta.json")
	if err == nil {
		var meta struct {
			Description string `json:"description"`
		}
		if json.Unmarshal(data, &meta) == nil {
			src.Description = meta.Description
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Source{}, fmt.Errorf("subagent metadata %s: %w", path, err)
	}
	return src, nil
}
