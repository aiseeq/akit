package main

import (
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAllAndSessionRejectedBeforeDatabase(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	for _, backend := range []string{backendFiles, backendDB} {
		e.backend = backend
		code, _, stderr := runTL(t, e, "--all", dbOtherSession[:8])
		if code != exitUsage || stderr != "tl: --all and SESSION together; pick one\n" {
			t.Errorf("%s conflict: exit=%d stderr=%q", backend, code, stderr)
		}
	}
	if got := ch.requests(); len(got) != 0 {
		t.Fatalf("invalid arguments queried database: %v", got)
	}
}

func TestAllAndRefUsesFileRefBehaviorOnBothBackends(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	ref := dbOtherSession[:8] + ":99999"
	var wantCode int
	var wantOut, wantErr string
	for i, backend := range []string{backendFiles, backendDB} {
		e.backend = backend
		code, out, stderr := runTL(t, e, "--all", ref)
		if i == 0 {
			wantCode, wantOut, wantErr = code, out, stderr
			if code == exitUsage || strings.Contains(stderr, "--all and SESSION together") {
				t.Fatalf("REF was rejected as a session: code=%d stderr=%q", code, stderr)
			}
			continue
		}
		if code != wantCode || out != wantOut || stderr != wantErr {
			t.Fatalf("--all REF differs by backend: files=(%d,%q,%q), db=(%d,%q,%q)", wantCode, wantOut, wantErr, code, out, stderr)
		}
	}
	if got := ch.requests(); len(got) != 0 {
		t.Fatalf("REF path queried database: %v", got)
	}
}

func TestDBSubagentsOmitsDiskOnlyFileWithoutFallback(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	diskOnly := filepath.Join(e.home, ".claude", "projects", "-home-x-proj", dbOtherSession, "subagents", "agent-deadbeef.jsonl")
	writeWorkFile(t, diskOnly, []string{userRecord("2026-10-25T10:10:50.000Z", "disk-only-subagent-marker")})
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	e.backend = backendDB
	codeDB, outDB, errDB := runTL(t, e, dbOtherSession[:8], "--subagents", "--max", "0")
	e.backend = backendFiles
	codeFiles, outFiles, _ := runTL(t, e, dbOtherSession[:8], "--subagents", "--max", "0")
	if codeDB != codeFiles || codeDB != 0 || !strings.Contains(outFiles, "disk-only-subagent-marker") || strings.Contains(outDB, "disk-only-subagent-marker") {
		t.Fatalf("disk-only subagent: db=(%d,%q,%q), files=(%d,%q)", codeDB, outDB, errDB, codeFiles, outFiles)
	}
	if strings.Contains(errDB, "reading files instead") || !slices.Contains(ch.requests(), "files") {
		t.Fatalf("database answer fell back or did not query files: requests=%v stderr=%q", ch.requests(), errDB)
	}
}

func TestDBContextDoesNotReadUnselectedLongText(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	e.backend = backendDB
	code, out, stderr := runTL(t, e, "--all", "--grep", "needle", "-C", "1", "--max", "0")
	if code != 0 || out == "" || strings.Contains(stderr, "reading files instead") {
		t.Fatalf("context query: exit=%d stderr=%q output=%q", code, stderr, out)
	}
	if slices.Contains(ch.requests(), "texts") {
		t.Fatalf("context query fetched full text for rows outside the selected windows: %v", ch.requests())
	}
}

func TestReviewHugeContext(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	probeCompare(t, e, "huge context", dbOtherSession[:8], "--grep", "needle alpha", "-C", "9223372036854775807", "--max", "0")
}
