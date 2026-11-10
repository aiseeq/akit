package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scopeTicket writes an open (or in-work, with an executor) HIGH ticket.
func scopeTicket(t *testing.T, dir, slug, status, executor string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	head := "# " + slug + "\n\nСтатус: " + status + "\n"
	if executor != "" {
		head += "Исполнитель: " + executor + "\n"
	}
	body := head + "\n## Приёмка\n- check\n\n## Журнал\n- 2026-11-01 10:00 открыт — someone\n"
	if err := os.WriteFile(filepath.Join(dir, "20261101-1000-HIGH-"+slug+".md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A shared root holds tickets of several projects. Closing one there, tk done
// names the next ticket from the executor's own project, not from the root.
func TestDoneHintsNextInExecutorProject(t *testing.T) {
	type setup struct {
		executor string
		names    string
		dirs     []string // project folders relative to HOME that get one open ticket each
	}
	run := func(t *testing.T, s setup) (string, string) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		root := filepath.Join(home, "shared")
		dir := filepath.Join(root, "docs", "tasks")
		scopeTicket(t, dir, "closing", "в работе", s.executor)
		scopeTicket(t, dir, "root-open", "открыт", "")
		for _, d := range s.dirs {
			scopeTicket(t, filepath.Join(home, d, "docs", "tasks"), filepath.Base(d)+"-own", "открыт", "")
		}
		if s.names != "" {
			namesDir := filepath.Join(home, ".config", "cl")
			if err := os.MkdirAll(namesDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(namesDir, "names.tsv"), []byte(s.names), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		var out, errout bytes.Buffer
		o := options{cmd: "done", id: "closing", by: "checker", check: "checked by test", baseRoot: root}
		if code := runLockedCommand(root, dir, o, &out, &errout); code != 0 {
			t.Fatalf("done = %d, stderr %q", code, errout.String())
		}
		return out.String(), errout.String()
	}

	t.Run("project folder inside the root by master name", func(t *testing.T) {
		out, _ := run(t, setup{executor: "alpha-master", dirs: []string{"shared/alpha"}})
		if !strings.Contains(out, "следующий в alpha: alpha-own ") || strings.Contains(out, "root-open") {
			t.Errorf("output %q", out)
		}
	})
	t.Run("project folder beside the root by master name", func(t *testing.T) {
		out, _ := run(t, setup{executor: "beta-master", dirs: []string{"beta"}})
		if !strings.Contains(out, "следующий в beta: beta-own ") || strings.Contains(out, "root-open") {
			t.Errorf("output %q", out)
		}
	})
	t.Run("session names table maps the executor to its project", func(t *testing.T) {
		out, _ := run(t, setup{executor: "custom", names: "~/shared/gamma\tcustom\n", dirs: []string{"shared/gamma"}})
		if !strings.Contains(out, "следующий в gamma: gamma-own ") || strings.Contains(out, "root-open") {
			t.Errorf("output %q", out)
		}
	})
	t.Run("executor's project with an empty queue gives no hint from the root", func(t *testing.T) {
		h := t.TempDir()
		t.Setenv("HOME", h)
		root := filepath.Join(h, "shared")
		dir := filepath.Join(root, "docs", "tasks")
		scopeTicket(t, dir, "closing", "в работе", "eps-master")
		scopeTicket(t, dir, "root-open", "открыт", "")
		if err := os.MkdirAll(filepath.Join(root, "eps", "docs", "tasks"), 0o755); err != nil {
			t.Fatal(err)
		}
		var out, errout bytes.Buffer
		o := options{cmd: "done", id: "closing", by: "checker", check: "checked by test", baseRoot: root}
		if code := runLockedCommand(root, dir, o, &out, &errout); code != 0 {
			t.Fatalf("done = %d, stderr %q", code, errout.String())
		}
		if strings.Contains(out.String(), "следующий") {
			t.Errorf("output %q", out.String())
		}
	})
	t.Run("the root's own master keeps the root queue", func(t *testing.T) {
		out, _ := run(t, setup{executor: "root-admin", names: "~/shared\troot-admin\n", dirs: []string{"shared/alpha"}})
		if !strings.Contains(out, "следующий: root-open ") {
			t.Errorf("output %q", out)
		}
	})
	t.Run("an executor with no project keeps the root queue", func(t *testing.T) {
		out, _ := run(t, setup{executor: "stranger", dirs: []string{"shared/alpha"}})
		if !strings.Contains(out, "следующий: root-open ") {
			t.Errorf("output %q", out)
		}
	})
	t.Run("two folders match the master name: no guess, a warning", func(t *testing.T) {
		out, errout := run(t, setup{executor: "zeta-master", dirs: []string{"shared/zeta", "zeta"}})
		if strings.Contains(out, "следующий") || !strings.Contains(errout, "zeta") {
			t.Errorf("output %q, stderr %q", out, errout)
		}
	})
}
