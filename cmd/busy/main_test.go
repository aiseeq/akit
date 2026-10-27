package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func proc(t *testing.T, running string) string {
	t.Helper()
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
	opts, err := parseArgs([]string{"--proc", proc(t, "2"), "--window=1ms"})
	if err != nil {
		t.Fatal(err)
	}
	var line bytes.Buffer
	if run(context.Background(), opts, &line, &line); !strings.Contains(line.String(), "; memory pressure 0.4% (some avg10, limit 10%); swapping 0 KB/s") {
		t.Errorf("строка без давления памяти: %q", line.String())
	}
	for _, c := range cases {
		opts, err := parseArgs(append([]string{"--proc", proc(t, c.running), "--window=1ms"}, c.args...))
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
	var out bytes.Buffer
	code := run(context.Background(), options{procRoot: t.TempDir()}, &out, &out)
	if code != 0 || !strings.Contains(out.String(), "not checked: no /proc/stat") {
		t.Errorf("без /proc: код %d, %q", code, out.String())
	}
}

func TestParseArgs(t *testing.T) {
	opts, err := parseArgs([]string{"--window", "2s", "--runnable=16", "--mem-some", "5.5"})
	if err != nil || opts.limits.SwapWindow != 2*time.Second || opts.limits.Runnable != 16 || opts.limits.MaxMemSome != 5.5 {
		t.Errorf("parseArgs = %+v, %v", opts, err)
	}
	for _, bad := range [][]string{{"--runnable", "0"}, {"--mem-some", "0"}, {"--window"}, {"--nope", "1"}, {"x"}} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("%v — ждали ошибку", bad)
		}
	}
}
