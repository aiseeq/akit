package transcript

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
