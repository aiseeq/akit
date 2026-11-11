package frozen

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aiseeq/akit/queuegate"
)

// fields is the number of tab-separated fields: path, who, date, why.
const fields = 4

// Result describes one project's entry in a frozen-project list.
type Result struct {
	Reason     string
	Frozen     bool
	Incomplete bool
	Line       int
}

// List is a frozen-project list and the home directory used to expand ~/ paths.
type List struct {
	content, home, path string
}

// Path returns the frozen-project list path for home.
func Path(home string) string {
	return filepath.Join(home, ".config", "queue", "frozen")
}

// Read loads the frozen-project list for home. A missing list is an empty list.
func Read(home string) (List, error) {
	path := Path(home)
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return List{}, fmt.Errorf("read frozen list: %w", err)
	}
	return List{content: string(content), home: home, path: path}, nil
}

// NewList creates a list from content already read by the caller.
func NewList(content, home string) List {
	return List{content: content, home: home, path: Path(home)}
}

// Path returns the path of the list held by l.
func (l List) Path() string { return l.path }

// Frozen reports whether project is listed. onLineError receives invalid rows
// with their one-based line number and trimmed text; missing paths are ignored.
func (l List) Frozen(project string, onLineError func(number int, line string, err error)) (Result, error) {
	if l.content == "" {
		return Result{}, nil
	}
	want, err := queuegate.Resolve(project)
	if err != nil {
		return Result{}, fmt.Errorf("resolve project %s: %w", project, err)
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
				onLineError(number+1, line, lineErr)
			}
			continue
		}
		if !matches {
			continue
		}
		result := Result{Reason: LineReason(line), Frozen: true, Line: number + 1}
		if result.Reason == "" {
			result.Reason = "incomplete line in the frozen list"
			result.Incomplete = true
		}
		return result, nil
	}
	return Result{}, nil
}

// Frozen reads the default list and checks project. Invalid rows are returned as
// errors with the list path and line number instead of being silently discarded.
func Frozen(project string) (reason string, frozen bool, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("determine frozen list home: %w", err)
	}
	list, err := Read(home)
	if err != nil {
		return "", false, err
	}
	var rowErr error
	result, err := list.Frozen(project, func(number int, line string, lineErr error) {
		if rowErr == nil {
			rowErr = fmt.Errorf("%s:%d: line %q: %w", list.Path(), number, line, lineErr)
		}
	})
	if err != nil {
		return "", false, err
	}
	if rowErr != nil {
		return "", false, rowErr
	}
	if result.Incomplete {
		return result.Reason, true, nil
	}
	return result.Reason, result.Frozen, nil
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
	return queuegate.Resolve(listed)
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
