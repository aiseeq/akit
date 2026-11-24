package transcript

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// EventRetention is the age after which the loader may remove transcript events.
const EventRetention = 90 * 24 * time.Hour

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

// SourcePatterns — glob-шаблоны файлов, которые перечисляет AllSources: транскрипты Claude Code, при includeSubs —
// суб-агентов, при codex — rollouts бойцов worker. Тому, кто следит за появлением файлов сам.
func SourcePatterns(home string, codex, includeSubs bool) []string {
	projects := filepath.Join(home, ".claude", "projects")
	out := []string{filepath.Join(projects, "*", "*.jsonl")}
	if includeSubs {
		out = append(out, filepath.Join(projects, "*", "*", "subagents", "agent-*.jsonl"))
	}
	if codex {
		out = append(out, filepath.Join(WorkerSessions(home), "*", "*", "*", "rollout-*.jsonl"))
	}
	return out
}

// AllSourcesWindow lists transcripts modified since, excluding sources that began after until.
func AllSourcesWindow(home string, since, until time.Time, project string, codex, includeSubs bool) ([]Source, error) {
	var paths []string
	for _, pattern := range SourcePatterns(home, codex, includeSubs) {
		more, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("list transcripts %s: %w", pattern, err)
		}
		paths = append(paths, more...)
	}
	return SelectSources(paths, since, until, project)
}

// SelectSources makes the choice of AllSourcesWindow among the given paths, for a caller that lists the
// transcript names from another place than the folders: a missing file is left out, the others are checked on
// disk the same way.
func SelectSources(paths []string, since, until time.Time, project string) ([]Source, error) {
	var out []Source
	for _, p := range paths {
		codexSource := strings.Contains(p, string(filepath.Separator)+"codex"+string(filepath.Separator)+"sessions"+string(filepath.Separator))
		pathMatches := projectPathMatches(p, project)
		if !pathMatches && !codexSource {
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
		if sourceStartsAfter(p, until) {
			continue
		}
		if !pathMatches {
			matches, err := codexProjectMatches(p, project)
			if err != nil {
				slog.Warn("skip codex source during project filter", "path", p, "error", err)
				continue
			}
			if !matches {
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

func sourceStartsAfter(path string, until time.Time) bool {
	if until.IsZero() {
		return false
	}
	started, known, err := sourceStart(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	// Other read errors leave the start unknown, so the source stays in the list.
	return err == nil && known && started.After(until)
}

func projectPathMatches(path, project string) bool {
	return project == "" || strings.Contains(path, project)
}

// codexProjectMatches checks the session's cwd in its first records, falling back to
// the worker job metadata when older or incomplete rollouts have no cwd.
func codexProjectMatches(path, project string) (bool, error) {
	cwd, err := codexCWD(path)
	if err != nil {
		return false, err
	}
	if cwd == "" {
		cwd, err = workerTree(path)
		if err != nil {
			return false, err
		}
	}
	return strings.Contains(cwd, project), nil
}

func codexCWD(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open transcript for cwd: %w", err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 1<<20)
	for lines := 0; lines < 64 && s.Scan(); lines++ {
		var rec struct {
			Type    string `json:"type"`
			Payload struct {
				CWD string `json:"cwd"`
			} `json:"payload"`
		}
		if json.Unmarshal(s.Bytes(), &rec) == nil && rec.Type == "session_meta" && rec.Payload.CWD != "" {
			return rec.Payload.CWD, nil
		}
	}
	if err := s.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return "", nil
		}
		return "", fmt.Errorf("read transcript cwd: %w", err)
	}
	return "", nil
}

// CodexCWD reads the cwd from the first records of a rollout, when present.
func CodexCWD(path string) (string, error) { return codexCWD(path) }

// CodexProjectCWD returns the rollout cwd, falling back to worker task metadata as source selection does.
func CodexProjectCWD(path string) (string, error) {
	cwd, err := codexCWD(path)
	if err != nil || cwd != "" {
		return cwd, err
	}
	return workerTree(path)
}

func workerTree(path string) (string, error) {
	// .../worker/<job>/codex/sessions/.../rollout-*.jsonl
	workerDir := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(path)))))
	if filepath.Base(workerDir) != "codex" {
		return "", nil
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(workerDir), "job.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read worker job metadata: %w", err)
	}
	var job struct {
		Dir string `json:"dir"`
	}
	if err := json.Unmarshal(data, &job); err != nil {
		return "", fmt.Errorf("decode worker job metadata: %w", err)
	}
	return job.Dir, nil
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
	if filepath.Base(filepath.Dir(path)) == "subagents" {
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
