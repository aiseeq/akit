package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcript"
	"github.com/aiseeq/akit/transcript/replycache"
)

func compareBriefRuns(t *testing.T, cached, uncached env, args ...string) {
	t.Helper()
	wantCode, wantOut, wantErr := runBriefParts(t, uncached, args...)
	gotCode, gotOut, gotErr := runBriefParts(t, cached, args...)
	if gotCode != wantCode || gotOut != wantOut || gotErr != wantErr {
		t.Fatalf("cached brief differs: code=%d/%d stdoutEqual=%v stderrEqual=%v", gotCode, wantCode, gotOut == wantOut, gotErr == wantErr)
	}
}

func runBriefParts(t *testing.T, e env, args ...string) (int, string, string) {
	t.Helper()
	code, stdout, stderr := runUW(t, e, append([]string{"--brief"}, args...)...)
	return code, stdout, stderr
}

func TestBriefCacheCandidateP1LateType(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	e := testEnv(t)
	e.replyCache = replycache.NewForCode(briefCacheCode)
	uncached := e
	uncached.replyCache = nil
	writeTranscript(t, e.home, "-home-x", "first", human("2026-10-20T10:00:00Z", "first phrase"))
	pad := strings.Repeat("p", 6000)
	lateType := `{"timestamp":"2026-10-31T10:00:00Z","padding":"` + pad + `","type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"second phrase"},"tail":"` + pad + `"}`
	writeTranscript(t, e.home, "-home-y", "late-type", lateType)
	b1 := writeBrief(t, t.TempDir(), "Пользователь 20.10: «first phrase»\n")
	b2 := writeBrief(t, t.TempDir(), "Пользователь 20.10: «first phrase»\nПользователь 31.10: «second phrase»\n")
	args1 := []string{b1, "--until", "2026-11-01 00:00"}
	args2 := []string{b2, "--until", "2026-11-01 00:00"}
	compareBriefRuns(t, e, uncached, args1...)
	compareBriefRuns(t, e, uncached, args2...)
	_, stdout, _ := runBriefParts(t, e, args2...)
	if !strings.Contains(stdout, "second phrase") || !strings.Contains(stdout, "2 found") {
		t.Fatalf("late-type candidate was not scanned by main: found=%v", strings.Contains(stdout, "second phrase"))
	}
}

func TestBriefCacheRepublishesAfterMissP2(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	e := testEnv(t)
	e.replyCache = replycache.NewForCode(briefCacheCode)
	uncached := e
	uncached.replyCache = nil
	brief := writeBrief(t, t.TempDir(), "Пользователь 31.10: «new phrase»\n")
	args := []string{brief, "--until", "2026-10-31 23:59"}
	first := writeTranscript(t, e.home, "-home-x", "first", human("2026-10-30T10:00:00Z", "unrelated"))
	compareBriefRuns(t, e, uncached, args...)
	path := filepath.Join(filepath.Dir(first), "new-file.jsonl")
	if err := os.WriteFile(path, []byte(human("2026-10-31T10:00:00Z", "new phrase")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	compareBriefRuns(t, e, uncached, args...)
	issues := e.replyCache.Issues.Load()
	if _, ok, err := e.replyCache.CachedEvents(path); err != nil || !ok {
		t.Fatalf("new-file index was not published after cache miss: ok=%v err=%v", ok, err)
	}
	compareBriefRuns(t, e, uncached, args...)
	if got := e.replyCache.Issues.Load(); got != issues {
		t.Fatalf("stable cached file raised Issues on next run: before=%d after=%d", issues, got)
	}
}

func TestBriefActivityCandidateKeepsMatchingQuote(t *testing.T) {
	loc := time.UTC
	filters := briefQuoteFilters{from: time.Date(2026, 10, 31, 0, 0, 0, 0, loc), candidateFragments: map[string][]string{
		"2026-10-31": {"needle candidate phrase"},
	}}
	index := replycache.ActivityIndex{Events: []transcript.Event{{
		Time: time.Date(2026, 10, 31, 10, 0, 0, 0, loc), Role: transcript.User, Text: "needle candidate phrase",
	}}}
	if !briefActivityCandidate(index, filters, time.Date(2026, 10, 31, 23, 59, 0, 0, loc), loc) {
		t.Fatal("matching indexed user quote must make its file a candidate")
	}
}

func TestBriefCacheTailHashRebuildsEditedIndexedRange(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	e := testEnv(t)
	e.replyCache = replycache.NewForCode(briefCacheCode)
	uncached := e
	uncached.replyCache = nil
	brief := writeBrief(t, t.TempDir(), "Пользователь 20.10: «marker phrase»\nПользователь 31.10 10:00 UTC: «needle tail phrase»\n")
	writeTranscript(t, e.home, "-home-marker", "marker", human("2026-10-20T10:00:00Z", "marker phrase"))
	path := filepath.Join(e.home, ".claude", "projects", "-home-tail", "tail.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	placeholder := "xxxxxx tail phrase"
	row := `{"timestamp":"2026-10-31T10:00:00Z","type":"user","origin":{"kind":"human"},"padding":"` + strings.Repeat("z", 6000) + `","message":{"role":"user","content":"` + placeholder + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{brief, "--until", "2026-10-31 23:59"}
	compareBriefRuns(t, e, uncached, args...)
	state, ok, err := e.replyCache.LoadActivityIndex(path)
	if err != nil || !ok || state.Index.Uncertain || len(state.Index.Events) != 1 || state.Index.Events[0].Text != placeholder {
		t.Fatalf("initial activity index is not precise: ok=%v err=%v uncertain=%v events=%d", ok, err, state.Index.Uncertain, len(state.Index.Events))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	at := strings.Index(string(data), placeholder)
	if at < 0 || len(data)-at > 4<<10 {
		t.Fatal("edited quote is outside the indexed tail hash")
	}
	copy(data[at:at+len(placeholder)], "needle tail phrase")
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(data, 0); err == nil {
		_, err = f.Seek(0, io.SeekEnd)
	}
	if err == nil {
		_, err = f.WriteString(human("2026-11-03T10:01:00Z", "unrelated suffix") + "\n")
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	compareBriefRuns(t, e, uncached, args...)
	_, stdout, _ := runBriefParts(t, e, args...)
	if !strings.Contains(stdout, "needle tail phrase") {
		t.Fatal("reindexed tail quote was not found")
	}
}

func TestBriefCacheGeneratedParity500(t *testing.T) {
	const seed int64 = 0x16c0ffee
	for i := range 500 {
		t.Run(fmt.Sprintf("seed-%d-case-%03d", seed, i), func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			e := testEnv(t)
			e.replyCache = replycache.NewForCode(briefCacheCode)
			uncached := e
			uncached.replyCache = nil
			phrase := fmt.Sprintf("generated phrase %03d", i)
			day := "2026-10-31"
			until := "2026-10-31 23:59"
			brief := writeBrief(t, t.TempDir(), "Пользователь 31.10 10:00 UTC: «"+phrase+"»\n")
			path := filepath.Join(e.home, ".claude", "projects", "-home-generated", fmt.Sprintf("%03d.jsonl", i))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			content := generatedTranscript(t, seed, i, day, phrase)
			if i%10 == 8 {
				placeholder := strings.Repeat("x", len(phrase))
				content = []byte(`{"timestamp":"2026-10-31T10:00:00Z","padding":"` + strings.Repeat("z", 6000) + `","type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"` + placeholder + `"}}` + "\n")
			}
			if err := os.WriteFile(path, content, 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{brief, "--until", until}
			compareBriefRuns(t, e, uncached, args...)
			compareBriefRuns(t, e, uncached, args...)
			if i%10 == 6 {
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString(human("2026-10-31T10:01:00Z", phrase+" appended") + "\n")
				if closeErr := f.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					t.Fatal(err)
				}
				compareBriefRuns(t, e, uncached, args...)
				compareBriefRuns(t, e, uncached, args...)
			}
			if i%10 == 7 {
				replacement := path + ".replacement"
				if err := os.WriteFile(replacement, []byte(human("2026-10-31T10:01:00Z", phrase)+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
				compareBriefRuns(t, e, uncached, args...)
			}
			if i%10 == 8 {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				placeholder := strings.Repeat("x", len(phrase))
				at := strings.Index(string(data), placeholder)
				if at < 0 || len(data)-at > 4<<10 {
					t.Fatal("generated text is not inside indexed tail hash")
				}
				copy(data[at:at+len(phrase)], phrase)
				f, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteAt(data, 0); err == nil {
					_, err = f.Seek(0, io.SeekEnd)
				}
				if err == nil {
					_, err = f.WriteString(human("2026-11-03T10:02:00Z", "unrelated appended suffix") + "\n")
				}
				if closeErr := f.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					t.Fatal(err)
				}
				compareBriefRuns(t, e, uncached, args...)
			}
		})
	}
}

func generatedTranscript(t *testing.T, seed int64, n int, day, phrase string) []byte {
	t.Helper()
	state := uint64(seed) + uint64(n)*0x9e3779b97f4a7c15
	next := func() uint64 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		return state
	}
	ts := day + "T10:00:00Z"
	row := human(ts, phrase)
	switch n % 10 {
	case 0: // Property input includes randomized key order.
		row = fmt.Sprintf(`{"message":{"content":%q,"role":"user"},"origin":{"source":"cli","kind":"human"},"timestamp":%q,"type":"user"}`, phrase, ts)
	case 1: // type beyond the former 512-byte prefix.
		row = `{"timestamp":"` + ts + `","padding":"` + strings.Repeat("p", 600+int(next()%3000)) + `","type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"` + phrase + `"}}`
	case 2: // Truncated line after a valid record.
		return []byte(row + "\n" + `{"type":"user","timestamp":"` + ts + `","message":{"role":"user","content":"` + phrase)
	case 3: // Broken line with a timestamp is conservatively indexed as bad.
		row = `{"type":"user","timestamp":"` + ts + `","origin":{"kind":"human"},"message":{"content":"` + phrase + `"`
	case 4: // Offset timestamp near the UTC date boundary.
		ts = "2026-10-31T23:30:00-02:00"
		row = human(ts, phrase)
	case 5: // Record immediately after the minute --until boundary.
		ts = "2026-10-31T10:00:00.000000001Z"
		row = human(ts, phrase)
	case 9: // Parser state and Ask events are included in generated inputs.
		return []byte(ask("2026-10-31T09:59:00Z", "call-generated") + "\n" + answer("2026-10-31T10:00:00Z", "call-generated", "Which phrase?", phrase) + "\n")
	}
	return []byte(row + "\n")
}
