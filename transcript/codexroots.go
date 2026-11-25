package transcript

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CodexRootsFile is the file that names the Codex session folders outside worker tasks: one folder per line, a
// glob allowed in any segment, "~/" for the home, '#' starting a comment. It sits under the home of the
// transcripts, so a loader with its own --home and tl in a shell read the same list.
func CodexRootsFile(home string) string {
	return filepath.Join(home, ".config", "akit", "codex-roots")
}

// DefaultCodexRoots are the folders used when CodexRootsFile does not exist: where the Codex CLI keeps its sessions.
func DefaultCodexRoots(home string) []string {
	return []string{filepath.Join(home, ".codex", "sessions")}
}

// CodexRoots lists the Codex session folders outside worker tasks: those of CodexRootsFile, or DefaultCodexRoots
// when there is no such file. A line that is not an absolute path or a valid glob is skipped and named in the error;
// the valid lines are returned with it.
func CodexRoots(home string) ([]string, error) {
	path := CodexRootsFile(home)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return DefaultCodexRoots(home), nil
	}
	if err != nil {
		return nil, fmt.Errorf("codex roots: %w", err)
	}
	var roots []string
	var bad []string
	s := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; s.Scan(); n++ {
		line, _, _ := strings.Cut(s.Text(), "#")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "~/"); ok {
			line = filepath.Join(home, rest)
		}
		if !filepath.IsAbs(line) {
			bad = append(bad, fmt.Sprintf("line %d %q: not an absolute path", n, line))
			continue
		}
		if _, err := filepath.Match(line, ""); err != nil {
			bad = append(bad, fmt.Sprintf("line %d %q: %v", n, line, err))
			continue
		}
		roots = append(roots, filepath.Clean(line))
	}
	if err := s.Err(); err != nil {
		return roots, fmt.Errorf("codex roots %s: %w", path, err)
	}
	if len(bad) > 0 {
		return roots, fmt.Errorf("codex roots %s: lines skipped: %s", path, strings.Join(bad, "; "))
	}
	return roots, nil
}

// CodexRolloutPattern is the glob of the rollouts under a Codex session folder: Codex files them by day.
func CodexRolloutPattern(root string) string {
	return filepath.Join(root, "*", "*", "*", "rollout-*.jsonl")
}

// CodexRootBase is the part of a session folder glob before its first pattern character: the folder that exists
// whatever the glob matches, where a watcher looks for new matches.
func CodexRootBase(root string) string {
	sep := string(filepath.Separator)
	segs := strings.Split(filepath.Clean(root), sep)
	for i, s := range segs {
		if strings.ContainsAny(s, `*?[\`) {
			if base := strings.Join(segs[:i], sep); base != "" {
				return base
			}
			return sep
		}
	}
	return filepath.Clean(root)
}

// CodexRootSources lists the rollouts under the given session folders (CodexRoots) modified since, chosen as
// AllSourcesWindow chooses: a project matches the path or the cwd of the session.
func CodexRootSources(roots []string, since, until time.Time, project string) ([]Source, error) {
	var paths []string
	for _, root := range roots {
		more, err := filepath.Glob(CodexRolloutPattern(root))
		if err != nil {
			return nil, fmt.Errorf("list codex rollouts %s: %w", root, err)
		}
		paths = append(paths, more...)
	}
	return SelectSources(paths, since, until, project)
}

// UnderCodexRoot reports whether path is a rollout under one of the session folders.
func UnderCodexRoot(roots []string, path string) bool {
	for _, root := range roots {
		if m, err := filepath.Match(CodexRolloutPattern(root), path); err == nil && m {
			return true
		}
	}
	return false
}
