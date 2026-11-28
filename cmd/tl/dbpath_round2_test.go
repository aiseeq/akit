package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcript"
)

func TestDBContextGrepSeesTextPastListingCut(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	long := strings.Repeat("prefix ", textCut(80)+10) + "needle after the listing cut"
	p := writeReviewClaude(t, e, "-home-x-proj", "7a7a7a7a-0000-4000-8000-000000000008",
		userRecord("2026-10-25T10:20:00.000Z", long),
		agentRecord("2026-10-25T10:20:01.000Z", "context row"))
	wholeSecond := time.Now().Truncate(time.Second)
	if err := os.Chtimes(p, wholeSecond, wholeSecond); err != nil {
		t.Fatal(err)
	}
	ch := newFakeCH(t, append(paths, p), unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	probeCompare(t, e, "context grep past width", "7a7a7a7a", "--grep", "needle", "-C", "1", "--max", "0")
}

func TestDBContextLoadsNeighboursOnlyForMatchingFiles(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	matched := writeReviewClaude(t, e, "-home-x-proj", "7c7c7c7c-0000-4000-8000-000000000008",
		userRecord("2026-10-25T10:20:00.000Z", "needle in this file"),
		agentRecord("2026-10-25T10:20:01.000Z", "matching neighbour"))
	unmatched := writeReviewClaude(t, e, "-home-x-proj", "7d7d7d7d-0000-4000-8000-000000000008",
		userRecord("2026-10-25T10:20:00.000Z", "unrelated transcript"),
		agentRecord("2026-10-25T10:20:01.000Z", "unrelated neighbour"))
	wholeSecond := time.Now().Truncate(time.Second)
	for _, p := range append(append([]string(nil), paths...), matched, unmatched) {
		if err := os.Chtimes(p, wholeSecond, wholeSecond); err != nil {
			t.Fatal(err)
		}
	}
	// Keep the file backend's --all scan limited to the two rows represented by this fake database.
	for _, p := range paths {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	ch := newFakeCH(t, []string{matched, unmatched}, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	probeCompare(t, e, "context neighbors only in matching files", "--all", "--grep", "needle", "-C", "1", "--max", "0")

	ch.mu.Lock()
	defer ch.mu.Unlock()
	if len(ch.contextFiles) != 1 {
		t.Fatalf("context event queries = %v, want one query", ch.contextFiles)
	}
	var matchedID string
	for _, f := range ch.files {
		if f.path == matched {
			matchedID = strconv.FormatUint(f.id, 10)
		}
	}
	if len(ch.contextFiles[0]) != 1 || ch.contextFiles[0][0] != matchedID {
		t.Fatalf("context query files = %v, want only matching file id %s (unmatched %s)", ch.contextFiles[0], matchedID, unmatched)
	}
}

func TestDBAllReadsFromDatabaseWhenTranscriptsAreUnavailable(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	e.backend = backendFiles
	codeFiles, outFiles, _ := runTL(t, e, "--all", "--grep", "needle", "-C", "1", "--max", "0")
	for _, path := range paths {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	e.backend = backendDB
	codeDB, outDB, errDB := runTL(t, e, "--all", "--grep", "needle", "-C", "1", "--max", "0")
	if codeFiles != codeDB || outFiles != outDB || strings.Contains(errDB, "reading files instead") {
		t.Fatalf("database answer depends on removed transcripts: files=%d %q; db=%d %q; stderr=%s", codeFiles, outFiles, codeDB, outDB, errDB)
	}
}

func TestDBSubagentsSkipsRemovedStoredPath(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	var subPath string
	for _, path := range paths {
		if strings.Contains(path, string(filepath.Separator)+"subagents"+string(filepath.Separator)+"agent-") {
			subPath = path
			break
		}
	}
	if subPath == "" {
		t.Fatal("synthetic corpus has no subagent")
	}
	secondSub := filepath.Join(filepath.Dir(filepath.Dir(subPath)), "subagents", "agent-00000000.jsonl")
	writeWorkFile(t, secondSub, []string{userRecord("2026-10-25T10:10:31Z", "second subagent")})
	paths = append(paths, secondSub)
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	if err := os.Remove(subPath); err != nil {
		t.Fatal(err)
	}
	probeCompare(t, e, "subagents skips removed stored path", dbOtherSession[:8], "--subagents", "--max", "50")
}

func TestDBSubagentsSortedByPath(t *testing.T) {
	e, paths, _ := dbCorpus(t)
	var mainPath string
	var subPath string
	for _, path := range paths {
		if strings.Contains(path, string(filepath.Separator)+"subagents"+string(filepath.Separator)+"agent-") {
			subPath = path
		} else if strings.HasPrefix(filepath.Base(path), dbOtherSession) {
			mainPath = path
		}
	}
	if mainPath == "" || subPath == "" {
		t.Fatal("synthetic corpus has no parent or subagent")
	}
	secondSub := filepath.Join(filepath.Dir(filepath.Dir(subPath)), "subagents", "agent-00000000.jsonl")
	writeWorkFile(t, secondSub, []string{userRecord("2026-10-25T10:10:31Z", "second subagent")})
	parent, err := transcript.SourceAt(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	d := &dbPath{opts: options{args: []string{mainPath}, subagents: true}, e: e, files: map[string]*dbFile{
		mainPath:  {path: mainPath, session: parent.ID},
		subPath:   {path: subPath, kind: "sub", parent: parent.ID},
		secondSub: {path: secondSub, kind: "sub", parent: parent.ID},
	}}
	got, err := d.resolveSessions()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{mainPath, secondSub, subPath}
	if len(got) != len(want) {
		t.Fatalf("got paths %v, want %v", got, want)
	}
	for i := range want {
		if got[i].Path != want[i] {
			t.Fatalf("subagents are not in path order: got %v, want %v", got, want)
		}
	}
}
