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

	"github.com/aiseeq/akit/transcript"
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
	if code != 0 || out != "10000\n" || !strings.Contains(errOut, "20 wait event(s)") {
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

func TestWaitStartsAfterEarlierSerialCallInSameMessage(t *testing.T) {
	e := fakeHome(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	rows := []string{
		waitRow("assistant", 0, `"id":"a","name":"Bash","input":{"command":"tk done one"}`, "m", ""),
		waitRow("user", 23, `"tool_use_id":"a","content":"ok"`, "", ""),
		waitRow("assistant", 23, `"id":"b","name":"Bash","input":{"command":"tk done two"}`, "m", ""),
		waitRow("user", 39, `"tool_use_id":"b","content":"ok"`, "", ""),
		waitRow("assistant", 39, `"id":"c","name":"Bash","input":{"command":"tk done three"}`, "m", ""),
		waitRow("user", 54, `"tool_use_id":"c","content":"ok"`, "", ""),
		waitRow("assistant", 54, `"id":"d","name":"Bash","input":{"command":"tk done four"}`, "m", ""),
		waitRow("user", 67, `"tool_use_id":"d","content":"ok"`, "", ""),
	}
	path := writeWaitTranscript(t, e, "sequential.jsonl", strings.Join(rows, "\n")+"\n")
	roles := map[transcript.Role]bool{transcript.Cmd: true, transcript.Out: true}
	_, waits, _, err := (&filter{opts: options{waitProg: "tk", waitSub: "done", roles: roles}}).file(source{path: path})
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{23 * time.Second, 16 * time.Second, 15 * time.Second, 13 * time.Second}
	if fmt.Sprint(waits) != fmt.Sprint(want) {
		var events []string
		_, readErr := transcript.Read(path, nil, func(ev transcript.Event) {
			events = append(events, fmt.Sprintf("%s %s %s %s %s", ev.Role, ev.Who, ev.CallID, ev.MessageID, ev.Text))
		})
		t.Fatalf("waits = %v, want %v; events=%v readErr=%v", waits, want, events, readErr)
	}
}

func waitRow(kind string, second int, part, messageID, _ string) string {
	timestamp := time.Date(2026, 10, 25, 10, 0, second, 0, time.UTC).Format(time.RFC3339)
	if kind == "assistant" {
		return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":%q,"content":[{"type":"tool_use",%s}]}}`, timestamp, messageID, part)
	}
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"content":[{"type":"tool_result",%s}]}}`, timestamp, part)
}

func TestWaitParallelReadIndependentMessagesAndFirstResult(t *testing.T) {
	e := fakeHome(t)
	rows := []string{
		waitRow("assistant", 0, `"id":"read","name":"Read","input":{"file_path":"x"}`, "m1", ""),
		waitRow("assistant", 1, `"id":"bash","name":"Bash","input":{"command":"tk done bash"}`, "m1", ""),
		waitRow("assistant", 2, `"id":"other","name":"Bash","input":{"command":"tk done other"}`, "m2", ""),
		waitRow("user", 3, `"tool_use_id":"read","content":"data"`, "", ""),
		waitRow("user", 11, `"tool_use_id":"bash","content":"ok"`, "", ""),
		waitRow("user", 12, `"tool_use_id":"other","content":"ok"`, "", ""),
		waitRow("assistant", 13, `"id":"repeat","name":"Bash","input":{"command":"tk done repeated"}`, "m3", ""),
		waitRow("user", 17, `"tool_use_id":"repeat","content":"first"`, "", ""),
		waitRow("user", 30, `"tool_use_id":"repeat","content":"duplicate"`, "", ""),
	}
	waits := readWaits(t, e, "parallel.jsonl", rows)
	want := []time.Duration{10 * time.Second, 10 * time.Second, 4 * time.Second}
	if fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
}

func TestWaitExcludesBackgroundPermissionRefusalAndSidechainPredecessor(t *testing.T) {
	e := fakeHome(t)
	rows := []string{
		`{"type":"assistant","timestamp":"2026-10-25T10:00:00Z","message":{"id":"m","content":[{"type":"tool_use","id":"bg","name":"Bash","input":{"command":"tk done background","run_in_background":true}}]}}`,
		`{"type":"assistant","timestamp":"2026-10-25T10:00:00Z","message":{"id":"m","content":[{"type":"tool_use","id":"denied","name":"Bash","input":{"command":"tk done denied"}}]}}`,
		`{"type":"assistant","timestamp":"2026-10-25T10:00:00Z","message":{"id":"m","content":[{"type":"tool_use","id":"hook","name":"Bash","input":{"command":"tk done hook"}}]}}`,
		`{"type":"user","timestamp":"2026-10-25T10:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"bg","content":"started"}]}}`,
		`{"type":"user","timestamp":"2026-10-25T10:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"denied","is_error":true,"content":"User doesn't want to proceed with this tool use"}]}}`,
		`{"type":"user","timestamp":"2026-10-25T10:00:04Z","message":{"content":[{"type":"tool_result","tool_use_id":"hook","is_error":true,"content":"PreToolUse hook denied this tool call"}]}}`,
		`{"type":"assistant","isSidechain":true,"timestamp":"2026-10-25T10:00:00Z","message":{"id":"shared","content":[{"type":"tool_use","id":"sub","name":"Bash","input":{"command":"tk done sidechain"}}]}}`,
		`{"type":"assistant","timestamp":"2026-10-25T10:00:00Z","message":{"id":"shared","content":[{"type":"tool_use","id":"parent","name":"Bash","input":{"command":"tk done parent"}}]}}`,
		`{"type":"user","timestamp":"2026-10-25T10:00:08Z","message":{"content":[{"type":"tool_result","tool_use_id":"sub","content":"ok"}]}}`,
		`{"type":"user","timestamp":"2026-10-25T10:00:10Z","message":{"content":[{"type":"tool_result","tool_use_id":"parent","content":"ok"}]}}`,
	}
	waits := readWaits(t, e, "excluded.jsonl", rows)
	if fmt.Sprint(waits) != fmt.Sprint([]time.Duration{8 * time.Second, 10 * time.Second}) {
		t.Fatalf("waits = %v, want [8s 10s]", waits)
	}
}

func readWaits(t *testing.T, e env, name string, rows []string) []time.Duration {
	t.Helper()
	path := writeWaitTranscript(t, e, name, strings.Join(rows, "\n")+"\n")
	roles := map[transcript.Role]bool{transcript.Cmd: true, transcript.Out: true}
	_, waits, _, err := (&filter{opts: options{waitProg: "tk", waitSub: "done", roles: roles}}).file(source{path: path})
	if err != nil {
		t.Fatal(err)
	}
	return waits
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
	if code != 0 || cold != "2000\n" || !strings.Contains(coldErr, "3 wait event(s)") {
		t.Fatalf("cold = (%d, %q, %q)", code, cold, coldErr)
	}
	code, warm, warmErr := runTL(t, e, args...)
	if code != 0 || warm != cold || !strings.Contains(warmErr, "3 wait event(s)") {
		t.Fatalf("warm = (%d, %q, %q), cold %q", code, warm, warmErr, cold)
	}
	cache := newWaitCache(io.Discard)
	if !cache.valid(path) {
		t.Fatal("cold call did not save a usable entry")
	}
	oldKey, err := cache.key(path)
	if err != nil {
		t.Fatal(err)
	}
	oldKey.Version--
	cache.saveRecord(oldKey, nil)
	if cache.valid(path) {
		t.Fatal("cache entry from the previous wait rule version was accepted")
	}
	otherBinary := &waitCache{dir: cache.dir, binary: "different-binary"}
	if otherBinary.valid(path) {
		t.Fatal("entry from another binary fingerprint was accepted")
	}
	// Ключ ловит каждое поле отдельно. Тот же размер, другой mtime: задержки 1, 5, 6 вместо 1, 2, 3 —
	// строки той же длины, p50 сдвигается с 2 до 5 с.
	sameSize := []struct {
		command string
		delay   int
		bg      bool
		output  bool
	}{{"tk done one", 1, false, true}, {"tk done two", 5, false, true}, {"tk done three", 6, false, true}}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(claudeWaitRows(sameSize)), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := before.ModTime().Add(time.Hour)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if after, err := os.Stat(path); err != nil || after.Size() != before.Size() {
		t.Fatalf("same-size rewrite changed the size: %v", err)
	}
	if code, out, errOut := runTL(t, e, args...); code != 0 || out != "5000\n" || !strings.Contains(errOut, "3 wait event(s)") {
		t.Fatalf("same size, new mtime = (%d, %q, %q)", code, out, errOut)
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
	// Другой размер, тот же mtime: дописанные вызовы сдвигают p50 с 5 до 3 с.
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	code, changed, changedErr := runTL(t, e, args...)
	if code != 0 || changed != "3000\n" || !strings.Contains(changedErr, "5 wait event(s)") || !cache.valid(path) {
		t.Fatalf("changed = (%d, %q, %q)", code, changed, changedErr)
	}
}

func TestWaitCacheSkipsTranscriptChangedDuringRead(t *testing.T) {
	e := fakeHome(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	spec := []struct {
		command string
		delay   int
		bg      bool
		output  bool
	}{{"tk done one", 1, false, true}, {"tk done two", 2, false, true}, {"tk done three", 3, false, true}}
	more := append(spec, struct {
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
	path := writeWaitTranscript(t, e, "live.jsonl", claudeWaitRows(spec))
	appended := false
	afterWaitRead = func(read string) {
		if read == path && !appended {
			appended = true
			if err := os.WriteFile(path, []byte(claudeWaitRows(more)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() { afterWaitRead = func(string) {} })
	args := []string{"--wait", "tk done", "--q", "50", path}
	// Первый вызов прочёл три вызова, файл тем временем дописан до пяти: его ответ — по прочитанному,
	// а запись не сохраняется, и второй вызов читает файл заново.
	if code, out, errOut := runTL(t, e, args...); code != 0 || out != "2000\n" || !strings.Contains(errOut, "3 wait event(s)") {
		t.Fatalf("during append = (%d, %q, %q)", code, out, errOut)
	}
	if code, out, errOut := runTL(t, e, args...); code != 0 || out != "3000\n" || !strings.Contains(errOut, "5 wait event(s)") {
		t.Fatalf("after append = (%d, %q, %q), want 3000 from a fresh read", code, out, errOut)
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
	// Запрос — с ответом (tk done 1, 3, 4 с): пустой список событий вместо промаха ответ бы изменил.
	full := []string{"--wait", "tk done", "--q", "50", path}
	wantCode, wantOut, wantErr := runTL(t, e, full...)
	if wantCode != 0 || wantOut != "3000\n" {
		t.Fatalf("full window = (%d, %q, %q), want 3000", wantCode, wantOut, wantErr)
	}
	if _, _, err := cache.load(path, true); err != nil {
		t.Fatalf("damaged payload was not rewritten: %v", err)
	}
	if err := os.WriteFile(cachePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cache.load(path, true); !errors.Is(err, errWaitCacheDecode) {
		t.Fatalf("damaged payload decoded: %v", err)
	}
	code, out, errOut := runTL(t, e, full...)
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
	if codeCold != 0 || codeWarm != codeCold || cold != "2000\n" || warm != cold || (!strings.Contains(errCold, "3 wait event(s)") || !strings.Contains(errWarm, "3 wait event(s)")) {
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

// В памяти трекера — только текущее сообщение: следующее модель пишет, получив все результаты прошлого.
// Вызов без результата не держит своё сообщение после появления следующего.
func TestWaitTrackerKeepsOnlyCurrentMessage(t *testing.T) {
	tr := newWaitTracker(options{waitProg: "worker", waitSub: "run"})
	at := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	for i, id := range []string{"c1", "c2"} {
		tr.eventCall(cachedWaitEvent{Role: transcript.Cmd, CallID: id, Time: at, Line: i + 1, MessageID: "m1", Who: "Bash", Command: "worker run x"})
	}
	tr.eventCall(cachedWaitEvent{Role: transcript.Out, CallID: "c1", Time: at.Add(10 * time.Second), Line: 3})
	tr.eventCall(cachedWaitEvent{Role: transcript.Out, CallID: "c2", Time: at.Add(25 * time.Second), Line: 4})
	if len(tr.waits) != 2 || tr.waits[1] != 15*time.Second {
		t.Errorf("waits %v, want [10s 15s]", tr.waits)
	}
	tr.eventCall(cachedWaitEvent{Role: transcript.Cmd, CallID: "c3", Time: at.Add(30 * time.Second), Line: 5, MessageID: "m2", Who: "Bash", Command: "worker run y"})
	if len(tr.messages) != 1 || len(tr.messages["m2"]) != 1 {
		t.Errorf("messages after the next one: %v", tr.messages)
	}
	// Подагент (sidechain) со своим сообщением не сбрасывает сообщение родителя.
	tr.eventCall(cachedWaitEvent{Role: transcript.Cmd, CallID: "s1", Time: at.Add(31 * time.Second), Line: 6, MessageID: "sub", Who: "Bash", Command: "ls", Sidechain: true})
	if len(tr.messages["m2"]) != 1 {
		t.Errorf("sidechain message dropped the parent one: %v", tr.messages)
	}
}

// Последовательный инструмент без команды (Edit) — предшественник; порядок — по появлению, не по номеру строки.
func TestWaitStartAfterEditAndSameLine(t *testing.T) {
	tr := newWaitTracker(options{waitProg: "worker", waitSub: "run"})
	at := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	tr.eventCall(cachedWaitEvent{Role: transcript.Cmd, CallID: "e1", Time: at, Line: 7, MessageID: "m1", Who: "Edit"})
	tr.eventCall(cachedWaitEvent{Role: transcript.Cmd, CallID: "c1", Time: at, Line: 7, MessageID: "m1", Who: "Bash", Command: "worker run x"})
	tr.eventCall(cachedWaitEvent{Role: transcript.Out, CallID: "e1", Time: at.Add(4 * time.Second), Line: 8})
	tr.eventCall(cachedWaitEvent{Role: transcript.Out, CallID: "c1", Time: at.Add(19 * time.Second), Line: 9})
	if len(tr.waits) != 1 || tr.waits[0] != 15*time.Second {
		t.Errorf("waits %v, want [15s]: Edit of the same message and line comes first", tr.waits)
	}
}
