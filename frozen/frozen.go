package frozen

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const fields = 4

// List is a frozen-project list and the home directory used to expand ~/ paths.
type List struct {
	content, home string
}

// NewList creates a list from content already read by the caller.
func NewList(content, home string) List {
	return List{content: content, home: home}
}

// Frozen reports why project is frozen. A malformed matching row still freezes
// it; onLineError receives malformed paths that do not identify a project.
func (l List) Frozen(project string, onLineError func(number int, err error)) (reason string, frozen bool, err error) {
	if l.content == "" {
		return "", false, nil
	}
	want, err := filepath.EvalSymlinks(project)
	if err != nil {
		return "", false, fmt.Errorf("resolve project %s: %w", project, err)
	}
	for number, raw := range strings.Split(l.content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		listed := LinePath(line)
		if listed == "" {
			continue
		}
		matches, lineErr := ListedPathIs(listed, l.home, want)
		if lineErr != nil {
			if onLineError != nil {
				onLineError(number+1, lineErr)
			}
			continue
		}
		if !matches {
			continue
		}
		reason = LineReason(line)
		if reason == "" {
			return "incomplete line in the frozen list", true, nil
		}
		return reason, true, nil
	}
	return "", false, nil
}

// Frozen reads ~/.config/queue/frozen and checks project against it.
func Frozen(project string) (reason string, frozen bool, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("determine frozen list home: %w", err)
	}
	path := filepath.Join(home, ".config", "queue", "frozen")
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read frozen list: %w", err)
	}
	return NewList(string(content), home).Frozen(project, nil)
}

// LineNames reports whether a frozen-list line names want.
func LineNames(line, home, want string) (bool, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return false, nil
	}
	listed := LinePath(line)
	if listed == "" {
		return false, nil
	}
	return ListedPathIs(listed, home, want)
}

// LinePath returns the project field, or the first word of an incomplete row.
func LinePath(line string) string {
	if parts := strings.Split(line, "\t"); len(parts) >= fields {
		return strings.TrimSpace(parts[0])
	}
	if words := strings.Fields(line); len(words) != 0 {
		return words[0]
	}
	return ""
}

// LineReason returns the reason field of a complete row.
func LineReason(line string) string {
	parts := strings.Split(line, "\t")
	if len(parts) < fields {
		return ""
	}
	return strings.TrimSpace(parts[fields-1])
}

// ResolveListedPath expands ~/ and resolves symlinks in a listed path.
func ResolveListedPath(listed, home string) (string, error) {
	if listed == "~" {
		listed = home
	}
	if rest, ok := strings.CutPrefix(listed, "~/"); ok {
		listed = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(listed) {
		return "", errors.New("path must be absolute or start with ~/")
	}
	abs, err := filepath.Abs(listed)
	if err != nil {
		return "", fmt.Errorf("resolve listed path %s: %w", listed, err)
	}
	return filepath.EvalSymlinks(abs)
}

// ListedPathIs compares a listed path with a resolved project. Missing paths
// name no project; other resolution errors are returned to the caller.
func ListedPathIs(listed, home, want string) (bool, error) {
	resolved, err := ResolveListedPath(listed, home)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return resolved == want, nil
}
