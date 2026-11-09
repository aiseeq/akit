// Package queuegate reads the queue gate file and decides whether a ticket
// clears its bar. The file is written by the queue daemon; every reader of the
// bar goes through this package, so that it cannot drift between them.
package queuegate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// maxAge is how long a written gate stays trusted.
const maxAge = 26 * time.Hour

// Gate is the gate file as read. Without Valid the file is not trusted and
// Problem says why.
type Gate struct {
	At         string              `json:"at"`
	Live       []string            `json:"live"`
	Open       []string            `json:"open"`
	OpenByRoot map[string][]string `json:"open_by_root"`

	Path    string `json:"-"`
	Problem string `json:"-"`
	Valid   bool   `json:"-"`
}

// Path is the gate file of the given home directory.
func Path(home string) string {
	return filepath.Join(home, ".local/state/queue/gate.json")
}

// Read loads the gate at path. A gate that cannot be trusted is a state of the
// result, not an error: readers go on with only the always open work.
func Read(path string, now time.Time) Gate {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Gate{Path: path, Problem: "gate.json is missing"}
	}
	if err != nil {
		return Gate{Path: path, Problem: "cannot read gate.json: " + err.Error()}
	}
	gate := Gate{Path: path}
	if err := json.Unmarshal(content, &gate); err != nil {
		return Gate{Path: path, Problem: "cannot parse gate.json: " + err.Error()}
	}
	at, err := time.Parse(time.RFC3339, gate.At)
	switch {
	case err != nil:
		gate.Problem = "gate.json has invalid at timestamp: " + err.Error()
	case now.Sub(at) > maxAge:
		gate.Problem = "gate.json is older than 26 hours"
	case at.After(now):
		gate.Problem = "gate.json timestamp is in the future"
	case gate.OpenByRoot == nil:
		gate.Problem = "gate.json has no open_by_root"
	default:
		gate.Valid = true
	}
	return gate
}

// Pairs is the open list of the longest root holding the project, else the
// common one.
func (g Gate) Pairs(project string) ([]string, error) {
	if len(g.OpenByRoot) == 0 {
		return g.Open, nil
	}
	resolvedProject, err := Resolve(project)
	if err != nil {
		return nil, fmt.Errorf("resolve project root %s: %w", project, err)
	}
	best, pairs := "", g.Open
	for candidate, rootPairs := range g.OpenByRoot {
		if !filepath.IsAbs(candidate) {
			continue
		}
		resolvedRoot, err := Resolve(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve gate root %s: %w", candidate, err)
		}
		rel, err := filepath.Rel(resolvedRoot, resolvedProject)
		if err != nil {
			return nil, fmt.Errorf("compare gate root %s with project %s: %w", resolvedRoot, resolvedProject, err)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || len(resolvedRoot) <= len(best) {
			continue
		}
		best, pairs = resolvedRoot, rootPairs
	}
	return pairs, nil
}

// Resolve makes a path absolute and free of symlinks, the form project and root
// paths are compared in.
func Resolve(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path %s: %w", path, err)
	}
	return filepath.EvalSymlinks(abs)
}

// Scope is the gate as it applies to one project. The zero Scope is an untrusted
// gate.
type Scope struct {
	Valid   bool
	Live    bool
	Open    []string
	Path    string
	Problem string
}

// For applies the gate to a project. The bar of a project whose gate is not
// trusted is not looked up.
func (g Gate) For(project string) (Scope, error) {
	scope := Scope{Valid: g.Valid, Live: slices.Contains(g.Live, filepath.Base(project)), Path: g.Path, Problem: g.Problem}
	if !g.Valid {
		return scope, nil
	}
	open, err := g.Pairs(project)
	if err != nil {
		return Scope{}, err
	}
	scope.Open = open
	return scope, nil
}

// Passes says whether a ticket of this priority and difficulty clears the bar.
// CRIT and HIGH always do. Under an untrusted gate nothing else does; a project
// out of the live list has no bar.
func (s Scope) Passes(prio, diff string) bool {
	if !s.Valid {
		return pairsAdmit(prio, diff, nil)
	}
	return !s.Live || pairsAdmit(prio, diff, s.Open)
}

func pairsAdmit(prio, diff string, open []string) bool {
	if prio == "MEDIUM" {
		prio = "MED"
	}
	if prio == "CRIT" || prio == "HIGH" {
		return true
	}
	if diff != "" {
		return pairOpen(prio, diff, open)
	}
	// A ticket without a difficulty may turn out to be any of them.
	for _, d := range []string{"DO", "HUNT", "ARCH"} {
		if !pairOpen(prio, d, open) {
			return false
		}
	}
	return true
}

func pairOpen(prio, diff string, open []string) bool {
	return slices.Contains(open, prio+"-"+diff) || prio == "MED" && slices.Contains(open, "MEDIUM-"+diff)
}
