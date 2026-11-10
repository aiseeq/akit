package transcript

import (
	"os"
	"path/filepath"
	"strings"
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
