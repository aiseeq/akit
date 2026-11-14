package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcript/replycache"
)

func TestReplyCacheMatchesOriginal(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "session.jsonl")
	rows := []string{
		`{"type":"user","origin":{"kind":"human"},"timestamp":"2026-10-25T10:00:00Z","message":{"role":"user","content":"alpha"}}`,
		`{"type":"user","origin":{"kind":"human"},"timestamp":"2026-10-25T10:01:00Z","message":{"role":"user","content":"beta"}}`,
		`{"type":"user","origin":{"kind":"human"},"timestamp":"2026-10-25T10:02:00Z","message":{"role":"user","content":"alpha again"}}`,
		`not json alpha`,
		`not json beta`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	sources := []source{{Path: path, ID: "session"}}
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	e := env{now: now, home: t.TempDir()}
	for _, args := range [][]string{{"--role", "user"}, {"--role", "user,ask", "--grep", "alpha"}, {"--role", "user", "--grep", "alpha", "-C", "1"}, {"--role", "ask", "--grep", ".*"}} {
		opts, err := parseArgs(args, now)
		if err != nil {
			t.Fatal(err)
		}
		original, err := scan(sources, opts, e.home)
		if err != nil {
			t.Fatal(err)
		}
		var want bytes.Buffer
		wantCode := report(original, opts, e, &want)
		cache := replycache.New()
		opts.replyCache = cache
		for i := range 2 {
			got, err := scan(sources, opts, e.home)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			code := report(got, opts, e, &out)
			if code != wantCode || out.String() != want.String() {
				t.Fatalf("%v run %d: code=%d out=%q want=%q", args, i, code, out.String(), want.String())
			}
		}
		if len(args) == 2 && cache.Misses.Load() != 1 {
			t.Fatalf("cold/warm misses: %d", cache.Misses.Load())
		}
	}
}

func TestRunUsesReplyCacheAndCleans(t *testing.T) {
	e := fakeHome(t)
	cache := replycache.New()
	oldFactory := newReplyCache
	newReplyCache = func() *replycache.Cache { return cache }
	t.Cleanup(func() { newReplyCache = oldFactory })
	obsolete := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "akit", "transcript-replies", "old-code")
	if err := os.MkdirAll(obsolete, 0700); err != nil {
		t.Fatal(err)
	}
	e.now = time.Unix(0, time.Now().UnixNano()/1024*1024)
	for range 2 {
		code, _, stderr := runTL(t, e, "--all", "--role", "user")
		if code != 0 {
			t.Fatalf("run: %d %s", code, stderr)
		}
	}
	if cache.Misses.Load() != 1 {
		t.Fatalf("misses: %d", cache.Misses.Load())
	}
	if _, err := os.Stat(obsolete); !os.IsNotExist(err) {
		t.Fatalf("cleanup: %v", err)
	}
}

func TestReplyCacheAdmissionBadHeader(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("GRAFT_THREADS", "")
	if err := os.Unsetenv("GRAFT_THREADS"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte("{broken needle\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	cache := replycache.New()
	if _, _, ok, err := cache.Events(path, false); !ok || err != nil {
		t.Fatal("cache setup")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "graft"), []byte("#!/bin/sh\nexit 37\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	old := heavyThreshold
	heavyThreshold = 1
	t.Cleanup(func() { heavyThreshold = old })
	code, done, err := admission([]source{{Path: path}}, nil, io.Discard, io.Discard, nil, cache, true)
	if err != nil || !done || code != 37 {
		t.Fatalf("bad entry admission: %d %v %v", code, done, err)
	}
}

func TestRunAdmissionRejectsBadHeader(t *testing.T) {
	e := fakeHome(t)
	t.Setenv("GRAFT_THREADS", "")
	if err := os.Unsetenv("GRAFT_THREADS"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.home, ".claude", "projects", "-home-x-proj", sessionID+".jsonl")
	if err := os.WriteFile(path, []byte("{broken needle\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	cache := replycache.New()
	if _, _, ok, err := cache.Events(path, false); !ok || err != nil {
		t.Fatal("cache setup")
	}
	oldFactory := newReplyCache
	newReplyCache = func() *replycache.Cache { return cache }
	t.Cleanup(func() { newReplyCache = oldFactory })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "graft"), []byte("#!/bin/sh\nexit 37\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	old := heavyThreshold
	heavyThreshold = 1
	t.Cleanup(func() { heavyThreshold = old })
	code, _, _ := runTL(t, e, "--all", "--role", "user", "--grep", "needle")
	if code != 37 {
		t.Fatalf("bad cache header skipped admission: %d", code)
	}
}

func TestReplyCacheAdmission(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("GRAFT_THREADS", "")
	if err := os.Unsetenv("GRAFT_THREADS"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","origin":{"kind":"human"},"timestamp":"2026-10-25T10:00:00Z","message":{"content":"hi"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cache := replycache.New()
	if _, _, ok, _ := cache.Events(path, false); !ok {
		t.Fatal("cache setup")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "graft"), []byte("#!/bin/sh\nexit 37\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	old := heavyThreshold
	heavyThreshold = 1
	t.Cleanup(func() { heavyThreshold = old })
	_, done, err := admission([]source{{Path: path}}, nil, io.Discard, io.Discard, nil, cache, false)
	if err != nil || done {
		t.Fatalf("warm entry admitted as heavy: done=%v err=%v", done, err)
	}
	if !cache.Valid(path, false) || cache.Misses.Load() != 1 {
		t.Fatal("entry not warm")
	}
}
