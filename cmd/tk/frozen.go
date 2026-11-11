package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/aiseeq/akit/frozen"
	sessions "github.com/aiseeq/akit/session"
)

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
	files := frozenFiles{home: home, listPath: frozen.Path(home), namesPath: filepath.Join(home, ".config", "cl", "names.tsv")}
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
	list := frozen.NewList(f.list, home)
	result, err := list.Frozen(project, func(number int, line string, lineErr error) {
		fmt.Fprintf(errout, "tk: %s:%d: line ignored: %v\n", path, number, lineErr)
	})
	if err != nil {
		return "", err
	}
	if result.Incomplete {
		fmt.Fprintf(errout, "tk: %s:%d: incomplete line, the project stays frozen\n", path, result.Line)
	}
	if result.Frozen {
		master, err := f.masterName(want, errout)
		if err != nil {
			return "", err
		}
		if liveMaster(home, who, master, errout) {
			return "", nil
		}
		return fmt.Sprintf("project %s is frozen (%s): only its running %s takes work here", filepath.Base(want), result.Reason, master), nil
	}
	return "", nil
}

// frozenLineNames: the line lists the project want. A listed folder that does
// not exist names nothing; an error is about this line only, so the caller goes
// on to the others.
var frozenLineNames = frozen.LineNames

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
		resolved, err := frozen.ResolveListedPath(row.path, f.home)
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
		// A file named like the project (a built binary in the root of its
		// repository) is no project folder: ENOTDIR means absent too.
		info, err := os.Stat(filepath.Join(candidate, "docs", "tasks"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
				continue
			}
			return "", false, fmt.Errorf("inspect project %s: %w", candidate, err)
		}
		if !info.IsDir() {
			continue
		}
		resolved, err := normalizedRoot(candidate)
		if err != nil {
			return "", false, fmt.Errorf("resolve project %s: %w", candidate, err)
		}
		found = append(found, resolved)
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
		matches, err := frozen.ListedPathIs(row.path, home, want)
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
