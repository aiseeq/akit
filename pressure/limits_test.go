package pressure

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLimitsPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	if p, err := LimitsPath(); err != nil || p != "/cfg/akit/pressure.yaml" {
		t.Errorf("LimitsPath = %q, %v", p, err)
	}
}

func TestReadLimits(t *testing.T) {
	dir := t.TempDir()
	if l, err := ReadLimits(filepath.Join(dir, "none.yaml")); err != nil || l != DefaultLimits {
		t.Errorf("нет файла = %+v, %v; ждали DefaultLimits", l, err)
	}
	path := filepath.Join(dir, "pressure.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("# пороги\nrunnable: 16\nmax_io_full: 100   # диск не судить\n\nswap_window: 10ms\n")
	l, err := ReadLimits(path)
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultLimits
	want.Runnable, want.MaxIOFull, want.SwapWindow = 16, 100, 10*time.Millisecond
	if l != want {
		t.Errorf("ReadLimits = %+v; ждали %+v", l, want)
	}
	for body, wantErr := range map[string]string{
		"runable: 16\n":              ":1: runable: unknown key",
		"runnable 16\n":              ":1: want `key: value`",
		"runnable:\n":                ":1: want `key: value`",
		"\nmax_mem_some: x\n":        ":2: max_mem_some: number",
		"max_swap_kbs: 0\n":          ":1: max_swap_kbs: must be positive",
		"swap_window: 1\n":           ":1: swap_window: duration",
		"swap_window: -1s\n":         ":1: swap_window: must be positive",
		"runnable: 8\nrunnable: 9\n": ":2: runnable set twice",
	} {
		write(body)
		if _, err := ReadLimits(path); err == nil || !strings.Contains(err.Error(), path+wantErr) {
			t.Errorf("%q: ошибка %v; ждали %q", body, err, path+wantErr)
		}
	}
}

func TestUserLimits(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	if l, err := UserLimits(); err != nil || l != DefaultLimits {
		t.Errorf("без файла = %+v, %v", l, err)
	}
	if err := os.MkdirAll(filepath.Join(cfg, "akit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "akit", "pressure.yaml"), []byte("max_mem_some: 5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if l, err := UserLimits(); err != nil || l.MaxMemSome != 5 || l.MaxIOFull != DefaultLimits.MaxIOFull {
		t.Errorf("с файлом = %+v, %v", l, err)
	}
}
