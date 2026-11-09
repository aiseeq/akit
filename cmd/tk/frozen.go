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
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine frozen list home: %w", err)
	}
	path := filepath.Join(home, ".config", "queue", "frozen")
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read frozen list: %w", err)
	}
	want, err := normalizedRoot(project)
	if err != nil {
		return "", fmt.Errorf("resolve project %s: %w", project, err)
	}
	for number, raw := range strings.Split(string(content), "\n") {
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
		master, err := masterName(home, want, errout)
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

// listedPathIs: the path of a list file is the project want. A listed folder that
// does not exist names nothing.
func listedPathIs(listed, home, want string) (bool, error) {
	if listed == "~" {
		listed = home
	}
	if rest, ok := strings.CutPrefix(listed, "~/"); ok {
		listed = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(listed) {
		return false, errors.New("path must be absolute or start with ~/")
	}
	resolved, err := normalizedRoot(listed)
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

// masterName is the session name the table of session names gives the project,
// else its folder name with -master. A table that exists but cannot be read is
// an error: another name would admit the wrong session.
func masterName(home, want string, errout io.Writer) (string, error) {
	fallback := filepath.Base(want) + "-master"
	path := filepath.Join(home, ".config", "cl", "names.tsv")
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fallback, nil
	}
	if err != nil {
		return "", fmt.Errorf("read session names: %w", err)
	}
	master, lastLine := "", 0
	for number, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		listed, name, separated := strings.Cut(line, "\t")
		if !separated { // malformed: it still names the project, and gives no name
			listed = strings.Fields(line)[0]
		}
		matches, err := listedPathIs(strings.TrimSpace(listed), home, want)
		if err != nil {
			fmt.Fprintf(errout, "tk: %s:%d: line ignored: %v\n", path, number+1, err)
			continue
		}
		if !matches {
			continue
		}
		master, lastLine = strings.TrimSpace(name), number+1 // the last line of a folder wins, as for session starts
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
