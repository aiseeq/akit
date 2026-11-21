package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAllSourcesWindowSkipsSourcesStartedAfterUntil(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, ".claude", "projects", "p")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	until := time.Date(2026, 11, 3, 12, 55, 0, 0, time.UTC)
	before := filepath.Join(project, "before.jsonl")
	after := filepath.Join(project, "after.jsonl")
	unknown := filepath.Join(project, "unknown.jsonl")
	rows := map[string]string{
		before:  `{"timestamp":"2026-11-03T12:54:00Z"}` + "\n" + `{"timestamp":"2026-11-10T00:00:00Z"}` + "\n",
		after:   `{"timestamp":"2026-11-03T12:56:00Z"}` + "\n" + strings.Repeat("x", 4096),
		unknown: `{"type":"metadata"}` + "\n",
	}
	for path, data := range rows {
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sources, err := AllSourcesWindow(home, time.Time{}, until, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(sources))
	for _, src := range sources {
		got[src.Path] = true
	}
	if got[after] || !got[before] || !got[unknown] {
		t.Fatalf("sources = %v; want before and unknown, skip after", got)
	}
	var size int64
	for _, src := range sources {
		info, err := os.Stat(src.Path)
		if err != nil {
			t.Fatal(err)
		}
		size += info.Size()
	}
	if size == 0 || size >= int64(len(rows[after]))+int64(len(rows[before]))+int64(len(rows[unknown])) {
		t.Fatalf("included size = %d, skipped source should not count", size)
	}
}

func TestAllSourcesProjectMatchesCodexCWDAndWorkerTree(t *testing.T) {
	home := t.TempDir()
	workerRoot := filepath.Join(home, ".local", "state", "worker")
	writeRollout := func(worker, name, firstLine string) string {
		t.Helper()
		path := filepath.Join(workerRoot, worker, "codex", "sessions", "2026", "11", "16", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(firstLine+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	alpha := writeRollout("job-alpha", "rollout-alpha.jsonl", `{"type":"session_meta","payload":{"cwd":"/home/user/worktrees/projalpha/w-x"}}`)
	other := writeRollout("job-other", "rollout-other.jsonl", `{"type":"session_meta","payload":{"cwd":"/home/user/worktrees/other/w-y"}}`)
	fallback := writeRollout("job-fallback", "rollout-fallback.jsonl", `{"type":"event_msg","payload":{}}`)
	for worker, dir := range map[string]string{"job-alpha": "/home/user/worktrees/projalpha/w-x", "job-other": "/home/user/worktrees/other/w-y", "job-fallback": "/home/user/worktrees/projalpha/w-z"} {
		data, err := json.Marshal(map[string]string{"dir": dir})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workerRoot, worker, "job.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	claudeDir := filepath.Join(home, ".claude", "projects", "-work-projalpha")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(claudeDir, "claude.jsonl")
	if err := os.WriteFile(claude, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sources, err := AllSourcesWindow(home, time.Time{}, time.Time{}, "projalpha", true, false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, src := range sources {
		got[src.Path] = true
	}
	if !got[alpha] || got[other] || !got[fallback] || !got[claude] {
		t.Fatalf("sources = %v; want codex cwd and job tree matches, plus path-matched Claude", got)
	}
}

func TestAllSourcesProjectKeepsOldCodexRunModifiedSince(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".local", "state", "worker", "job", "codex", "sessions", "2026", "11", "16", "rollout-2026-11-16T17-00-00-test.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"cwd":"/work/akit"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	modified := since.Add(time.Hour)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	sources, err := AllSourcesWindow(home, since, time.Time{}, "akit", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Path != path {
		t.Fatalf("sources = %v, want old run modified after since", sources)
	}
}

func TestCodexCWDParsesEscapedSessionMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	line := `{"\u0074ype":"session_meta","payload":{"cwd":"/work/akit"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd, err := codexCWD(path)
	if err != nil || cwd != "/work/akit" {
		t.Fatalf("codexCWD = (%q, %v), want /work/akit", cwd, err)
	}
}

func TestCodexCWDCanonicalRecordSkipsEmptyCwdAndUnescapesValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	rows := `{"type":"session_meta","payload":{"cwd":""}}` + "\n" +
		`{"type":"session_meta","payload":{"cwd":"/work/ak\u0069t"}}` + "\n"
	if err := os.WriteFile(path, []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd, err := codexCWD(path)
	if err != nil || cwd != "/work/akit" {
		t.Fatalf("codexCWD = (%q, %v), want /work/akit", cwd, err)
	}
}

func TestCodexCWDUsesEncodingJSONFieldNestingAndDuplicateRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	rows := `{"nested":{"cwd":"/work/other"},"type":"session_meta","payload":{"cwd":"/work/akit"}}` + "\n" +
		`{"type":"session_meta","payload":{"cwd":"/work/first","cwd":"/work/last"}}` + "\n"
	if err := os.WriteFile(path, []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd, err := codexCWD(path)
	if err != nil || cwd != "/work/akit" {
		t.Fatalf("codexCWD = (%q, %v), want root payload cwd /work/akit", cwd, err)
	}
	if err := os.WriteFile(path, []byte(strings.Split(rows, "\n")[1]+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd, err = codexCWD(path)
	if err != nil || cwd != "/work/last" {
		t.Fatalf("codexCWD duplicate = (%q, %v), want /work/last", cwd, err)
	}
}

func TestAllSourcesSkipsFileAtWorkerSessionsPath(t *testing.T) {
	home := t.TempDir()
	sessions := filepath.Join(home, ".local", "state", "worker", "job", "codex", "sessions")
	if err := os.MkdirAll(filepath.Dir(sessions), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessions, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AllSourcesWindow(home, time.Time{}, time.Time{}, "", true, false); err != nil {
		t.Fatalf("AllSourcesWindow with sessions file: %v", err)
	}
}

func TestAllSourcesSkipsWorkerSymlinkLoopLikeGlob(t *testing.T) {
	home := t.TempDir()
	workerRoot := filepath.Join(home, ".local", "state", "worker")
	if err := os.MkdirAll(workerRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	loop := filepath.Join(workerRoot, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink privileges unavailable on Windows: %v", err)
		}
		t.Fatalf("create synthetic symlink loop: %v", err)
	}
	sources, err := AllSourcesWindow(home, time.Time{}, time.Time{}, "", true, false)
	if err != nil {
		t.Fatalf("AllSourcesWindow on worker symlink loop: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("sources = %v, want no matches", sources)
	}
}

func TestAllSourcesWorkerGlobDoesNotMatchHiddenComponents(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".local", "state", "worker")
	visible := filepath.Join(root, "job", "codex", "sessions", "2026", "11", "16", "rollout-visible.jsonl")
	hidden := filepath.Join(root, ".hidden", "codex", "sessions", "2026", "11", "16", "rollout-hidden.jsonl")
	for _, path := range []string{visible, hidden} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	sources, err := AllSourcesWindow(home, time.Time{}, time.Time{}, "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Path != visible {
		t.Fatalf("sources = %+v, want only %s", sources, visible)
	}
}

func TestAllSourcesProjectCodexMetadataFallbacks(t *testing.T) {
	home := t.TempDir()
	workerRoot := filepath.Join(home, ".local", "state", "worker")
	writeRollout := func(worker, body string) string {
		t.Helper()
		path := filepath.Join(workerRoot, worker, "codex", "sessions", "2026", "11", "16", "rollout-2026-11-16T17-00-00-test.jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	metaLater := writeRollout("meta-later", `{"type":"event_msg"}`+"\n"+`{"type":"session_meta","payload":{"cwd":"/work/projalpha/w"}}`+"\n")
	turnContextOnly := writeRollout("turn-context", `{"type":"turn_context","payload":{"cwd":"/work/projalpha/w"}}`+"\n")
	longLine := writeRollout("long-line", `{"pad":"`+strings.Repeat("x", 2<<20)+`"}`+"\n"+`{"type":"session_meta","payload":{"cwd":"/work/projalpha/w"}}`+"\n")
	missingJob := writeRollout("missing-job", `{"type":"event_msg"}`+"\n")
	badJob := writeRollout("bad-job", `{"type":"event_msg"}`+"\n")
	for worker, dir := range map[string]string{
		"long-line": "/work/projalpha/w-fallback",
	} {
		if err := os.WriteFile(filepath.Join(workerRoot, worker, "job.json"), []byte(`{"dir":"`+dir+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workerRoot, "bad-job", "job.json"), []byte(`{"dir":`), 0o644); err != nil {
		t.Fatal(err)
	}

	sources, err := AllSourcesWindow(home, time.Time{}, time.Time{}, "projalpha", true, false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, src := range sources {
		got[src.Path] = true
	}
	if !got[metaLater] || !got[longLine] || got[turnContextOnly] || got[missingJob] || got[badJob] {
		t.Fatalf("sources = %v; want later session_meta and long-line job fallback only", got)
	}

	wrongBase := filepath.Join(workerRoot, "not-a-codex-job", "notcodex", "sessions", "2026", "11", "16", "rollout-test.jsonl")
	if tree, err := workerTree(wrongBase); err != nil || tree != "" {
		t.Fatalf("workerTree(%q) = (%q, %v), want empty tree outside codex directory", wrongBase, tree, err)
	}
}

func TestAllSourcesProjectDoesNotReadClaudeOrOldCodexForCWD(t *testing.T) {
	home := t.TempDir()
	claude := filepath.Join(home, ".claude", "projects", "other-project", "fifo.jsonl")
	old := filepath.Join(home, ".local", "state", "worker", "old-job", "codex", "sessions", "2026", "11", "01", "rollout-2026-11-01T00-00-00-test.jsonl")
	for _, path := range []string{claude, old} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	sources, err := AllSourcesWindow(home, time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), time.Time{}, "projalpha", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 0 {
		t.Fatalf("sources = %v, want none", sources)
	}
}

// codex names a rollout by its local start time, not UTC.
func TestSourceStartFromCodexRolloutName(t *testing.T) {
	local := time.Local
	time.Local = time.FixedZone("CET", 3600)
	t.Cleanup(func() { time.Local = local })
	got, ok, err := sourceStart("rollout-2026-11-03T12-54-00-1234.jsonl")
	want := time.Date(2026, 11, 3, 11, 54, 0, 0, time.UTC)
	if err != nil || !ok || !got.Equal(want) {
		t.Fatalf("sourceStart = (%v, %t, %v), want (%v, true, nil)", got, ok, err, want)
	}
}

// A first line too long to scan leaves the start unknown instead of failing the listing.
func TestSourceStartLongFirstLineIsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "long.jsonl")
	data := `{"pad":"` + strings.Repeat("x", 2<<20) + `"}` + "\n" + `{"timestamp":"2026-11-03T12:56:00Z"}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := sourceStart(path); err != nil || ok {
		t.Fatalf("sourceStart = (%t, %v), want unknown without error", ok, err)
	}
}

func TestAllSourcesWalkOrderAllowsMissingRoots(t *testing.T) {
	home := t.TempDir()
	sources, err := AllSourcesWalkOrder(home, time.Time{}, "", true, true)
	if err != nil {
		t.Fatalf("AllSourcesWalkOrder with absent roots: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("sources = %v, want none", sources)
	}

	projects := filepath.Join(home, ".claude", "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(home, ".local", "state", "worker")
	if err := os.MkdirAll(filepath.Dir(worker), 0o755); err != nil {
		t.Fatal(err)
	}
	loop := filepath.Join(filepath.Dir(worker), "loop")
	if err := os.Symlink(loop, worker); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(worker, loop); err != nil {
		t.Fatal(err)
	}
	if _, err := AllSourcesWalkOrder(home, time.Time{}, "", true, false); err == nil {
		t.Fatal("non-NotExist root error was ignored")
	}
}

// A transcript whose start cannot be read stays listed: reading it later reports it
// as unreadable, while the other transcripts are still read.
func TestAllSourcesWindowKeepsUnreadableStart(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, ".claude", "projects", "p")
	if err := os.MkdirAll(filepath.Join(project, "dir.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(project, "good.jsonl")
	if err := os.WriteFile(good, []byte(`{"timestamp":"2026-01-01T10:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sources, err := AllSourcesWindow(home, time.Time{}, time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC), "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, src := range sources {
		got[filepath.Base(src.Path)] = true
	}
	if !got["good.jsonl"] || !got["dir.jsonl"] {
		t.Fatalf("sources = %v; want both kept", got)
	}
}
