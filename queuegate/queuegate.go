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
	"strconv"
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
	// CritOnly maps roots where new work is CRIT only right now (HIGH closed too)
	// to the reason, e.g. a day off. Keys are roots of OpenByRoot; a file
	// without the field closes nothing.
	CritOnly map[string]string `json:"crit_only"`

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
		gate.Problem = critOnlyProblem(gate)
		gate.Valid = gate.Problem == ""
	}
	return gate
}

// critOnlyProblem names crit_only roots missing from open_by_root: the root
// choice could never select them, and guessing which projects they cover is
// not safe.
func critOnlyProblem(g Gate) string {
	roots := make([]string, 0, len(g.CritOnly))
	for root := range g.CritOnly {
		if _, ok := g.OpenByRoot[root]; !ok || root == "" {
			roots = append(roots, strconv.Quote(root))
		}
	}
	if len(roots) == 0 {
		return ""
	}
	slices.Sort(roots)
	return "gate.json crit_only has roots missing from open_by_root (or empty): " + strings.Join(roots, ", ")
}

// Pairs is the open list of the longest root holding the project, else the
// common one.
func (g Gate) Pairs(project string) ([]string, error) {
	_, pairs, err := g.root(project)
	return pairs, err
}

// root is the longest root of open_by_root holding the project, as its key in the
// file ("" for none), and its open list, else the common one.
func (g Gate) root(project string) (string, []string, error) {
	if len(g.OpenByRoot) == 0 {
		return "", g.Open, nil
	}
	resolvedProject, err := Resolve(project)
	if err != nil {
		return "", nil, fmt.Errorf("resolve project root %s: %w", project, err)
	}
	key, best, pairs := "", "", g.Open
	for candidate, rootPairs := range g.OpenByRoot {
		if !filepath.IsAbs(candidate) {
			continue
		}
		resolvedRoot, err := Resolve(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("resolve gate root %s: %w", candidate, err)
		}
		rel, err := filepath.Rel(resolvedRoot, resolvedProject)
		if err != nil {
			return "", nil, fmt.Errorf("compare gate root %s with project %s: %w", resolvedRoot, resolvedProject, err)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || len(resolvedRoot) <= len(best) {
			continue
		}
		key, best, pairs = candidate, resolvedRoot, rootPairs
	}
	return key, pairs, nil
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
	// CritRoot and CritOnly: the project's root is in crit_only — new work only CRIT, for this reason.
	CritRoot, CritOnly string
}

// For applies the gate to a project. The bar of a project whose gate is not
// trusted is not looked up.
func (g Gate) For(project string) (Scope, error) {
	scope := Scope{Valid: g.Valid, Live: slices.Contains(g.Live, filepath.Base(project)), Path: g.Path, Problem: g.Problem}
	if !g.Valid {
		return scope, nil
	}
	root, open, err := g.root(project)
	if err != nil {
		return Scope{}, err
	}
	scope.Open = open
	if reason, ok := g.CritOnly[root]; ok {
		scope.CritRoot, scope.CritOnly = root, reason
	}
	return scope, nil
}

// Passes says whether a ticket of this priority and difficulty clears the bar.
// CRIT always does; under crit_only of the project's root nothing else does,
// live list or not. Otherwise HIGH does too; under an untrusted gate nothing
// else does; a project out of the live list has no bar.
func (s Scope) Passes(prio, diff string) bool {
	if normalizePrio(prio) == "CRIT" {
		return true
	}
	if !s.Valid {
		return pairsAdmit(prio, diff, nil)
	}
	if s.CritRoot != "" {
		return false
	}
	return !s.Live || pairsAdmit(prio, diff, s.Open)
}

// Admits is Passes for a reader that decides ticket by ticket: the project's bar
// is looked up only when the answer depends on it, so a bad root costs only the
// tickets it decides.
func (g Gate) Admits(project, prio, diff string) (bool, error) {
	if normalizePrio(prio) == "CRIT" {
		return true, nil
	}
	if !g.Valid {
		return pairsAdmit(prio, diff, nil), nil
	}
	live := slices.Contains(g.Live, filepath.Base(project))
	if len(g.CritOnly) == 0 && (!live || pairsAdmit(prio, diff, nil)) {
		return true, nil
	}
	scope, err := g.For(project)
	if err != nil {
		return false, err
	}
	return scope.Passes(prio, diff), nil
}

func normalizePrio(prio string) string {
	if prio == "MEDIUM" {
		return "MED"
	}
	return prio
}

func pairsAdmit(prio, diff string, open []string) bool {
	prio = normalizePrio(prio)
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
