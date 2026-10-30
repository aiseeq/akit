package main

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/timearg"
	"github.com/aiseeq/akit/transcript"
)

const sessionID = "1a2b3c4d-0000-4000-8000-000000000001"

// fakeHome — домашняя папка с одним транскриптом Claude Code из фикстуры пакета transcript.
func fakeHome(t *testing.T) env {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", "-home-x-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../transcript/testdata/claude.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	return env{home: home, now: now}
}

func runTL(t *testing.T, e env, args ...string) (int, string, string) {
	t.Helper()
	opts, err := parseArgs(args, e.now)
	if err != nil {
		t.Fatalf("parseArgs %q: %v", args, err)
	}
	var out, errOut bytes.Buffer
	code := run(opts, e, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestParseArgsErrors(t *testing.T) {
	now := time.Now()
	for _, args := range [][]string{
		{"--role", "boss"}, {"-C", "2"}, {"--codex"}, {"--max", "-1"}, {"--since"}, {"--frobnicate", "x"},
	} {
		if _, err := parseArgs(args, now); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
}

func TestRoleAliases(t *testing.T) {
	opts, err := parseArgs([]string{"--role", "assistant, tool,user"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := map[transcript.Role]bool{transcript.Agent: true, transcript.Cmd: true, transcript.User: true}
	if !maps.Equal(opts.roles, want) {
		t.Errorf("roles %v, want %v", opts.roles, want)
	}
}

func TestConversationByID(t *testing.T) {
	e := fakeHome(t)
	code, out, errOut := runTL(t, e, "1a2b3c4d")
	if code != 0 {
		t.Fatalf("code %d, err %q", code, errOut)
	}
	for _, want := range []string{
		"== 1a2b3c4d main ~/.claude/projects/-home-x-proj/" + sessionID + ".jsonl",
		"1a2b3c4d:3 user привет, сделай sub",
		"1a2b3c4d:7 ask sub (Recommended) ← Какое имя?",
		"1a2b3c4d:8 peer reviewer: имя — sub",
		"1a2b3c4d:4 agent Делаю.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, " cmd ") || strings.Contains(out, " out ") {
		t.Errorf("tool events in the default roles:\n%s", out)
	}
}

func TestGrepWithContextAndMax(t *testing.T) {
	e := fakeHome(t)
	_, out, _ := runTL(t, e, "1a2b3c4d", "--role", "all", "--grep", "имя", "-C", "1")
	// находки — ask (7) и peer (8), окрестность — cmd (6) и queued user (9)
	for _, want := range []string{":6 cmd", ":7 ask", ":8 peer", ":9 user"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, ":5 out") || strings.Contains(out, ":10 note") {
		t.Errorf("context wider than -C 1:\n%s", out)
	}
	_, out, _ = runTL(t, e, "1a2b3c4d", "--max", "2")
	if !strings.Contains(out, "earlier not shown") || strings.Count(out, "1a2b3c4d:") != 2 {
		t.Errorf("--max 2:\n%s", out)
	}
}

func TestAllSinceAndNone(t *testing.T) {
	e := fakeHome(t)
	code, out, _ := runTL(t, e, "--all", "--role", "user", "--grep", "СДЕЛАЙ SUB")
	if code != 0 || !strings.Contains(out, "1a2b3c4d:3 user") {
		t.Fatalf("code %d:\n%s", code, out)
	}
	code, out, _ = runTL(t, e, "--all", "--grep", "нет такого текста")
	if code != exitNone || !strings.Contains(out, "no events in 1 transcript(s)") {
		t.Errorf("code %d:\n%s", code, out)
	}
	code, out, _ = runTL(t, e, "1a2b3c4d", "--since", "2026-10-25T10:02:30Z")
	if code != 0 || strings.Contains(out, ":8 peer") || !strings.Contains(out, ":9 user") {
		t.Errorf("--since: code %d:\n%s", code, out)
	}
}

func TestSubagentsAndRawFilter(t *testing.T) {
	e := fakeHome(t)
	parentPath := filepath.Join(e.home, ".claude", "projects", "-home-x-proj", sessionID+".jsonl")
	subDir := filepath.Join(strings.TrimSuffix(parentPath, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const agentID = "abcdef0123456789"
	subPath := filepath.Join(subDir, "agent-"+agentID+".jsonl")
	row := `{"type":"user","isSidechain":true,"agentId":"abcdef0123456789","sessionId":"` + sessionID + `","timestamp":"2026-10-25T11:59:00Z","message":{"role":"user","content":"sub marker"}}` + "\n"
	row += `{"type":"user","agentId":"abcdef0123456789","sessionId":"` + sessionID + `","timestamp":"2026-10-25T11:59:01Z","message":{"role":"user","content":"sub marker ordinary"}}` + "\n"
	if err := os.WriteFile(subPath, []byte(row), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "agent-"+agentID+".meta.json"), []byte(`{"description":"Synthetic work"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runTL(t, e, "--all", "--role", "all", "--since", "1h")
	if code != 0 || !strings.Contains(out, "== abcdef01 sub of 1a2b3c4d main \"Synthetic work\"") || !strings.Contains(out, "abcdef01:1 ") {
		t.Fatalf("--all: code %d:\n%s", code, out)
	}
	code, out, _ = runTL(t, e, "--all", "--no-subagents", "--role", "all", "--since", "1h")
	if strings.Contains(out, "sub marker") {
		t.Fatalf("--no-subagents: code %d:\n%s", code, out)
	}
	for _, args := range [][]string{{"abcdef01:1"}, {"abcdef01", "--role", "all"}, {subPath, "--role", "all"}, {"1a2b3c4d", "--subagents", "--role", "all"}} {
		code, out, _ = runTL(t, e, args...)
		if code != 0 || !strings.Contains(out, "sub marker") {
			t.Errorf("%q: code %d:\n%s", args, code, out)
		}
	}
	// Суб-агент по префиксу id и по пути — тот же заголовок и короткие ссылки, что в --all.
	for _, arg := range []string{"abcdef01", subPath} {
		code, out, _ = runTL(t, e, arg, "--role", "all")
		if code != 0 || !strings.Contains(out, "== abcdef01 sub of 1a2b3c4d") || !strings.Contains(out, "abcdef01:1 ") {
			t.Errorf("%s: code %d:\n%s", arg, code, out)
		}
	}
	code, out, _ = runTL(t, e, "--all", "--role", "all", "--since", "1h", "--raw", `"isSidechain":true`, "--grep", "sub marker")
	if code != 0 || !strings.Contains(out, "abcdef01:1 ") || strings.Contains(out, "abcdef01:2 ") {
		t.Errorf("--raw with --grep: code %d:\n%s", code, out)
	}
}

func TestRef(t *testing.T) {
	e := fakeHome(t)
	code, out, _ := runTL(t, e, "1a2b3c4d:12")
	if code != 0 || !strings.Contains(out, "1a2b3c4d:12 user\nсмотри скрин\n[image]") {
		t.Errorf("code %d:\n%s", code, out)
	}
	if code, _, _ := runTL(t, e, "1a2b3c4d:2"); code != exitNone {
		t.Errorf("service record: code %d", code)
	}
}

func TestResolveErrors(t *testing.T) {
	e := fakeHome(t)
	for _, args := range [][]string{{}, {"abc"}, {"deadbeef"}, {"--all", "1a2b3c4d"}} {
		if code, _, _ := runTL(t, e, args...); code != exitUsage {
			t.Errorf("%q: code %d", args, code)
		}
	}
}

func TestProblemsInSummary(t *testing.T) {
	e := fakeHome(t)
	path := filepath.Join(e.home, ".claude", "projects", "-home-x-proj", sessionID+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"user","timestamp":"2026-10-25T1`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// строка 13 фикстуры испорчена нарочно, вторая — дописанная сейчас
	code, out, _ := runTL(t, e, "1a2b3c4d")
	if code != 0 || !strings.Contains(out, "; 2 unparsable line(s) skipped") {
		t.Errorf("code %d:\n%s", code, out)
	}
	if _, err := timearg.Parse("1000000m", e.now); err == nil {
		t.Error("7-digit ago accepted")
	}
	if ref, _, err := splitRef([]string{"1a2b3c4d:0"}); ref != "" || err != nil {
		t.Errorf("line 0 taken as a ref: %q %v", ref, err)
	}
}
