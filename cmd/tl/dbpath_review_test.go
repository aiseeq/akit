package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRelativeTranscriptPathsMatchFilesAndDB(t *testing.T) {
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalDir); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	e, paths, unloaded := dbCorpus(t)
	project := filepath.Dir(paths[0])
	projectName := filepath.Base(project)
	parent := filepath.Dir(project)
	rollout := paths[len(paths)-1]
	target := filepath.Join(project, "x.jsonl")
	writeWorkFile(t, target, []string{userRecord("2026-10-25T11:00:00.000Z", "synthetic relative path")})
	paths = append(paths, target)
	filePaths, err := transcriptPaths(e.home)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(filePaths, paths[2]) || !slices.Contains(filePaths, paths[1]) {
		t.Fatalf("file path search misses parent or subagent: parent=%t subagent=%t", slices.Contains(filePaths, paths[1]), slices.Contains(filePaths, paths[2]))
	}
	alias := filepath.Join(parent, "project-alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range slices.Clone(paths) { // the loader lists the files of a project link under the link too
		if rest, ok := strings.CutPrefix(path, project+string(filepath.Separator)); ok {
			paths = append(paths, filepath.Join(alias, rest))
		}
	}
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	aliasDotDot := filepath.Join(e.home, "alias-dotdot")
	if err := os.Symlink(project, aliasDotDot); err != nil {
		t.Fatal(err)
	}
	fromDir := filepath.Join(parent, "from")
	if err := os.Mkdir(fromDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rolloutDir := filepath.Dir(rollout)
	forms := []struct {
		name, dir, arg string
		valid          bool
	}{
		{"project basename", project, "x.jsonl", true},
		{"project dot path", project, "./x.jsonl", true},
		{"parent project path", fromDir, filepath.Join("..", projectName, "x.jsonl"), true},
		{"symlink project path", parent, filepath.Join("project-alias", "x.jsonl"), true},
		{"symlink dot dot path", e.home, "alias-dotdot/../" + projectName + "/x.jsonl", true},
		{"agent path", project, filepath.Join(dbOtherSession, "subagents", "agent-a1b2c3d4.jsonl"), true},
		{"Codex rollout absolute", project, rollout, true},
		{"Codex rollout relative", rolloutDir, filepath.Base(rollout), true},
		{"line reference", project, "x.jsonl:1", true},

		{"directory path", project, "./", false},
	}
	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			if err := os.Chdir(form.dir); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Chdir(project); err != nil {
					t.Errorf("restore working directory: %v", err)
				}
			})
			probeComparePath(t, e, form.name, form.arg, false, form.valid)
		})
	}
	t.Run("uuid in a linked project", func(t *testing.T) {
		// The loader lists the transcript under the link and under the project: ambiguous on both backends.
		probeComparePath(t, e, "uuid in a linked project", dbOtherSession, true, false)
	})
	t.Run("missing relative path", func(t *testing.T) {
		if err := os.Chdir(project); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(project) })
		probeComparePath(t, e, "missing relative path", "missing.jsonl", true, false)
	})
	t.Run("missing path through symlink and dot dot", func(t *testing.T) {
		// far/.. is parent/from by the kernel; a lexical clean would name the existing parent/<project>/x.jsonl.
		if err := os.Mkdir(filepath.Join(fromDir, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(fromDir, "sub"), filepath.Join(parent, "far")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(parent); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(project) })
		arg := "far/../" + projectName + "/x.jsonl"
		e.backend = backendFiles
		if code, out, _ := runTL(t, e, arg, "--role", "all", "--max", "0"); code == 0 || out != "" {
			t.Fatalf("files %q: exit %d, output %q; want a missing transcript", arg, code, out)
		}
		probeComparePath(t, e, "missing path through symlink and dot dot", arg, true, false)
	})
	t.Run("looping project link", func(t *testing.T) {
		if err := os.Symlink("loop", filepath.Join(parent, "loop")); err != nil {
			t.Fatal(err)
		}
		// The project link makes the Claude ids ambiguous on both backends, as the loader lists both names; a rollout is not.
		probeComparePath(t, e, "looping project link", findUUID(filepath.Base(rollout)), false, true)
	})
}

func probeComparePath(t *testing.T, e env, label, arg string, allowMissing, wantTranscript bool) {
	t.Helper()
	e.backend = backendFiles
	codeF, outF, errF := runTL(t, e, arg, "--role", "all", "--max", "0")
	e.backend = backendDB
	codeD, outD, errD := runTL(t, e, arg, "--role", "all", "--max", "0")
	errComparable := errD
	if allowMissing {
		if warning, rest, ok := strings.Cut(errD, "\n"); ok && strings.Contains(warning, "reading files instead") {
			errComparable = rest
		}
	}
	if codeF != codeD || outF != outD || errF != errComparable || !allowMissing && strings.Contains(errD, "is not in the database") {
		t.Errorf("%s %q: files exit %d, db exit %d\nfiles out:\n%s\ndb out:\n%s\nfiles err: %s\ndb err: %s", label, arg, codeF, codeD, outF, outD, errF, errD)
	}
	if wantTranscript && (codeF != 0 || codeD != 0 || outF == "" || outD == "") {
		t.Errorf("%s %q: expected both backends to succeed with output; files exit %d, db exit %d; files output %q; db output %q", label, arg, codeF, codeD, outF, outD)
	}
}

func probeCompare(t *testing.T, e env, label string, args ...string) {
	t.Helper()
	e.backend = backendFiles
	codeF, outF, errF := runTL(t, e, args...)
	e.backend = backendDB
	codeD, outD, errD := runTL(t, e, args...)
	if codeF != codeD || outF != outD || errF != errD {
		t.Errorf("%s %q: files exit %d, db exit %d\nfiles out:\n%s\nfiles err: %s\ndb out:\n%s\ndb err: %s", label, args, codeF, codeD, outF, errF, outD, errD)
	}
}

func TestProbeBadRecordUnderGrep(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	bad := writeReviewClaude(t, e, "-home-x-proj", "7e7e7e7e-0000-4000-8000-000000000003",
		userRecord("2026-10-25T10:20:00.000Z", "обычная строка needle"),
		`{"type":"user","timestamp":"not-a-time","message":{"role":"user","content":"без иглы"}}`,
		userRecord("2026-10-25T10:20:02.000Z", "ещё строка"))
	paths = append(paths, bad)
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	probeCompare(t, e, "bad record, needle elsewhere", "--all", "--grep", "needle", "--max", "0")
	probeCompare(t, e, "bad record, no grep", "--all", "--max", "0")
}

func TestProbeTextEndsNewlineDollar(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	nl := writeReviewClaude(t, e, "-home-x-proj", "6b6b6b6b-0000-4000-8000-000000000006",
		userRecord("2026-10-25T10:50:00.000Z", "Error\nX tail"))
	paths = append(paths, nl)
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	probeCompare(t, e, "dot over newline, case-sensitive", "--all", "--grep", "(?-i)Error.X", "--max", "0")
}

// The loader stores transcript paths as spelled under HOME; a HOME or a projects root reached through a symlink must
// not turn the argument into a resolved path the base does not hold.
func TestTranscriptPathsThroughLinkedHome(t *testing.T) {
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalDir); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	e, paths, unloaded := dbCorpus(t)
	real := e.home
	codexRel := filepath.Join(".codex", "sessions", "2026", "10", "25", "rollout-2026-10-25T10-20-00-0199bbbb-0000-7000-8000-000000000001.jsonl")
	writeWorkFile(t, filepath.Join(real, codexRel), extRolloutRows("/work/extproj", "synthetic codex session"))
	paths = append(paths, filepath.Join(real, codexRel))
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	e.home = link
	for i, p := range paths {
		paths[i] = link + strings.TrimPrefix(p, real)
	}
	statePath := filepath.Join(os.Getenv("XDG_STATE_HOME"), "akit", "tlfeed", "state.json")
	var state dbState
	if b, err := os.ReadFile(statePath); err != nil || json.Unmarshal(b, &state) != nil {
		t.Fatalf("read feeder state: %v", err)
	}
	state.Home = link
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	name := filepath.Join(".claude", "projects", "-home-x-proj", dbOtherSession+".jsonl")
	rollout := paths[len(paths)-2]
	for _, form := range []struct{ label, dir, arg string }{
		{"codex session through the home link", real, filepath.Join(link, codexRel)},
		{"home-relative codex session", real, "~/" + codexRel},
		{"resolved codex session", real, filepath.Join(real, codexRel)},
		{"relative codex session", filepath.Join(real, filepath.Dir(codexRel)), filepath.Base(codexRel)},
		{"path through the home link", real, filepath.Join(link, name)},
		{"resolved path", real, filepath.Join(real, name)},
		{"home-relative path", real, "~/" + name},
		{"relative path from the resolved project", filepath.Join(real, filepath.Dir(name)), dbOtherSession + ".jsonl"},
		{"uuid", real, dbOtherSession},
		{"resolved rollout", real, real + strings.TrimPrefix(rollout, link)},
	} {
		t.Run(form.label, func(t *testing.T) {
			if err := os.Chdir(form.dir); err != nil {
				t.Fatal(err)
			}
			probeComparePath(t, e, form.label, form.arg, false, true)
		})
	}
	e.backend = backendFiles
	if code, out, errOut := runTL(t, e, real+strings.TrimPrefix(rollout, link), "--role", "all", "--max", "0"); code != 0 || strings.Contains(out, real) {
		t.Errorf("resolved rollout: exit %d, the output names the resolved path instead of a short id:\n%s\n%s", code, out, errOut)
	}
}

func TestTranscriptPathsThroughLinkedProjectsRoot(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	// The projects root goes to a folder in home, the Codex sessions root out of home: a name under home may still
	// go through a root link, and a resolved name lies under no spelled root.
	projects := filepath.Join(e.home, ".claude", "projects")
	store := filepath.Join(e.home, "store", "projects")
	if err := os.MkdirAll(filepath.Dir(store), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(projects, store); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store, projects); err != nil {
		t.Fatal(err)
	}
	codexStore := filepath.Join(t.TempDir(), "codex-sessions")
	codexRel := filepath.Join("2026", "10", "25", "rollout-2026-10-25T10-20-00-0199bbbb-0000-7000-8000-000000000001.jsonl")
	writeWorkFile(t, filepath.Join(codexStore, codexRel), extRolloutRows("/work/extproj", "synthetic codex session"))
	if err := os.MkdirAll(filepath.Join(e.home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(codexStore, filepath.Join(e.home, ".codex", "sessions")); err != nil {
		t.Fatal(err)
	}
	paths = append(paths, filepath.Join(e.home, ".codex", "sessions", codexRel))
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	probeComparePath(t, e, "resolved codex session", filepath.Join(codexStore, codexRel), false, true)
	workers := filepath.Join(e.home, ".local", "state", "worker")
	workerStore := filepath.Join(t.TempDir(), "worker")
	if err := os.Rename(workers, workerStore); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(workerStore, workers); err != nil {
		t.Fatal(err)
	}
	rollout := paths[len(paths)-2]
	rest, ok := strings.CutPrefix(rollout, workers)
	if !ok {
		t.Fatalf("rollout %s is not under %s", rollout, workers)
	}
	probeComparePath(t, e, "resolved worker rollout", workerStore+rest, false, true)
	resolved := filepath.Join(store, "-home-x-proj", dbOtherSession+".jsonl")
	for _, arg := range []string{dbOtherSession, resolved} {
		probeComparePath(t, e, "linked projects root", arg, false, true)
		e.backend = backendFiles
		if _, out, _ := runTL(t, e, arg, "--role", "all", "--max", "0"); strings.Contains(out, store) {
			t.Errorf("%q: the output names the resolved projects root:\n%s", arg, out)
		}
	}
}

// A project link out of the projects root is a project of its own: the loader stores its transcripts under the link.
func TestTranscriptPathsThroughProjectLinkOutOfRoot(t *testing.T) {
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalDir); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	e, paths, unloaded := dbCorpus(t)
	projects := filepath.Join(e.home, ".claude", "projects")
	outside := filepath.Join(t.TempDir(), "outside-project")
	const id = "5c5c5c5c-0000-4000-8000-000000000005"
	writeWorkFile(t, filepath.Join(outside, id+".jsonl"), []string{userRecord("2026-10-25T11:10:00.000Z", "synthetic linked project")})
	if err := os.Symlink(outside, filepath.Join(projects, "linked-out")); err != nil {
		t.Fatal(err)
	}
	paths = append(paths, filepath.Join(projects, "linked-out", id+".jsonl"))
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	if err := os.Chdir(projects); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{id, filepath.Join("linked-out", id+".jsonl")} {
		probeComparePath(t, e, "project link out of the root", arg, false, true)
	}
}
