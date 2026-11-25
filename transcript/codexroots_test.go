package transcript

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeRolloutAt(t *testing.T, path, cwd string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"timestamp":"2026-11-20T10:00:00Z","type":"session_meta","payload":{"id":"x","cwd":"` + cwd + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeCodexRoots(t *testing.T, home string, lines ...string) {
	t.Helper()
	path := CodexRootsFile(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCodexRootsDefaultAndFile(t *testing.T) {
	home := t.TempDir()
	roots, err := CodexRoots(home)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(home, ".codex", "sessions"), filepath.Join(home, ".codex-personal", "sessions")}
	if !slices.Equal(roots, want) {
		t.Fatalf("roots without a file %q, want %q", roots, want)
	}
	writeCodexRoots(t, home, "# benchmark runs", "", "~/bench/runs/*/codex*-home/sessions  # two homes per run",
		"/srv/codex/sessions", "relative/sessions", "/bad/[run")
	roots, err = CodexRoots(home)
	want = []string{filepath.Join(home, "bench", "runs", "*", "codex*-home", "sessions"), "/srv/codex/sessions"}
	if !slices.Equal(roots, want) {
		t.Fatalf("roots of the file %q, want %q", roots, want)
	}
	if err == nil || !strings.Contains(err.Error(), "line 5") || !strings.Contains(err.Error(), "line 6") || strings.Contains(err.Error(), "line 3") {
		t.Fatalf("bad lines not named: %v", err)
	}
}

func TestCodexRootBase(t *testing.T) {
	for root, want := range map[string]string{
		"/h/bench/runs/*/codex*-home/sessions": "/h/bench/runs",
		"/h/.codex/sessions":                   "/h/.codex/sessions",
		"/*/sessions":                          "/",
		"/h/run?/sessions":                     "/h",
	} {
		if got := CodexRootBase(root); got != want {
			t.Errorf("CodexRootBase(%q) = %q, want %q", root, got, want)
		}
	}
}

// The configured roots find a benchmark-like tree and a Codex CLI home; synthetic copies of the worker tree and of
// a Codex home under a scratch folder are in neither the configured roots nor the worker glob.
func TestCodexRootSourcesFindConfiguredRootsOnly(t *testing.T) {
	home := t.TempDir()
	writeCodexRoots(t, home, "~/bench/runs/*/codex*-home/sessions", "~/.codex/sessions")
	day := filepath.Join("2026", "11", "20")
	name := "rollout-2026-11-20T10-00-00-0199abcd-0000-7000-8000-00000000000"
	bench := writeRolloutAt(t, filepath.Join(home, "bench", "runs", "run-a", "codex-home", "sessions", day, name+"1.jsonl"), "/work/farm/run-a")
	hands := writeRolloutAt(t, filepath.Join(home, "bench", "runs", "run-b", "codex-hands-home", "sessions", day, name+"2.jsonl"), "/work/farm/run-b")
	cli := writeRolloutAt(t, filepath.Join(home, ".codex", "sessions", day, name+"3.jsonl"), "/work/repo")
	scratch := filepath.Join(home, ".cache", "claude-tmp", "claude-1", "proj", "sess", "scratchpad")
	copies := []string{
		writeRolloutAt(t, filepath.Join(scratch, ".local", "state", "worker", "job", "codex", "sessions", day, name+"4.jsonl"), "/work/repo"),
		writeRolloutAt(t, filepath.Join(scratch, ".codex", "sessions", day, name+"5.jsonl"), "/work/repo"),
		writeRolloutAt(t, filepath.Join(home, "bench", "runs", "run-a", "other-home", "sessions", day, name+"6.jsonl"), "/work/repo"),
	}
	roots, err := CodexRoots(home)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := CodexRootSources(roots, time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range sources {
		got = append(got, s.Path)
	}
	if want := []string{bench, hands, cli}; !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Fatalf("sources %q, want %q", got, want)
	}
	worker, err := AllSources(home, time.Time{}, "", true, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range worker {
		if slices.Contains(copies, s.Path) {
			t.Fatalf("a synthetic copy is listed: %s", s.Path)
		}
	}
	for _, p := range copies {
		if UnderCodexRoot(roots, p) {
			t.Fatalf("a synthetic copy is under a root: %s", p)
		}
	}
	if !UnderCodexRoot(roots, bench) || !UnderCodexRoot(roots, cli) {
		t.Fatal("configured rollouts are not under a root")
	}
}

// --project picks a rollout outside worker by its cwd when its path does not name the project.
func TestCodexRootSourcesProjectMatchesCWD(t *testing.T) {
	home := t.TempDir()
	day := filepath.Join("2026", "11", "20")
	in := writeRolloutAt(t, filepath.Join(home, ".codex", "sessions", day, "rollout-2026-11-20T10-00-00-a.jsonl"), "/work/projalpha")
	writeRolloutAt(t, filepath.Join(home, ".codex", "sessions", day, "rollout-2026-11-20T10-00-00-b.jsonl"), "/work/other")
	sources, err := CodexRootSources(DefaultCodexRoots(home), time.Time{}, time.Time{}, "projalpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Path != in {
		t.Fatalf("sources %+v, want only %s", sources, in)
	}
}
