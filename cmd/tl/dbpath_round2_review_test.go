package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func probeServe(t *testing.T, paths []string, unloaded map[string]int) {
	t.Helper()
	ch := newFakeCH(t, paths, unloaded)
	for _, f := range ch.files {
		if f.loaded.Year() < 2000 { // fake: no loaded line gives no load time
			f.loaded = time.Now()
		}
	}
	srv := httptest.NewServer(ch)
	t.Cleanup(srv.Close)
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
}

func rewriteInPlace(t *testing.T, path string, data []byte) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("inode changed")
	}
}

// probe11 again: same inode, same byte length, text changed in the head of the file.
func TestProbeRewrittenSameInode(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	p := writeReviewClaude(t, e, "-home-x-proj", "3a3a3a3a-0000-4000-8000-000000000008",
		userRecord("2026-10-25T10:20:00.000Z", "старый текст sub один"),
		userRecord("2026-10-25T10:20:01.000Z", "старый текст sub два"))
	probeServe(t, append(paths, p), unloaded)
	data, _ := os.ReadFile(p)
	rewriteInPlace(t, p, []byte(strings.ReplaceAll(string(data), "старый", "другой")))
	probeCompare(t, e, "same inode same size", "3a3a3a3a", "--role", "all")
	probeCompare(t, e, "same inode same size --all", "--all", "--grep", "другой|старый", "--max", "0")
	rewriteInPlace(t, p, []byte(userRecord("2026-10-25T10:20:00.000Z", "совсем другой и более длинный текст sub, длиннее прежнего файла "+strings.Repeat("ж", 200))+"\n"))
	probeCompare(t, e, "same inode longer", "3a3a3a3a", "--role", "all")
	probeCompare(t, e, "same inode longer --all", "--all", "--grep", "другой|старый", "--max", "0")
}

// A change far before the last 256 loaded bytes, same size: neither the loader nor tl sees it.
func TestProbeRewriteBeforeTail(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	p := writeReviewClaude(t, e, "-home-x-proj", "3b3b3b3b-0000-4000-8000-000000000008",
		userRecord("2026-10-25T10:20:00.000Z", "старый первый"),
		userRecord("2026-10-25T10:20:01.000Z", "длинный второй "+strings.Repeat("щ", 300)))
	probeServe(t, append(paths, p), unloaded)
	data, _ := os.ReadFile(p)
	rewriteInPlace(t, p, []byte(strings.Replace(string(data), "старый", "другой", 1)))
	e.backend = backendFiles
	codeFiles, outFiles, errFiles := runTL(t, e, "3b3b3b3b", "--role", "all")
	e.backend = backendDB
	codeDB, outDB, errDB := runTL(t, e, "3b3b3b3b", "--role", "all")
	if codeFiles != 0 || codeDB != 0 || errFiles != errDB || !strings.Contains(outFiles, "другой первый") ||
		!strings.Contains(outDB, "старый первый") || strings.Contains(outDB, "другой первый") {
		t.Fatalf("tail-only rewrite: files (%d) %q %q; db (%d) %q %q", codeFiles, outFiles, errFiles, codeDB, outDB, errDB)
	}
}

// A file shorter than 256 bytes rewritten with the same size.
func TestProbeShortFileRewrite(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	p := writeReviewClaude(t, e, "-home-x-proj", "3c3c3c3c-0000-4000-8000-000000000008",
		userRecord("2026-10-25T10:20:00.000Z", "aa"))
	data, _ := os.ReadFile(p)
	if len(data) >= 256 {
		t.Fatalf("fixture %d bytes", len(data))
	}
	probeServe(t, append(paths, p), unloaded)
	rewriteInPlace(t, p, []byte(strings.Replace(string(data), `"aa"`, `"bb"`, 1)))
	probeCompare(t, e, "short rewrite", "3c3c3c3c", "--role", "all")
	probeCompare(t, e, "short rewrite --all", "--all", "--grep", "bb|aa", "--max", "0")
}

// Nothing loaded yet (ingested 0) with lines on disk, and an empty file in the store.
func TestProbeIngestedZero(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	p := writeReviewClaude(t, e, "-home-x-proj", "3d3d3d3d-0000-4000-8000-000000000008",
		userRecord("2026-10-25T10:20:00.000Z", "ничего не загружено sub"),
		userRecord("2026-10-25T10:20:01.000Z", "вторая sub"))
	empty := filepath.Join(e.home, ".claude", "projects", "-home-x-proj", "3e3e3e3e-0000-4000-8000-000000000008.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	unloaded[p] = 2
	probeServe(t, append(paths, p, empty), unloaded)
	if err := os.WriteFile(empty, []byte(userRecord("2026-10-25T10:20:02.000Z", "пустой вырос sub")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	probeCompare(t, e, "ingested 0", "3d3d3d3d", "--role", "all")
	probeCompare(t, e, "empty grew", "3e3e3e3e", "--role", "all")
	probeCompare(t, e, "ingested 0 --all", "--all", "--grep", "sub", "--max", "0")
}

// The file grows after the store row: the appended lines come from the disk.
func TestProbeGrowsAfterLoad(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	p := writeReviewClaude(t, e, "-home-x-proj", "3f3f3f3f-0000-4000-8000-000000000008",
		userRecord("2026-10-25T10:20:00.000Z", "до роста sub"))
	probeServe(t, append(paths, p), unloaded)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(userRecord("2026-10-25T10:20:05.000Z", "после роста sub") + "\n" + `{"type":"user","timest`)
	_ = f.Close()
	probeCompare(t, e, "grown", "3f3f3f3f", "--role", "all")
	probeCompare(t, e, "grown --all", "--all", "--grep", "роста", "--max", "0")
}

// A codex rollout with a line that is not JSON and message events: the stored bad lines are parsed again.
func TestProbeCodexBadLineWithMessageEvents(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	rollout := filepath.Join(e.home, ".local", "state", "worker", "w1", "codex", "sessions", "2026", "10", "25",
		"rollout-2026-10-25T11-00-00-0199bbbb-0000-7000-8000-000000000002.jsonl")
	writeWorkFile(t, rollout, []string{
		`{"timestamp":"2026-10-25T11:00:00.000Z","type":"session_meta","payload":{"id":"0199bbbb","cwd":"/w"}}`,
		`{"timestamp":"2026-10-25T11:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"бриф sub"}]}}`,
		`not json at all`,
		`{"timestamp":"2026-10-25T11:00:02.000Z","type":"event_msg","payload":{"type":"user_message","message":"привет sub"}}`,
		`{"timestamp":"2026-10-25T11:00:03.000Z","type":"event_msg","payload":{"type":"agent_message","message":"ответ sub"}}`,
	})
	probeServe(t, append(paths, rollout), unloaded)
	probeCompare(t, e, "codex bad + message events", "--all", "--codex", "--role", "all", "--max", "0")
	probeCompare(t, e, "codex bad + message events grep", "--all", "--codex", "--grep", "sub", "--max", "0")
}

// Raw line accounting includes a bad line after the last stored event line.
func TestProbeBadRawLinePastLastEvent(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	p := writeReviewClaude(t, e, "-home-x-proj", "4b4b4b4b-0000-4000-8000-000000000008",
		userRecord("2026-10-25T10:20:00.000Z", "stored event sub"),
		`not json needle`)
	ch := newFakeCH(t, append(paths, p), unloaded)
	for _, f := range ch.files {
		if f.path == p {
			f.evLines = 1
		}
	}
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	probeCompare(t, e, "bad raw line after last event", "--all", "--grep", "needle", "--max", "0")
}

// A Claude transcript with a bad line among records, no grep and grep.
func TestProbeClaudeBadLines(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	p := writeReviewClaude(t, e, "-home-x-proj", "4a4a4a4a-0000-4000-8000-000000000008",
		userRecord("2026-10-25T10:20:00.000Z", "первая sub"),
		`{"broken": `,
		`{"type":"user","timestamp":"bad","message":{"role":"user","content":"плохое время sub"}}`,
		userRecord("2026-10-25T10:20:02.000Z", "третья"))
	probeServe(t, append(paths, p), unloaded)
	probeCompare(t, e, "bad no grep", "--all", "--role", "all", "--max", "0")
	probeCompare(t, e, "bad grep sub", "--all", "--grep", "sub", "--max", "0")
	probeCompare(t, e, "bad grep broken", "--all", "--grep", "broken", "--max", "0")
	probeCompare(t, e, "bad session", "4a4a4a4a", "--role", "all")
}

// The home folder has a part named subagents: --no-subagents must keep the main transcripts.
func TestProbeHomeNamedSubagents(t *testing.T) {
	e := fakeHome(t)
	e.home = filepath.Join(e.home, "subagents", "h")
	p := writeReviewClaude(t, e, "-home-x-proj", "5a5a5a5a-0000-4000-8000-000000000008",
		userRecord("2026-10-25T10:20:00.000Z", "главная сессия sub"))
	ch := newFakeCH(t, []string{p}, map[string]int{})
	for _, f := range ch.files {
		f.kind = "claude"
	}
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	probeCompare(t, e, "home/subagents --no-subagents", "--all", "--no-subagents", "--grep", "sub", "--max", "0")
}

// The file cannot be opened although stat works: files path reports it, db path must not fail the whole call.
func TestProbeUnreadableStored(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads anything")
	}
	e, paths, unloaded := dbCorpus(t)
	p := writeReviewClaude(t, e, "-home-x-proj", "6a6a6a6a-0000-4000-8000-000000000008",
		userRecord("2026-10-25T10:20:00.000Z", "закрытый sub"))
	probeServe(t, append(paths, p), unloaded)
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	probeCompare(t, e, "unreadable --all", "--all", "--grep", "sub", "--max", "0")
}
