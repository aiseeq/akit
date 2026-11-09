package queuegate

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

func writeGate(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gate.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func gateAt(at time.Time) string {
	return fmt.Sprintf(`{"at":%q,"live":["p"],"open":["MED-DO"],"open_by_root":{}}`, at.Format(time.RFC3339))
}

func TestPathIsUnderHome(t *testing.T) {
	if got, want := Path("/h"), "/h/.local/state/queue/gate.json"; got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
}

func TestReadValid(t *testing.T) {
	path := writeGate(t, gateAt(now.Add(-time.Hour)))
	g := Read(path, now)
	if !g.Valid || g.Problem != "" || g.Path != path {
		t.Fatalf("Read = %+v", g)
	}
	if !slices.Equal(g.Open, []string{"MED-DO"}) || !slices.Equal(g.Live, []string{"p"}) || g.OpenByRoot == nil {
		t.Fatalf("fields = %+v", g)
	}
}

func TestReadUntrusted(t *testing.T) {
	for _, tc := range []struct{ name, body, problem string }{
		{"broken json", `{`, "cannot parse gate.json"},
		{"bad at", `{"at":"yesterday","open_by_root":{}}`, "invalid at timestamp"},
		{"older than a day and two hours", gateAt(now.Add(-26*time.Hour - time.Second)), "older than 26 hours"},
		{"from the future", gateAt(now.Add(time.Second)), "in the future"},
		{"no open_by_root", fmt.Sprintf(`{"at":%q}`, now.Format(time.RFC3339)), "no open_by_root"},
		{"null open_by_root", fmt.Sprintf(`{"at":%q,"open_by_root":null}`, now.Format(time.RFC3339)), "no open_by_root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := Read(writeGate(t, tc.body), now)
			if g.Valid || !strings.Contains(g.Problem, tc.problem) {
				t.Fatalf("Valid=%v Problem=%q, want %q", g.Valid, g.Problem, tc.problem)
			}
		})
	}
}

func TestReadAtTheEdgesOfTrust(t *testing.T) {
	for _, at := range []time.Time{now.Add(-26 * time.Hour), now} {
		if g := Read(writeGate(t, gateAt(at)), now); !g.Valid {
			t.Errorf("at %v: %q", at, g.Problem)
		}
	}
}

func TestReadMissingAndUnreadable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gate.json")
	if g := Read(missing, now); g.Valid || g.Problem != "gate.json is missing" || g.Path != missing {
		t.Fatalf("missing: %+v", g)
	}
	dir := t.TempDir()
	if g := Read(dir, now); g.Valid || !strings.Contains(g.Problem, "cannot read gate.json") {
		t.Fatalf("unreadable: %+v", g)
	}
}

func TestPairs(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(parts ...string) string {
		dir := filepath.Join(append([]string{base}, parts...)...)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	work, group, project := mk("work"), mk("work", "group"), mk("work", "group", "project")
	sibling := mk("workshop", "project")
	link := filepath.Join(base, "link")
	if err := os.Symlink(project, link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base) // the relative root would otherwise resolve and win as the longest
	g := Gate{Open: []string{"COMMON"}, OpenByRoot: map[string][]string{
		work:                        {"WORK"},
		group:                       {"GROUP"},
		"work/group/project":        {"RELATIVE"},
		filepath.Join(base, "gone"): {"GONE"},
	}}
	for _, tc := range []struct {
		name, project string
		want          []string
	}{
		{"longest root wins", project, []string{"GROUP"}},
		{"root itself", work, []string{"WORK"}},
		{"symlink resolves", link, []string{"GROUP"}},
		{"relative project path", "work/group/project", []string{"GROUP"}},
		{"a prefix of the name is not a parent", sibling, []string{"COMMON"}},
	} {
		got, err := g.Pairs(tc.project)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if got, err := (Gate{Open: []string{"COMMON"}}).Pairs(filepath.Join(base, "no-such")); err != nil || !slices.Equal(got, []string{"COMMON"}) {
		t.Errorf("no roots: %v, %v", got, err)
	}
	if got, err := (Gate{Open: []string{"COMMON"}, OpenByRoot: map[string][]string{}}).Pairs(filepath.Join(base, "no-such")); err != nil || !slices.Equal(got, []string{"COMMON"}) {
		t.Errorf("empty roots: %v, %v", got, err)
	}
	if _, err := g.Pairs(filepath.Join(base, "no-such")); err == nil {
		t.Error("a missing project under roots must be an error")
	}
}

func TestPairsComparesRootsResolved(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shallow, deep := filepath.Join(base, "a"), filepath.Join(base, "a", "b")
	project := filepath.Join(deep, "p")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	// Longer as written than the deep root, shorter once resolved.
	link := filepath.Join(base, "a-link-with-a-name-longer-than-the-deep-root")
	if err := os.Symlink(shallow, link); err != nil {
		t.Fatal(err)
	}
	g := Gate{OpenByRoot: map[string][]string{link: {"SHALLOW"}, deep: {"DEEP"}}}
	for range 64 { // map order decides which root is seen first
		got, err := g.Pairs(project)
		if err != nil || !slices.Equal(got, []string{"DEEP"}) {
			t.Fatalf("Pairs = %v, %v", got, err)
		}
	}
}

func TestAdmits(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, "p")
	file := filepath.Join(dir, "file")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	badRoot := map[string][]string{filepath.Join(file, "below"): {"MED-DO"}}
	open := map[string][]string{dir: {"MED-DO"}}
	missing := filepath.Join(dir, "no-such")
	for _, tc := range []struct {
		name          string
		gate          Gate
		project, prio string
		want, fails   bool
	}{
		{"HIGH does not need the roots", Gate{Valid: true, Live: []string{"p"}, OpenByRoot: badRoot}, project, "HIGH", true, false},
		{"CRIT with a missing project", Gate{Valid: true, Live: []string{"no-such"}, OpenByRoot: badRoot}, missing, "CRIT", true, false},
		{"untrusted closes the rest", Gate{Live: []string{"p"}, OpenByRoot: open}, project, "MED", false, false},
		{"not live does not need the roots", Gate{Valid: true, OpenByRoot: badRoot}, project, "MED", true, false},
		{"live and open", Gate{Valid: true, Live: []string{"p"}, OpenByRoot: open}, project, "MED", true, false},
		{"live and closed", Gate{Valid: true, Live: []string{"p"}, OpenByRoot: open}, project, "LOW", false, false},
		{"a bad root fails what it decides", Gate{Valid: true, Live: []string{"p"}, OpenByRoot: badRoot}, project, "MED", false, true},
	} {
		got, err := tc.gate.Admits(tc.project, tc.prio, "DO")
		if (err != nil) != tc.fails || got != tc.want {
			t.Errorf("%s: %v, %v", tc.name, got, err)
		}
	}
}

func TestForLiveAndOpen(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, "p")
	other := filepath.Join(dir, "q")
	for _, d := range []string{project, other} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	g := Gate{Valid: true, Live: []string{"p"}, Open: []string{"MED-DO"}, Path: "x"}
	s, err := g.For(project)
	if err != nil || !s.Valid || !s.Live || !slices.Equal(s.Open, []string{"MED-DO"}) || s.Path != "x" {
		t.Fatalf("live: %+v, %v", s, err)
	}
	if s, err := g.For(other); err != nil || s.Live {
		t.Fatalf("not live: %+v, %v", s, err)
	}
}

func TestForOfUntrustedGateLooksNothingUp(t *testing.T) {
	g := Gate{Problem: "gate.json is missing", Path: "x", OpenByRoot: map[string][]string{"/": {"A"}}}
	s, err := g.For("/no/such/project")
	if err != nil || s.Valid || s.Open != nil || s.Problem != g.Problem || s.Path != "x" {
		t.Fatalf("%+v, %v", s, err)
	}
}

func TestForReportsARootError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	g := Gate{Valid: true, OpenByRoot: map[string][]string{filepath.Join(file, "below"): {"A"}}}
	if _, err := g.For(dir); err == nil {
		t.Fatal("a root that cannot be resolved must be an error")
	}
}

func TestPasses(t *testing.T) {
	all := []string{"MED-DO", "MED-HUNT", "MED-ARCH"}
	for _, tc := range []struct {
		name       string
		scope      Scope
		prio, diff string
		want       bool
	}{
		{"untrusted: CRIT", Scope{}, "CRIT", "DO", true},
		{"untrusted: HIGH", Scope{}, "HIGH", "", true},
		{"untrusted: MED", Scope{Open: all}, "MED", "DO", false},
		{"not live: no bar", Scope{Valid: true}, "LOW", "ARCH", true},
		{"live: closed pair", Scope{Valid: true, Live: true}, "MED", "DO", false},
		{"live: open pair", Scope{Valid: true, Live: true, Open: all}, "MED", "HUNT", true},
		{"live: other pair", Scope{Valid: true, Live: true, Open: []string{"MED-HUNT"}}, "MED", "DO", false},
		{"live: HIGH through a closed bar", Scope{Valid: true, Live: true}, "HIGH", "DO", true},
		{"MEDIUM ticket reads MED pairs", Scope{Valid: true, Live: true, Open: []string{"MED-DO"}}, "MEDIUM", "DO", true},
		{"MED ticket reads MEDIUM pairs", Scope{Valid: true, Live: true, Open: []string{"MEDIUM-DO"}}, "MED", "DO", true},
		{"LOW ticket does not read MEDIUM pairs", Scope{Valid: true, Live: true, Open: []string{"MEDIUM-DO"}}, "LOW", "DO", false},
		{"no difficulty: all pairs open", Scope{Valid: true, Live: true, Open: all}, "MED", "", true},
		{"no difficulty: ARCH closed", Scope{Valid: true, Live: true, Open: all[:2]}, "MED", "", false},
		{"no difficulty: HUNT closed", Scope{Valid: true, Live: true, Open: []string{"MED-DO", "MED-ARCH"}}, "MED", "", false},
		{"no difficulty: DO closed", Scope{Valid: true, Live: true, Open: []string{"MED-HUNT", "MED-ARCH"}}, "MED", "", false},
	} {
		if got := tc.scope.Passes(tc.prio, tc.diff); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
