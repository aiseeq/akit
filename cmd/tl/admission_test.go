package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAdmissionQueuesHeavyReadAndPropagatesGraft125(t *testing.T) {
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(transcriptPath, []byte("synthetic transcript"), 0o600); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(dir, "args")
	childPath := filepath.Join(dir, "child-ran")
	graft := filepath.Join(dir, "graft")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argsPath + "\"\nwhile [ $# -gt 0 ] && [ \"$1\" != -- ]; do shift; done\nshift\nTL_GRAFT_HELPER='" + childPath + "' exec \"$@\"\n"
	if err := os.WriteFile(graft, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin:/bin")
	unsetEnv(t, "GRAFT_THREADS")
	oldThreshold := heavyThreshold
	heavyThreshold = 1
	t.Cleanup(func() { heavyThreshold = oldThreshold })
	var stdout, stderr bytes.Buffer
	code, done, err := admission([]source{{path: transcriptPath}}, []string{"-test.run=TestGraftHelper"}, &stdout, &stderr)
	if !done || code != 125 || err != nil {
		t.Fatalf("admission = (%d, %t, %v), want (125, true, nil); stderr: %s", code, done, err, stderr.String())
	}
	wantPrefix := "exec\n--heavy\n--local\n--\n"
	got, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), wantPrefix) || !strings.Contains(string(got), "-test.run=TestGraftHelper\n") {
		t.Fatalf("graft args %q", got)
	}
	if _, err := os.Stat(childPath); err != nil {
		t.Fatalf("graft did not run the command after --: %v", err)
	}
	if !strings.Contains(stderr.String(), "exit 125") {
		t.Fatalf("missing graft error: %q", stderr.String())
	}
}

func TestGraftHelper(t *testing.T) {
	if path := os.Getenv("TL_GRAFT_HELPER"); path != "" {
		if err := os.WriteFile(path, []byte("ran"), 0o600); err != nil {
			os.Exit(1)
		}
		os.Exit(125)
	}
}

func unsetEnv(t *testing.T, name string) {
	t.Helper()
	old, hadOld := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadOld {
			if err := os.Setenv(name, old); err != nil {
				t.Errorf("restore %s: %v", name, err)
			}
			return
		}
		if err := os.Unsetenv(name); err != nil {
			t.Errorf("unset %s: %v", name, err)
		}
	})
}

func TestConfiguredThreads(t *testing.T) {
	t.Setenv("GRAFT_THREADS", "3")
	threads, set, err := configuredThreads()
	if err != nil || !set || threads != min(3, runtime.NumCPU()) || scanWorkerCount() != threads {
		t.Fatalf("configured workers = %d, set %t, err %v", threads, set, err)
	}
	t.Setenv("GRAFT_THREADS", "invalid")
	if _, _, err := configuredThreads(); err == nil {
		t.Fatal("invalid GRAFT_THREADS accepted")
	}
}

func TestAdmissionWithoutGraftWarnsAndContinues(t *testing.T) {
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(transcriptPath, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	unsetEnv(t, "GRAFT_THREADS")
	oldThreshold := heavyThreshold
	heavyThreshold = 1
	t.Cleanup(func() { heavyThreshold = oldThreshold })
	var stdout, stderr bytes.Buffer
	code, done, err := admission([]source{{path: transcriptPath}}, nil, &stdout, &stderr)
	if done || code != 0 || err != nil || stderr.String() != "tl: graft not in PATH, reading without the machine queue\n" {
		t.Fatalf("admission = (%d, %t, %v), stderr %q", code, done, err, stderr.String())
	}
}
