package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/pressure"
)

func proc(t *testing.T, running string) string {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())  // файл тихого режима — свой у теста, не настоящий машины
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // и пороги пользователя
	root := t.TempDir()
	files := map[string]string{
		"stat":            "cpu  1\ncpu0 1\ncpu1 1\nprocs_running " + running + "\n",
		"pressure/io":     "full avg10=0 avg60=0.5 avg300=0 total=0\n",
		"pressure/memory": "some avg10=0.38 avg60=0 avg300=0 total=0\n",
		"vmstat":          "pswpin 1\npswpout 1\n",
	}
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestRun(t *testing.T) {
	cases := []struct {
		name, running string
		args          []string
		code          int
		want          string
	}{
		{"свободна", "2", nil, 0, "free; runnable 1 (busy at 2, threads 2)"},
		{"занята по потокам", "3", nil, exitBusy, "busy: runnable 2 is at or above 2; runnable 2"},
		{"свой порог", "3", []string{"--runnable", "5"}, 0, "free; runnable 2 (busy at 5"},
	}
	opts, err := parseArgs([]string{"--proc", proc(t, "2"), "--window=1ms"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var line bytes.Buffer
	if run(context.Background(), opts, &line, &line); !strings.Contains(line.String(), "; memory pressure 0.4% (some avg10, limit 10%); swapping 0 KB/s") {
		t.Errorf("строка без давления памяти: %q", line.String())
	}
	for _, c := range cases {
		opts, err := parseArgs(append([]string{"--proc", proc(t, c.running), "--window=1ms"}, c.args...), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		code := run(context.Background(), opts, &out, &errOut)
		if code != c.code || !strings.HasPrefix(out.String(), c.want) {
			t.Errorf("%s: код %d, вывод %q %q", c.name, code, out.String(), errOut.String())
		}
	}
}

func TestRunNoProc(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	var out bytes.Buffer
	code := run(context.Background(), options{procRoot: t.TempDir()}, &out, &out)
	if code != 0 || !strings.Contains(out.String(), "not checked: no /proc/stat") {
		t.Errorf("без /proc: код %d, %q", code, out.String())
	}
}

func TestLineIOWindow(t *testing.T) {
	limits := pressure.Limits{MaxIOFull: 9.6}
	withSource := line(pressure.Verdict{Load: pressure.Load{IOFull: 9.6, IOWindow: 5 * time.Second, Disk: &pressure.DiskNow{}}}, limits)
	if !strings.Contains(withSource, "of 5s from the disk state source") {
		t.Errorf("строка с источником = %q", withSource)
	}
	fallback := line(pressure.Verdict{Load: pressure.Load{IOFull: 9.6, IOWindow: time.Second, DiskFault: pressure.ErrNoIOSource}, Skipped: []error{pressure.ErrNoIOSource}}, limits)
	if !strings.Contains(fallback, "of 1s") || !strings.Contains(fallback, "no disk state source") {
		t.Errorf("строка fallback = %q", fallback)
	}
}

func TestParseArgs(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "akit"), 0o755); err != nil {
		t.Fatal(err)
	}
	limits := filepath.Join(cfg, "akit", "pressure.yaml")
	if err := os.WriteFile(limits, []byte("runnable: 12\nmax_io_full: 50\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if opts, err := parseArgs([]string{"--io-full", "20"}, time.Now()); err != nil || opts.limits.Runnable != 12 || opts.limits.MaxIOFull != 20 {
		t.Errorf("пороги пользователя под флагами = %+v, %v; ждали runnable 12 из файла, io 20 из флага", opts.limits, err)
	}
	if err := os.WriteFile(limits, []byte("runable: 12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseArgs(nil, time.Now()); err == nil || !strings.Contains(err.Error(), "runable") {
		t.Errorf("опечатка в ключе = %v; ждали ошибку с ключом", err)
	}
	// Битый файл нужен только замеру: справка и тихий режим работают.
	for _, args := range [][]string{{"-h"}, {"--quiet-off"}, {"--quiet", "5m", "--why", "call"}} {
		if _, err := parseArgs(args, time.Now()); err != nil && !errors.Is(err, errHelp) {
			t.Errorf("parseArgs(%q) с битым файлом = %v", args, err)
		}
	}
	if _, err := parseArgs([]string{"--runnable", "500"}, time.Now()); err == nil {
		t.Error("флаг порога обошёл битый файл")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	opts, err := parseArgs([]string{"--window", "2s", "--runnable=16", "--mem-some", "5.5"}, time.Now())
	if err != nil || opts.limits.SwapWindow != 2*time.Second || opts.limits.Runnable != 16 || opts.limits.MaxMemSome != 5.5 {
		t.Errorf("parseArgs = %+v, %v", opts, err)
	}
	for _, bad := range [][]string{{"--runnable", "0"}, {"--mem-some", "0"}, {"--window"}, {"--nope", "1"}, {"x"}, {"--io-full", "NaN"}, {"--mem-some", "Inf"}, {"--runnable", "nan"}, {"--window", "0"}, {"--window", "-1s"}} {
		if _, err := parseArgs(bad, time.Now()); err == nil {
			t.Errorf("%v — ждали ошибку", bad)
		}
	}
}

func TestQuiet(t *testing.T) {
	root := proc(t, "1")
	call := func(args ...string) (int, string) {
		t.Helper()
		opts, err := parseArgs(append([]string{"--proc", root, "--window=1ms"}, args...), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		code := run(context.Background(), opts, &out, &out)
		return code, out.String()
	}
	if code, out := call("--quiet", "1h"); code != 0 || !strings.HasPrefix(out, "quiet mode until ") {
		t.Fatalf("--quiet 1h: код %d, %q", code, out)
	}
	if code, out := call(); code != exitBusy || !strings.HasPrefix(out, "busy: quiet mode until ") || !strings.Contains(out, "runnable 0") {
		t.Errorf("в тихом режиме: код %d, %q", code, out)
	}
	if code, out := call("--ignore-quiet"); code != 0 || !strings.HasPrefix(out, "free; ") || !strings.Contains(out, "; ignored: quiet mode until ") {
		t.Errorf("--ignore-quiet в тихом режиме: код %d, %q", code, out)
	}
	if code, out := call("--quiet-off"); code != 0 || !strings.HasPrefix(out, "quiet mode off") {
		t.Errorf("--quiet-off: код %d, %q", code, out)
	}
	if code, out := call(); code != 0 || !strings.HasPrefix(out, "free; ") {
		t.Errorf("после --quiet-off: код %d, %q", code, out)
	}
	if code, out := call("--ignore-quiet"); code != 0 || strings.Contains(out, "ignored") {
		t.Errorf("--ignore-quiet без тихого режима: код %d, %q", code, out)
	}
}

func TestQuietBySession(t *testing.T) {
	root := proc(t, "1")
	home := t.TempDir()
	t.Setenv("HOME", home)
	registry := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(registry, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry, "4242.json"), []byte(`{"pid":4242,"name":"planner"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PID", "4242")
	call := func(args ...string) (int, string) {
		t.Helper()
		opts, err := parseArgs(append([]string{"--proc", root, "--window=1ms"}, args...), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		code := run(context.Background(), opts, &out, &out)
		return code, out.String()
	}
	if code, out := call("--quiet", "1h", "--why", "call"); code != 0 {
		t.Fatalf("--quiet 1h --why: код %d, %q", code, out)
	}
	if code, out := call(); code != exitBusy || !strings.Contains(out, " (planner: call); ") {
		t.Errorf("пометка в строке: код %d, %q", code, out)
	}
	t.Setenv("CLAUDE_PID", "4243") // сессии нет в реестре
	if code, out := call("--quiet", "1h"); code != 0 {
		t.Fatalf("--quiet 1h без записи реестра: код %d, %q", code, out)
	}
	if code, out := call(); code != exitBusy || strings.Contains(out, "planner") {
		t.Errorf("без записи реестра: код %d, %q", code, out)
	}
	t.Setenv("CLAUDE_PID", "x")
	if code, out := call("--quiet", "1h"); code != exitUsage || !strings.Contains(out, "CLAUDE_PID") {
		t.Errorf("CLAUDE_PID не число: код %d, %q", code, out)
	}
}

func TestParseQuiet(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.Local)
	opts, err := parseArgs([]string{"--quiet-until", "14:45"}, now)
	if err != nil || !opts.quiet.Equal(now.Add(2*time.Hour+45*time.Minute)) {
		t.Errorf("--quiet-until 14:45 = %v, %v", opts.quiet, err)
	}
	if opts, err = parseArgs([]string{"--quiet=2h"}, now); err != nil || !opts.quiet.Equal(now.Add(2*time.Hour)) {
		t.Errorf("--quiet 2h = %v, %v", opts.quiet, err)
	}
	if opts, err = parseArgs([]string{"--quiet=2h", "--why", " call "}, now); err != nil || opts.why != "call" {
		t.Errorf("--why = %q, %v", opts.why, err)
	}
	for _, bad := range [][]string{
		{"--quiet-until", "11:00"}, {"--quiet-until", "2h"}, {"--quiet", "0s"}, {"--quiet", "1h", "--quiet-off"},
		{"--why", "call"}, {"--quiet", "1h", "--why", " "}, {"--quiet", "1h", "--ignore-quiet"}, {"--quiet-off", "--ignore-quiet"},
	} {
		if _, err := parseArgs(bad, now); err == nil {
			t.Errorf("%v — ждали ошибку", bad)
		}
	}
}
