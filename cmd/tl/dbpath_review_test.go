package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func probeCompare(t *testing.T, e env, label string, args ...string) {
	t.Helper()
	e.backend = backendFiles
	codeF, outF, errF := runTL(t, e, args...)
	e.backend = backendDB
	codeD, outD, errD := runTL(t, e, args...)
	if codeF != codeD || outF != outD || errF != errD {
		t.Errorf("%s %q: files exit %d, db exit %d\nfiles out:\n%s\nfiles err: %s\ndb out:\n%s\ndb err: %s", label, args, codeF, codeD, outF, errF, outD, errD)
	}
}

func TestProbeBadRecordUnderGrep(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	bad := writeReviewClaude(t, e, "-home-x-proj", "7e7e7e7e-0000-4000-8000-000000000003",
		userRecord("2026-10-25T10:20:00.000Z", "обычная строка needle"),
		`{"type":"user","timestamp":"not-a-time","message":{"role":"user","content":"без иглы"}}`,
		userRecord("2026-10-25T10:20:02.000Z", "ещё строка"))
	paths = append(paths, bad)
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	probeCompare(t, e, "bad record, needle elsewhere", "--all", "--grep", "needle", "--max", "0")
	probeCompare(t, e, "bad record, no grep", "--all", "--max", "0")
}

func TestProbeDeletedFile(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	gone := writeReviewClaude(t, e, "-home-x-proj", "9d9d9d9d-0000-4000-8000-000000000004",
		userRecord("2026-10-25T10:30:00.000Z", "удалённая сессия sub"))
	paths = append(paths, gone)
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	probeCompare(t, e, "deleted session", "9d9d9d9d", "--role", "all")
	probeCompare(t, e, "deleted ref", "9d9d9d9d:1")
	probeCompare(t, e, "deleted in --all", "--all", "--grep", "sub", "--max", "0")
}

func TestProbeNewFileNotLoaded(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	fresh := writeReviewClaude(t, e, "-home-x-proj", "8c8c8c8c-0000-4000-8000-000000000005",
		userRecord("2026-10-25T10:40:00.000Z", "свежая сессия sub"))
	_ = filepath.Base(fresh)
	probeCompare(t, e, "new file in --all", "--all", "--grep", "sub", "--max", "0")
	probeCompare(t, e, "new file by id", "8c8c8c8c", "--role", "all")
	_ = strings.TrimSpace
}

func TestProbeTextEndsNewlineDollar(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	nl := writeReviewClaude(t, e, "-home-x-proj", "6b6b6b6b-0000-4000-8000-000000000006",
		userRecord("2026-10-25T10:50:00.000Z", "Error\nX tail"))
	paths = append(paths, nl)
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	probeCompare(t, e, "dot over newline, case-sensitive", "--all", "--grep", "(?-i)Error.X", "--max", "0")
}
