package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeWaitTranscript(t *testing.T, e env, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func claudeWaitRows(specs []struct {
	command string
	delay   int
	bg      bool
	output  bool
}) string {
	var rows []string
	base := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	sec := 0
	for i, s := range specs {
		id := fmt.Sprintf("call%d", i)
		input := fmt.Sprintf(`{"command":%q,"run_in_background":%t}`, s.command, s.bg)
		rows = append(rows, fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":%s}]}}`, base.Add(time.Duration(sec)*time.Second).Format(time.RFC3339), id, input))
		sec += s.delay
		if s.output {
			result := fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]}}`, base.Add(time.Duration(sec)*time.Second).Format(time.RFC3339), id)
			rows = append(rows, result)
		}
	}
	return strings.Join(rows, "\n") + "\n"
}

func TestWaitClaudePercentilesAndFiltering(t *testing.T) {
	e := fakeHome(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	specs := make([]struct {
		command string
		delay   int
		bg      bool
		output  bool
	}, 0, 25)
	for i := 1; i <= 20; i++ {
		specs = append(specs, struct {
			command string
			delay   int
			bg      bool
			output  bool
		}{command: "tk done task | head", delay: i, output: true})
	}
	specs = append(specs,
		struct {
			command string
			delay   int
			bg      bool
			output  bool
		}{command: "tk done task && other", delay: 1, output: true},
		struct {
			command string
			delay   int
			bg      bool
			output  bool
		}{command: "tk done task", delay: 1, bg: true, output: true},
		struct {
			command string
			delay   int
			bg      bool
			output  bool
		}{command: "tk done task", delay: 1, output: false},
	)
	path := writeWaitTranscript(t, e, "wait-claude.jsonl", claudeWaitRows(specs))
	code, out, errOut := runTL(t, e, "--wait", "tk done", "--q", "50", path)
	if code != 0 || out != "10000\n" {
		t.Errorf("q50 code=%d out=%q stderr=%q", code, out, errOut)
	}
	code, out, errOut = runTL(t, e, "--wait=tk done", "--q=95", path)
	if code != 0 || out != "19000\n" {
		t.Errorf("q95 code=%d out=%q stderr=%q", code, out, errOut)
	}
	code, out, errOut = runTL(t, e, "--wait", "tk note", "--q", "95", path)
	if code != 3 || out != "" || !strings.Contains(errOut, "0 wait event") {
		t.Errorf("SUB code=%d out=%q stderr=%q", code, out, errOut)
	}
}

func TestWaitCacheColdWarmChangedAndBinaryFingerprint(t *testing.T) {
	e := fakeHome(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	spec := []struct {
		command string
		delay   int
		bg      bool
		output  bool
	}{{"tk done one", 1, false, true}, {"tk done two", 2, false, true}, {"tk done three", 3, false, true}}
	path := writeWaitTranscript(t, e, "cache.jsonl", claudeWaitRows(spec))
	args := []string{"--wait", "tk done", "--q", "50", path}
	code, cold, coldErr := runTL(t, e, args...)
	if code != 0 || cold != "2000\n" || coldErr != "" {
		t.Fatalf("cold = (%d, %q, %q)", code, cold, coldErr)
	}
	code, warm, warmErr := runTL(t, e, args...)
	if code != 0 || warm != cold || warmErr != "" {
		t.Fatalf("warm = (%d, %q, %q), cold %q", code, warm, warmErr, cold)
	}
	cache := newWaitCache(io.Discard)
	if !cache.valid(path) {
		t.Fatal("cold call did not save a usable entry")
	}
	otherBinary := &waitCache{dir: cache.dir, binary: "different-binary"}
	if otherBinary.valid(path) {
		t.Fatal("entry from another binary fingerprint was accepted")
	}
	withMore := append(spec, struct {
		command string
		delay   int
		bg      bool
		output  bool
	}{"tk done four", 4, false, true}, struct {
		command string
		delay   int
		bg      bool
		output  bool
	}{"tk done five", 5, false, true})
	if err := os.WriteFile(path, []byte(claudeWaitRows(withMore)), 0o644); err != nil {
		t.Fatal(err)
	}
	// Дописанные вызовы сдвигают p50 с 2 до 3 с: устаревшая запись кэша дала бы прежний ответ.
	code, changed, changedErr := runTL(t, e, args...)
	if code != 0 || changed != "3000\n" || changedErr != "" || !cache.valid(path) {
		t.Fatalf("changed = (%d, %q, %q)", code, changed, changedErr)
	}
}

func TestWaitCacheCorruptionAndWindowSemantics(t *testing.T) {
	e := fakeHome(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	specs := []struct {
		command string
		delay   int
		bg      bool
		output  bool
	}{
		{"tk done one", 1, false, true}, {"busy two", 2, false, true},
		{"tk done three", 3, false, true}, {"tk done four", 4, false, true},
	}
	path := writeWaitTranscript(t, e, "window.jsonl", claudeWaitRows(specs))
	queries := [][]string{
		{"--wait", "tk done", "--since", "10:00:02", "--until", "10:00:08", "--q", "50", path},
		{"--wait", "busy", "--since", "10:00:02", "--until", "10:00:08", "--q", "50", path},
		{"--wait", "tk done", "--since", "10:00:00", "--until", "10:00:01", "--q", "50", path},
	}
	for _, args := range queries {
		codeCold, outCold, errCold := runTL(t, e, args...)
		codeWarm, outWarm, errWarm := runTL(t, e, args...)
		if codeCold != codeWarm || outCold != outWarm || errCold != errWarm {
			t.Errorf("query %q cold=(%d,%q,%q) warm=(%d,%q,%q)", args, codeCold, outCold, errCold, codeWarm, outWarm, errWarm)
		}
	}
	cache := newWaitCache(io.Discard)
	record := cache.recordPath(path)
	if err := os.WriteFile(record, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runTL(t, e, queries[0]...)
	if code != 3 || strings.Contains(errOut, "cache") {
		t.Fatalf("corrupt entry result = (%d, %q)", code, errOut)
	}
	if !cache.valid(path) {
		t.Fatal("corrupt entry was not replaced")
	}
	cachePath := cache.recordPath(path)
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(cachePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// Заголовок цел, события испорчены: допуск запись примет, но чтение событий даст промах и перечтёт транскрипт.
	wantCode, wantOut, wantErr := runTL(t, e, queries[0]...)
	if _, _, err := cache.load(path, true); err != nil {
		t.Fatalf("damaged payload was not rewritten: %v", err)
	}
	if err := os.WriteFile(cachePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cache.load(path, true); !errors.Is(err, errWaitCacheDecode) {
		t.Fatalf("damaged payload decoded: %v", err)
	}
	code, out, errOut := runTL(t, e, queries[0]...)
	if code != wantCode || out != wantOut || errOut != wantErr {
		t.Fatalf("damaged payload result = (%d, %q, %q), want (%d, %q, %q)", code, out, errOut, wantCode, wantOut, wantErr)
	}
	if _, state, err := cache.load(path, true); err != nil || state != waitCacheHit {
		t.Fatalf("damaged payload was not replaced: %v", err)
	}
}

func TestWaitCachePreservesRepeatedCallIDSemantics(t *testing.T) {
	e := fakeHome(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	duplicate := strings.Join([]string{
		`{"type":"assistant","timestamp":"2026-10-25T09:59:58Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"same","name":"Bash","input":{"command":"tk done duplicate"}}]}}`,
		`{"type":"assistant","timestamp":"2026-10-25T09:59:59Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"same","name":"Bash","input":{"command":"tk done duplicate"}}]}}`,
		`{"type":"user","timestamp":"2026-10-25T10:00:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"same","content":"ok"}]}}`,
	}, "\n")
	specs := []struct {
		command string
		delay   int
		bg      bool
		output  bool
	}{{"tk done one", 1, false, true}, {"tk done two", 2, false, true}, {"tk done three", 3, false, true}}
	path := writeWaitTranscript(t, e, "repeat.jsonl", duplicate+"\n"+claudeWaitRows(specs))
	args := []string{"--wait", "tk done", "--q", "50", path}
	codeCold, cold, errCold := runTL(t, e, args...)
	codeWarm, warm, errWarm := runTL(t, e, args...)
	if codeCold != 0 || codeWarm != codeCold || cold != "2000\n" || warm != cold || errCold != "" || errWarm != "" {
		t.Fatalf("cold=(%d,%q,%q) warm=(%d,%q,%q)", codeCold, cold, errCold, codeWarm, warm, errWarm)
	}
}

// codexWaitRows — rollout codex с n вызовами `tk done`, каждый ждёт вывода 1 с.
func codexWaitRows(n int) string {
	rows := []string{`{"timestamp":"2026-10-25T10:00:00Z","type":"session_meta","payload":{"id":"synthetic"}}`}
	for i := 1; i <= n; i++ {
		rows = append(rows, fmt.Sprintf(`{"timestamp":"2026-10-25T10:00:%02dZ","type":"response_item","payload":{"type":"function_call","call_id":"c%d","name":"exec_command","arguments":%q}}`, i, i, `{"cmd":"tk done item | head"}`))
		rows = append(rows, fmt.Sprintf(`{"timestamp":"2026-10-25T10:00:%02dZ","type":"response_item","payload":{"type":"function_call_output","call_id":"c%d","output":"ok"}}`, i+1, i))
	}
	return strings.Join(rows, "\n") + "\n"
}

func TestWaitCodexAndInsufficientData(t *testing.T) {
	e := fakeHome(t)
	for _, tc := range []struct {
		events int
		q      string
		code   int
		out    string
		errOut string
	}{
		{events: 2, q: "50", code: 3, errOut: "2 wait event(s); --q 50 needs at least 3"},
		{events: 3, q: "50", code: 0, out: "1000\n"},
		{events: 3, q: "51", code: 3, errOut: "3 wait event(s); --q 51 needs at least 20"},
		{events: 3, q: "95", code: 3, errOut: "3 wait event(s); --q 95 needs at least 20"},
		{events: 19, q: "50", code: 0, out: "1000\n"},
		{events: 19, q: "95", code: 3, errOut: "19 wait event(s); --q 95 needs at least 20"},
		{events: 20, q: "95", code: 0, out: "1000\n"},
	} {
		path := writeWaitTranscript(t, e, fmt.Sprintf("rollout-synthetic-%d.jsonl", tc.events), codexWaitRows(tc.events))
		code, out, errOut := runTL(t, e, "--wait", "tk done", "--q", tc.q, path)
		if code != tc.code || out != tc.out || !strings.Contains(errOut, tc.errOut) {
			t.Errorf("events=%d q=%s: code=%d out=%q stderr=%q", tc.events, tc.q, code, out, errOut)
		}
	}
}

func TestWaitIgnoresNonShellAndMultipleCodexCommands(t *testing.T) {
	e := fakeHome(t)
	grep := strings.Join([]string{
		`{"type":"assistant","timestamp":"2026-10-25T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"grep1","name":"Grep","input":{"pattern":"sub"}}]}}`,
		`{"type":"user","timestamp":"2026-10-25T10:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"grep1","content":"match"}]}}`,
	}, "\n") + "\n"
	grepPath := writeWaitTranscript(t, e, "wait-grep.jsonl", grep)
	code, out, errOut := runTL(t, e, "--wait", "sub", grepPath)
	if code != 3 || out != "" || !strings.Contains(errOut, "0 wait event") {
		t.Errorf("Grep code=%d out=%q stderr=%q", code, out, errOut)
	}

	codeText := `const a = await tools.exec_command({cmd:"tk done one"}); const b = await tools.exec_command({cmd:"tk done two"});`
	codex := strings.Join([]string{
		`{"timestamp":"2026-10-25T10:00:00Z","type":"session_meta","payload":{"id":"synthetic"}}`,
		fmt.Sprintf(`{"timestamp":"2026-10-25T10:00:01Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"exec1","name":"exec","input":%q}}`, codeText),
		`{"timestamp":"2026-10-25T10:00:03Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"exec1","output":"ok"}}`,
	}, "\n") + "\n"
	codexPath := writeWaitTranscript(t, e, "rollout-multiple.jsonl", codex)
	code, out, errOut = runTL(t, e, "--wait", "tk done", codexPath)
	if code != 3 || out != "" || !strings.Contains(errOut, "0 wait event") {
		t.Errorf("multiple commands code=%d out=%q stderr=%q", code, out, errOut)
	}
}

func TestWaitUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--wait", "tk", "--usage"}, {"--wait", "tk", "--role", "cmd"},
		{"--wait", "tk", "--max", "5"}, {"--q", "50"},
		{"--wait", "tk", "--q", "100"}, {"--wait", "a b c"},
	} {
		if _, err := parseArgs(args, fakeHome(t).now); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
}
