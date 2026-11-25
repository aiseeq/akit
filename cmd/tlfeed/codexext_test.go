package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcript"
	"github.com/aiseeq/akit/transcriptdb"
)

// evCol — the position of a column of eventColumns.
func evCol(t *testing.T, name string) int {
	t.Helper()
	for i, c := range strings.Split(eventColumns, ",") {
		if strings.TrimSpace(c) == name {
			return i
		}
	}
	t.Fatalf("no events column %s", name)
	return -1
}

// extRollout — a synthetic rollout: its cwd, a user message, a priced token report and one repeating its total.
func extRollout(cwd string) []byte {
	return []byte(strings.Join([]string{
		`{"timestamp":"2026-11-20T10:00:00Z","type":"session_meta","payload":{"id":"x","cwd":"` + cwd + `"}}`,
		`{"timestamp":"2026-11-20T10:00:01Z","type":"turn_context","payload":{"cwd":"` + cwd + `","model":"gpt-5"}}`,
		`{"timestamp":"2026-11-20T10:00:02Z","type":"event_msg","payload":{"type":"user_message","message":"run the task"}}`,
		`{"timestamp":"2026-11-20T10:00:03Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":3,"total_tokens":13},"last_token_usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":3,"total_tokens":13}}}}`,
		`{"timestamp":"2026-11-20T10:00:04Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":3,"total_tokens":13},"last_token_usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":3,"total_tokens":13}}}}`,
	}, "\n") + "\n")
}

func writeRootsFile(t *testing.T, home string, lines ...string) {
	t.Helper()
	write(t, transcript.CodexRootsFile(home), []byte(strings.Join(lines, "\n")+"\n"))
}

// The loader follows the rollouts of the configured Codex roots as codex-ext, a worker rollout stays codex, and
// synthetic copies of both trees under a scratch folder are not loaded. A run that appears after the start is found
// by the pass, not by a new listing.
func TestLoaderCodexExtRoots(t *testing.T) {
	h := newHarness(t)
	writeRootsFile(t, h.home, "~/bench/runs/*/codex*-home/sessions", "~/.codex/sessions")
	day := filepath.Join("2026", "11", "20")
	name := func(n string) string {
		return "rollout-2026-11-20T10-00-00-0199abcd-0000-7000-8000-00000000000" + n + ".jsonl"
	}
	bench := filepath.Join(h.home, "bench", "runs", "run-a", "codex-home", "sessions", day, name("1"))
	hands := filepath.Join(h.home, "bench", "runs", "run-b", "codex-hands-home", "sessions", day, name("2"))
	cli := filepath.Join(h.home, ".codex", "sessions", day, name("3"))
	worker := filepath.Join(h.home, ".local", "state", "worker", "job1", "codex", "sessions", day, name("4"))
	scratch := filepath.Join(h.home, ".cache", "claude-tmp", "claude-1", "proj", "sess", "scratchpad")
	copies := []string{
		filepath.Join(scratch, ".local", "state", "worker", "job", "codex", "sessions", day, name("5")),
		filepath.Join(scratch, ".codex", "sessions", day, name("6")),
	}
	write(t, bench, extRollout("/work/farm/run-a"))
	write(t, hands, extRollout("/work/farm/run-b"))
	write(t, cli, extRollout("/work/repo"))
	write(t, worker, extRollout("/work/repo"))
	for _, p := range copies {
		write(t, p, extRollout("/work/repo"))
	}
	l := h.loader(nil)
	for path, want := range map[string][2]string{
		bench:  {transcriptdb.KindCodexExt, "-work-farm-run-a"},
		hands:  {transcriptdb.KindCodexExt, "-work-farm-run-b"},
		cli:    {transcriptdb.KindCodexExt, "-work-repo"},
		worker: {"codex", "worker:job1"},
	} {
		f := h.same(l, path)
		if f.kind != want[0] || f.project != want[1] {
			t.Errorf("%s: kind %q project %q, want %q", path, f.kind, f.project, want)
		}
	}
	for _, p := range copies {
		if l.files[p] != nil {
			t.Errorf("a synthetic copy is loaded: %s", p)
		}
	}
	// rows of the files table: a restart reads them back with the same kind and project
	for _, row := range h.fake.latestFiles() {
		f, _, _, err := parseFileRow(h.home, row)
		if err != nil {
			t.Fatal(err)
		}
		if f.src.Path == cli && (f.kind != transcriptdb.KindCodexExt || f.project != "-work-repo" || row[fileCol["kind"]] != transcriptdb.KindCodexExt) {
			t.Errorf("files row of %s: stored kind %q, read kind %q project %q", cli, row[fileCol["kind"]], f.kind, f.project)
		}
	}
	// the events of a codex-ext rollout: its kind, the full rollout name as usage session, a key per priced report
	f := l.files[cli]
	kind, session, reply, line := evCol(t, "kind"), evCol(t, "usage_session"), evCol(t, "reply_id"), evCol(t, "line")
	var keys []string
	h.fake.mu.Lock()
	for _, row := range h.fake.rows["events"] {
		if row[evCol(t, "file_id")] != strconv.FormatUint(f.id, 10) {
			continue
		}
		if row[kind] != transcriptdb.KindCodexExt || row[session] != strings.TrimSuffix(name("3"), ".jsonl") {
			t.Errorf("event row kind %q usage session %q", row[kind], row[session])
		}
		if row[reply] != "" {
			keys = append(keys, row[line]+"="+row[reply])
		}
	}
	h.fake.mu.Unlock()
	if strings.Join(keys, ",") != "4=codex:4" {
		t.Errorf("usage keys %q, want one for the priced report at line 4", keys)
	}
	// a new run after the start: the pass finds it below the root's base
	late := filepath.Join(h.home, "bench", "runs", "run-c", "codex-home", "sessions", day, name("7"))
	write(t, late, extRollout("/work/farm/run-c"))
	h.pass(l)
	if got := h.same(l, late); got.kind != transcriptdb.KindCodexExt {
		t.Fatalf("late rollout kind %q", got.kind)
	}
}

// Without a roots file the loader follows ~/.codex/sessions.
func TestLoaderCodexExtDefaultRoots(t *testing.T) {
	h := newHarness(t)
	day := filepath.Join("2026", "11", "20")
	cli := filepath.Join(h.home, ".codex", "sessions", day, "rollout-2026-11-20T10-00-00-a.jsonl")
	write(t, cli, extRollout("/work/repo"))
	l := h.loader(nil)
	for _, p := range []string{cli} {
		if f := h.same(l, p); f.kind != transcriptdb.KindCodexExt {
			t.Errorf("%s: kind %q", p, f.kind)
		}
	}
	if _, err := os.Stat(transcript.CodexRootsFile(h.home)); err == nil {
		t.Fatal("the test home has a roots file")
	}
}

func TestEventRowCodexExtUsesFullRollout(t *testing.T) {
	name := "rollout-2026-11-25T10-00-00-0199abcd-1111-7000-8000-000000000001"
	var b bytes.Buffer
	eventRow(&b, &file{kind: transcriptdb.KindCodexExt, project: "-work-repo", src: transcript.Source{ID: "0199abcd", Path: "/synthetic/.codex/sessions/" + name + ".jsonl"}}, &transcript.Event{
		Line: 12, Tokens: &transcript.Tokens{Model: "gpt-5", Last: &transcript.TokenUsage{Input: 10, CachedInput: 4, Output: 3}},
	}, 0)
	fields := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\t")
	if fields[evCol(t, "kind")] != transcriptdb.KindCodexExt || fields[evCol(t, "project")] != "-work-repo" {
		t.Errorf("kind %q project %q", fields[evCol(t, "kind")], fields[evCol(t, "project")])
	}
	if got := fields[evCol(t, "reply_id")]; got != "codex:12" {
		t.Errorf("usage key %q", got)
	}
	if got := fields[evCol(t, "usage_session")]; got != name {
		t.Errorf("usage session %q, want the full rollout name %q", got, name)
	}
}

func TestProjectOfCWD(t *testing.T) {
	if got := projectOfCWD("/home/u/.cache/farm/run-1_x"); got != "-home-u--cache-farm-run-1-x" {
		t.Fatalf("projectOfCWD = %q", got)
	}
}

// recWatch — a watcher that records the folders given to it.
type recWatch struct {
	noWatch
	added []string
}

func (w *recWatch) add(dir string) { w.added = append(w.added, dir) }

// A rollout under a Codex root that changed lately has its folders up to the root's base watched; an old one's are
// left to the sweep, and the base is watched as a root.
func TestDirsWatchRecentCodexExtFolders(t *testing.T) {
	home := t.TempDir()
	writeRootsFile(t, home, "~/bench/runs/*/codex*-home/sessions")
	day := filepath.Join("2026", "11", "20")
	fresh := filepath.Join(home, "bench", "runs", "run-a", "codex-home", "sessions", day, "rollout-a.jsonl")
	old := filepath.Join(home, "bench", "runs", "run-b", "codex-home", "sessions", day, "rollout-b.jsonl")
	write(t, fresh, extRollout("/work/a"))
	write(t, old, extRollout("/work/b"))
	d := newDirs(home, func(string, ...any) {})
	w := &recWatch{}
	d.w = w
	d.addFileDir(old, time.Now().Add(-3*recentTask))
	d.addFileDir(fresh, time.Now())
	d.watchClaude()
	base := filepath.Join(home, "bench", "runs")
	for _, dir := range []string{filepath.Dir(fresh), filepath.Join(base, "run-a"), base} {
		if !slices.Contains(w.added, dir) {
			t.Errorf("%s not watched; watched %q", dir, w.added)
		}
	}
	if slices.Contains(w.added, filepath.Dir(old)) || slices.Contains(w.added, filepath.Join(base, "run-b")) {
		t.Errorf("the folders of an old rollout are watched: %q", w.added)
	}
}
