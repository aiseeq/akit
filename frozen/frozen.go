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

// LineError describes a malformed list row. Err is a path resolution problem;
// a listed path that does not exist is ignored and does not produce a LineError.
type LineError struct {
	Number int
	Text   string
	Err    error
}

// Result describes a project's frozen status and any malformed rows in the list.
type Result struct {
	Reason         string
	Frozen         bool
	Line           int
	IncompleteLine int
	Problems       []LineError
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

// NewList creates a list from content and its source path, already read by the caller.
func NewList(content, home, path string) List {
	return List{content: content, home: home, path: path}
}

// Path returns the path of the list held by l.
func (l List) Path() string { return l.path }

// Frozen resolves project and checks every row. Invalid rows are returned in
// Problems, while a valid matching row still sets Frozen and Reason.
func (l List) Frozen(project string) (Result, error) {
	want, err := queuegate.Resolve(project)
	if err != nil {
		return Result{}, fmt.Errorf("resolve project %s: %w", project, err)
	}
	var result Result
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
			result.Problems = append(result.Problems, LineError{Number: number + 1, Text: line, Err: lineErr})
			continue
		}
		if !matches || result.Frozen {
			continue
		}
		result = frozenResult(line, number+1, result.Problems)
	}
	return result, nil
}

func frozenResult(line string, number int, problems []LineError) Result {
	result := Result{Reason: LineReason(line), Frozen: true, Line: number, Problems: problems}
	if result.Reason == "" {
		result.Reason = "incomplete line in the frozen list"
		result.IncompleteLine = number
	}
	return result
}

// Frozen reads the default list and checks project. Err is fatal (HOME, list
// reading, or project resolution); malformed rows are reported in Result.Problems
// and do not prevent a matching row from being returned as frozen.
func Frozen(project string) (Result, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Result{}, fmt.Errorf("determine frozen list home: %w", err)
	}
	list, err := Read(home)
	if err != nil {
		return Result{}, err
	}
	return list.Frozen(project)
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
