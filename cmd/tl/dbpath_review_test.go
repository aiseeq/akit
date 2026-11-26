package main

import (
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
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)

	alias := filepath.Join(parent, "project-alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
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
		probeComparePath(t, e, "looping project link", dbOtherSession, false, true)
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
