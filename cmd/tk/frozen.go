package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	sessions "github.com/aiseeq/akit/session"
)

// frozenFields is the number of tab-separated fields of a complete line: path,
// who, date, why.
const frozenFields = 4

// frozenRefusal says why work of the project is not given to who, or "" when it
// may be. A frozen project is worked on only by its running master.
func frozenRefusal(project, who string, errout io.Writer) (string, error) {
	if project == "" {
		return "", nil
	}
	files, err := readFrozenFiles()
	if err != nil {
		return "", err
	}
	return files.refusal(project, who, errout)
}

// frozenFiles are the frozen list and the session names table, read once per
// command. A table that cannot be read matters only where a name is needed.
type frozenFiles struct {
	home, listPath, list string
	namesPath, names     string
	namesErr             error
}

func readFrozenFiles() (frozenFiles, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return frozenFiles{}, fmt.Errorf("determine frozen list home: %w", err)
	}
	files := frozenFiles{home: home, listPath: filepath.Join(home, ".config", "queue", "frozen"), namesPath: filepath.Join(home, ".config", "cl", "names.tsv")}
	list, err := os.ReadFile(files.listPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return frozenFiles{}, fmt.Errorf("read frozen list: %w", err)
	}
	files.list = string(list)
	names, err := os.ReadFile(files.namesPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		files.namesErr = err
	}
	files.names = string(names)
	return files, nil
}

func (f frozenFiles) refusal(project, who string, errout io.Writer) (string, error) {
	if f.list == "" {
		return "", nil
	}
	home, path := f.home, f.listPath
	want, err := normalizedRoot(project)
	if err != nil {
		return "", fmt.Errorf("resolve project %s: %w", project, err)
	}
	for number, raw := range strings.Split(f.list, "\n") {
		line := strings.TrimSpace(raw)
		place := fmt.Sprintf("%s:%d", path, number+1)
		matches, err := frozenLineNames(line, home, want)
		if err != nil {
			fmt.Fprintf(errout, "tk: %s: line ignored: %v\n", place, err)
			continue
		}
		if !matches {
			continue
		}
		reason := frozenWhy(line)
		if reason == "" {
			fmt.Fprintf(errout, "tk: %s: incomplete line, the project stays frozen\n", place)
			reason = "incomplete line in the frozen list"
		}
		master, err := f.masterName(want, errout)
		if err != nil {
			return "", err
		}
		if liveMaster(home, who, master, errout) {
			return "", nil
		}
		return fmt.Sprintf("project %s is frozen (%s): only its running %s takes work here", filepath.Base(want), reason, master), nil
	}
	return "", nil
}

// frozenLineNames: the line lists the project want. A listed folder that does
// not exist names nothing; an error is about this line only, so the caller goes
// on to the others.
func frozenLineNames(line, home, want string) (bool, error) {
	if strings.HasPrefix(line, "#") {
		return false, nil
	}
	listed := frozenPath(line)
	if listed == "" {
		return false, nil
	}
	return listedPathIs(listed, home, want)
}

func resolveListedPath(listed, home string) (string, error) {
	if listed == "~" {
		listed = home
	}
	if rest, ok := strings.CutPrefix(listed, "~/"); ok {
		listed = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(listed) {
		return "", errors.New("path must be absolute or start with ~/")
	}
	return normalizedRoot(listed)
}

// listedPathIs: the path of a list file is the project want. A listed folder that
// does not exist names nothing.
func listedPathIs(listed, home, want string) (bool, error) {
	resolved, err := resolveListedPath(listed, home)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return resolved == want, nil
}

// frozenPath is the first tab-separated field of a complete line. Any other line
// is read by its first word: malformed, it still names a project and keeps it
// frozen.
func frozenPath(line string) string {
	if parts := strings.Split(line, "\t"); len(parts) >= frozenFields {
		return strings.TrimSpace(parts[0])
	}
	if fields := strings.Fields(line); len(fields) != 0 {
		return fields[0]
	}
	return ""
}

// frozenWhy is the fourth tab-separated field; "" when the line lacks it.
func frozenWhy(line string) string {
	parts := strings.Split(line, "\t")
	if len(parts) < frozenFields {
		return ""
	}
	return strings.TrimSpace(parts[frozenFields-1])
}

type sessionNameRow struct {
	path, name string
	line       int
}

// sessionNameRows parses the shared tab-separated names table for all users of
// its project and session mappings.
func (f frozenFiles) sessionNameRows() []sessionNameRow {
	var rows []sessionNameRow
	for number, raw := range strings.Split(f.names, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		path, name, separated := strings.Cut(line, "\t")
		if !separated { // malformed: it still names the project, and gives no name
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			path = fields[0]
		}
		rows = append(rows, sessionNameRow{path: strings.TrimSpace(path), name: strings.TrimSpace(name), line: number + 1})
	}
	return rows
}

var errAmbiguousExecutorProject = errors.New("ambiguous executor project")

// executorProject resolves a session name through names.tsv, then through a
// conventional <name>-master project folder beside or inside the current root.
func (f frozenFiles) executorProject(executor, currentRoot string, errout io.Writer) (string, bool, error) {
	var mapped string
	lastLine := 0
	if f.namesErr != nil {
		// The hint is advice after the ticket is closed: an unreadable table leaves
		// the folder convention, and says so.
		fmt.Fprintf(errout, "tk: next ticket: session names unreadable, project by folder only: %v\n", f.namesErr)
	}
	for _, row := range f.sessionNameRows() {
		if !strings.EqualFold(row.name, executor) {
			continue
		}
		resolved, err := resolveListedPath(row.path, f.home)
		if errors.Is(err, os.ErrNotExist) { // a listed folder that does not exist names nothing
			continue
		}
		if err != nil {
			fmt.Fprintf(errout, "tk: %s:%d: line ignored: %v\n", f.namesPath, row.line, err)
			continue
		}
		mapped, lastLine = resolved, row.line
	}
	if lastLine != 0 {
		return mapped, true, nil
	}

	suffix := "-master"
	if !strings.HasSuffix(strings.ToLower(executor), suffix) {
		return "", false, nil
	}
	// Session names are matched without case: the folder is the lower-case name.
	name := strings.ToLower(executor[:len(executor)-len(suffix)])
	if name == "" {
		return "", false, nil
	}
	candidates := []string{filepath.Join(currentRoot, name), filepath.Join(filepath.Dir(currentRoot), name)}
	var found []string
	for _, candidate := range candidates {
		if _, err := os.Stat(filepath.Join(candidate, "docs", "tasks")); err == nil {
			resolved, err := normalizedRoot(candidate)
			if err != nil {
				return "", false, fmt.Errorf("resolve project %s: %w", candidate, err)
			}
			found = append(found, resolved)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false, fmt.Errorf("inspect project %s: %w", candidate, err)
		}
	}
	if len(found) > 1 {
		fmt.Fprintf(errout, "tk: executor %s has ambiguous project folders: %s and %s\n", executor, found[0], found[1])
		return "", false, errAmbiguousExecutorProject
	}
	if len(found) == 1 {
		return found[0], true, nil
	}
	return "", false, nil
}

// masterName is the session name the table of session names gives the project,
// else its folder name with -master. A table that exists but cannot be read is
// an error: another name would admit the wrong session.
func (f frozenFiles) masterName(want string, errout io.Writer) (string, error) {
	fallback := filepath.Base(want) + "-master"
	if f.namesErr != nil {
		return "", fmt.Errorf("read session names: %w", f.namesErr)
	}
	home, path := f.home, f.namesPath
	master, lastLine := "", 0
	for _, row := range f.sessionNameRows() {
		matches, err := listedPathIs(row.path, home, want)
		if err != nil {
			fmt.Fprintf(errout, "tk: %s:%d: line ignored: %v\n", path, row.line, err)
			continue
		}
		if !matches {
			continue
		}
		master, lastLine = row.name, row.line // the last line of a folder wins, as for session starts
	}
	if lastLine == 0 {
		return fallback, nil
	}
	if master == "" {
		return "", fmt.Errorf("%s:%d: no session name for the project", path, lastLine)
	}
	return master, nil
}

func liveMaster(home, who, master string, errout io.Writer) bool {
	if !strings.EqualFold(who, master) {
		return false
	}
	alive, err := sessions.ForHome(home).Alive()
	for name := range alive {
		if strings.EqualFold(name, master) {
			return true
		}
	}
	if err != nil {
		fmt.Fprintf(errout, "tk: session registry: %v\n", err)
	}
	return false
}
