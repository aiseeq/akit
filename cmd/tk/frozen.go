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
	for number, line := range strings.Split(string(content), "\n") {
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
		master := filepath.Base(want) + "-master"
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
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return false, nil
	}
	listed := frozenPath(line)
	if listed == "" {
		return false, nil
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

// frozenPath is the first tab-separated field, or the first word of a line
// written without tabs.
func frozenPath(line string) string {
	if first, _, tabbed := strings.Cut(line, "\t"); tabbed {
		return strings.TrimSpace(first)
	}
	if fields := strings.Fields(line); len(fields) != 0 {
		return fields[0]
	}
	return ""
}

// frozenWhy is the fourth tab-separated field; "" when the line lacks it.
func frozenWhy(line string) string {
	parts := strings.Split(line, "\t")
	if len(parts) < 4 {
		return ""
	}
	return strings.TrimSpace(parts[3])
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
