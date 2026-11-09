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

// frozenRefusal says why work of the project is not given to this session, or ""
// when it may be. A frozen project is worked on only by its running master; the
// identity is the session itself, since --by is free text.
func frozenRefusal(project string, errout io.Writer) (string, error) {
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
		matches, err := frozenLineNames(line, home, want)
		if err != nil {
			return "", fmt.Errorf("%s:%d: %w", path, number+1, err)
		}
		if !matches {
			continue
		}
		reason := frozenWhy(line)
		if reason == "" {
			fmt.Fprintf(errout, "tk: %s:%d: incomplete line, the project stays frozen\n", path, number+1)
			reason = "incomplete line in the frozen list"
		}
		master := filepath.Base(want) + "-master"
		if liveMaster(home, master, errout) {
			return "", nil
		}
		return fmt.Sprintf("project %s is frozen (%s): only its running %s takes work here", filepath.Base(want), reason, master), nil
	}
	return "", nil
}

func frozenLineNames(line, home, want string) (bool, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
		return false, nil
	}
	listed := fields[0]
	if rest, ok := strings.CutPrefix(listed, "~/"); ok {
		listed = filepath.Join(home, rest)
	}
	resolved, err := normalizedRoot(listed)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("resolve %s: %w", listed, err)
	}
	return resolved == want, nil
}

// frozenWhy is the fourth tab-separated field; "" when the line lacks it.
func frozenWhy(line string) string {
	parts := strings.Split(line, "\t")
	if len(parts) < 4 {
		return ""
	}
	return strings.TrimSpace(parts[3])
}

func liveMaster(home, master string, errout io.Writer) bool {
	if !strings.EqualFold(session(), master) {
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
