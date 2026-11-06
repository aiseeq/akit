package oneshot

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/price"
)

// fakeCLI кладёт в PATH скрипт name: он пишет аргументы, окружение и stdin в файлы папки, копирует туда файлы из
// аргументов (и из «ключ="путь"») и печатает out.
func fakeCLI(t *testing.T, name, out string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + dir + "/args\nenv > " + dir + "/env\npwd > " + dir +
		"/pwd\ncat > " + dir + "/stdin\nfor a; do p=${a#*=}; p=${p#\\\"}; p=${p%\\\"}; [ -f \"$p\" ] && cp \"$p\" " + dir +
		"/; done\ncat <<'EOF'\n" + out + "\nEOF\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

func approved(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "approved")
	data := "# comment\nopus claude-opus-5-5\nsol gpt-6.1-sol\n"
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestClaude(t *testing.T) {
	dir := fakeCLI(t, "claude", `{"result":"ответ","is_error":false,"usage":{"input_tokens":5,`+
		`"cache_creation_input_tokens":100,"cache_read_input_tokens":2000,"output_tokens":7}}`)
	t.Setenv("CLAUDE_PID", "123")
	t.Setenv("CLAUDECODE", "1")
	res, err := Ask(context.Background(), Request{Model: "opus", Effort: "low", System: "будь краток",
		Input: "вопрос", Approved: approved(t)})
	if err != nil {
		t.Fatal(err)
	}
	want := price.Usage{Input: 5, CacheWrite: 100, CacheRead: 2000, Output: 7}
	if res.Text != "ответ" || res.ID != "claude-opus-5-5" || res.Usage != want {
		t.Errorf("got %+v", res)
	}
	args := read(t, dir+"/args")
	for _, a := range []string{"--system-prompt\nбудь краток\n", "--effort\nlow\n", "--tools\n\n", "--model\nopus\n"} {
		if !strings.Contains(args, a) {
			t.Errorf("args lack %q:\n%s", a, args)
		}
	}
	if read(t, dir+"/stdin") != "вопрос" {
		t.Errorf("stdin %q", read(t, dir+"/stdin"))
	}
	if e := read(t, dir+"/env"); strings.Contains(e, "CLAUDE_PID=") || strings.Contains(e, "CLAUDECODE=") {
		t.Error("parent session markers leaked into env")
	}
	if pwd := strings.TrimSpace(read(t, dir+"/pwd")); !strings.Contains(pwd, "oneshot-") {
		t.Errorf("ran in %s, want a fresh temp dir", pwd)
	} else if _, err := os.Stat(pwd); !os.IsNotExist(err) {
		t.Errorf("temp dir %s left behind", pwd)
	}
}

func TestClaudeError(t *testing.T) {
	fakeCLI(t, "claude", `{"result":"Prompt is too long","is_error":true,"usage":{}}`)
	_, err := Ask(context.Background(), Request{Model: "opus", Input: "x", Approved: approved(t)})
	if err == nil || !strings.Contains(err.Error(), "Prompt is too long") {
		t.Errorf("err %v", err)
	}
}

func TestClaudeRefusalInErrors(t *testing.T) {
	refusal := `{"is_error":true,"result":"","errors":["Reached maximum budget"],"usage":{"input_tokens":3}}`
	fakeCLI(t, "claude", refusal)
	if _, err := Ask(context.Background(), Request{Model: "opus", Input: "x", Approved: approved(t)}); err == nil ||
		!strings.Contains(err.Error(), "Reached maximum budget") {
		t.Errorf("code 0: err %v", err)
	}
	fakeFailing(t, "claude", refusal, "")
	res, err := Ask(context.Background(), Request{Model: "opus", Input: "x", Approved: approved(t)})
	if err == nil || !strings.Contains(err.Error(), "exit status 1: Reached maximum budget") {
		t.Errorf("code 1: err %v", err)
	}
	if res.Usage.Input != 3 {
		t.Errorf("code 1: usage %+v", res.Usage)
	}
}

// Поле errors другой формы не отнимает ни ответ, ни причину отказа.
func TestClaudeErrorsOfAnyShape(t *testing.T) {
	for _, errs := range []string{`[{"message":"warn"}]`, `"warn"`, `[1,2]`, `null`, `[]`} {
		fakeCLI(t, "claude", `{"is_error":false,"result":"ответ","errors":`+errs+`,"usage":{"output_tokens":7}}`)
		res, err := Ask(context.Background(), Request{Model: "opus", Input: "x", Approved: approved(t)})
		if err != nil || res.Text != "ответ" || res.Usage.Output != 7 {
			t.Errorf("errors %s, answer: %+v %v", errs, res, err)
		}
		fakeCLI(t, "claude", `{"is_error":false,"result":"","structured_output":{"a":1},"errors":`+errs+`,"usage":{}}`)
		res, err = Ask(context.Background(), Request{Model: "opus", Input: "x", Approved: approved(t)})
		if err != nil || res.Text != `{"a":1}` {
			t.Errorf("errors %s, schema answer: %+v %v", errs, res, err)
		}
		refusal := `{"is_error":true,"result":"refused","errors":` + errs + `,"usage":{}}`
		fakeCLI(t, "claude", refusal)
		if _, err := Ask(context.Background(), Request{Model: "opus", Input: "x", Approved: approved(t)}); err == nil ||
			!strings.Contains(err.Error(), "refused") {
			t.Errorf("errors %s, code 0 refusal: %v", errs, err)
		}
		fakeFailing(t, "claude", refusal, "")
		if _, err := Ask(context.Background(), Request{Model: "opus", Input: "x", Approved: approved(t)}); err == nil ||
			!strings.Contains(err.Error(), "exit status 1: refused") {
			t.Errorf("errors %s, code 1 refusal: %v", errs, err)
		}
	}
}

func TestClaudeOutputNotJSON(t *testing.T) {
	fakeCLI(t, "claude", "not json")
	_, err := Ask(context.Background(), Request{Model: "opus", Input: "x", Approved: approved(t)})
	if err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("err %v", err)
	}
}

// Ненулевой код без отказа в JSON: причина остаётся в stderr.
func TestClaudeNonZeroExitKeepsStderr(t *testing.T) {
	for _, stdout := range []string{"not json", `{"is_error":false,"result":"частичный ответ","usage":{}}`,
		`{"is_error":true,"result":"","usage":{}}`} {
		fakeFailing(t, "claude", stdout, "config is broken")
		_, err := Ask(context.Background(), Request{Model: "opus", Input: "x", Approved: approved(t)})
		if err == nil || !strings.Contains(err.Error(), "exit status 1: config is broken") {
			t.Errorf("stdout %q: err %v", stdout, err)
		}
	}
}

func TestCodex(t *testing.T) {
	dir := fakeCLI(t, "codex", `{"type":"thread.started"}
{"type":"item.completed","item":{"type":"agent_message","text":"первый"}}
{"type":"item.completed","item":{"type":"reasoning","text":"думаю"}}
{"type":"item.completed","item":{"type":"agent_message","text":"последний"}}
{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":800,"output_tokens":50}}`)
	work := t.TempDir()
	res, err := Ask(context.Background(), Request{Model: "sol", Effort: "high", System: "правила", Input: "вопрос",
		Schema: `{"type":"object"}`, Dir: work, Approved: approved(t)})
	if err != nil {
		t.Fatal(err)
	}
	if want := (price.Usage{Input: 200, CacheRead: 800, Output: 50}); res.Text != "последний" || res.Usage != want {
		t.Errorf("got %+v", res)
	}
	args := read(t, dir+"/args")
	for _, a := range []string{"-m\ngpt-6.1-sol\n", "model_reasoning_effort=\"high\"\n", "--sandbox\nread-only\n",
		"--disable\nshell_tool\n", "include_apply_patch_tool=false\n", "--output-schema\n", "model_instructions_file="} {
		if !strings.Contains(args, a) {
			t.Errorf("args lack %q:\n%s", a, args)
		}
	}
	if s := read(t, dir+"/stdin"); s != "вопрос" {
		t.Errorf("stdin %q", s)
	}
	if read(t, dir+"/instructions.md") != "правила" || read(t, dir+"/schema.json") != `{"type":"object"}` {
		t.Error("instructions or schema file content")
	}
	if _, err := os.Stat(work + "/instructions.md"); err == nil {
		t.Error("call files written into the caller's Dir")
	}
	if pwd := strings.TrimSpace(read(t, dir+"/pwd")); pwd != work {
		t.Errorf("ran in %s, want %s", pwd, work)
	}
}

func TestCodexFailed(t *testing.T) {
	fakeCLI(t, "codex", `{"type":"error","message":"stream disconnected"}
{"type":"turn.failed","error":{"message":"usage limit reached"}}`)
	_, err := Ask(context.Background(), Request{Model: "sol", Input: "x", Approved: approved(t)})
	if err == nil || !strings.Contains(err.Error(), "usage limit reached") {
		t.Errorf("err %v", err)
	}
	fakeCLI(t, "codex", `{"type":"turn.completed","usage":{}}`)
	if _, err := Ask(context.Background(), Request{Model: "sol", Input: "x", Approved: approved(t)}); err == nil {
		t.Error("no agent message must be an error")
	}
	fakeCLI(t, "codex", `{"type":"error","message":"Reconnecting... 1/5"}
{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}
{"type":"turn.completed","usage":{}}`)
	if res, err := Ask(context.Background(), Request{Model: "sol", Input: "x", Approved: approved(t)}); err != nil ||
		res.Text != "ok" {
		t.Errorf("answer after a reconnect: %q %v", res.Text, err)
	}
}

// fakeFailing кладёт в PATH скрипт name, который пишет stdout и stderr и выходит с кодом 1.
func fakeFailing(t *testing.T, name, stdout, stderr string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat <<'EOF'\n" + stdout + "\nEOF\ncat >&2 <<'EOF'\n" + stderr + "\nEOF\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// Ненулевой код: причина из событий stdout, при их отсутствии — stderr, как раньше.
func TestCodexNonZeroExit(t *testing.T) {
	cases := []struct{ name, stdout, stderr, want, wantNot string }{
		{"turn.failed, empty stderr", `{"type":"turn.failed","error":{"message":"out of credits"}}`, "",
			"exit status 1: out of credits", ""},
		{"error event", `{"type":"error","message":"model is overloaded"}`, "",
			"exit status 1: model is overloaded", ""},
		{"events win over stderr noise", `{"type":"error","message":"retrying"}
{"type":"turn.failed","error":{"message":"out of credits"}}`, "Reading prompt from stdin",
			"exit status 1: retrying; out of credits", "Reading prompt"},
		{"same text in both events is said once", `{"type":"error","message":"out of credits"}
{"type":"turn.failed","error":{"message":"out of credits"}}`, "", "exit status 1: out of credits", "credits; out"},
		{"only error events, stderr stays too", `{"type":"error","message":"Reconnecting... 5/5"}`, "auth.json is corrupt",
			"exit status 1: Reconnecting... 5/5; auth.json is corrupt", ""},
		{"long stderr does not push out the event", `{"type":"error","message":"model is overloaded"}`,
			strings.Repeat("W", 400), "exit status 1: model is overloaded; …WWW", ""},
		{"turn.failed without text, stderr stays too", `{"type":"error","message":"boom"}
{"type":"turn.failed","error":{}}`, "auth.json is corrupt", "exit status 1: boom; auth.json is corrupt", ""},
		{"no events, stderr stays", `{"type":"thread.started"}`, "config is broken",
			"exit status 1: config is broken", ""},
		{"stdout is not JSON", "panic", "config is broken", "exit status 1: config is broken", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeFailing(t, "codex", c.stdout, c.stderr)
			_, err := Ask(context.Background(), Request{Model: "sol", Input: "x", Approved: approved(t)})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
			if c.wantNot != "" && strings.Contains(err.Error(), c.wantNot) {
				t.Errorf("err %v must not carry %q", err, c.wantNot)
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Errorf("err %v lost the exit error", err)
			}
		})
	}
}

// Расход уже сделанных ходов доходит до вызывающего и при ненулевом коде, с отказом и без него.
func TestCodexNonZeroExitKeepsUsage(t *testing.T) {
	completed := `{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":800,"output_tokens":50}}`
	want := price.Usage{Input: 200, CacheRead: 800, Output: 50}
	for _, stdout := range []string{completed, completed + "\n" + `{"type":"turn.failed","error":{"message":"out of credits"}}`} {
		fakeFailing(t, "codex", stdout, "")
		res, err := Ask(context.Background(), Request{Model: "sol", Input: "x", Approved: approved(t)})
		if err == nil || res.Usage != want {
			t.Errorf("stdout %q: usage %+v, err %v", stdout, res.Usage, err)
		}
	}
}

// Строка длиннее буфера не отнимает уже прочитанный отказ, и сбой чтения виден в причине.
func TestCodexOverlongLineKeepsRefusal(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho '{\"type\":\"turn.failed\",\"error\":{\"message\":\"out of credits\"}}'\n" +
		"head -c 68157440 /dev/zero | tr '\\0' x\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	_, err := Ask(context.Background(), Request{Model: "sol", Input: "x", Approved: approved(t)})
	if err == nil || !strings.Contains(err.Error(), "out of credits") || !strings.Contains(err.Error(), "read codex output") {
		t.Errorf("err %v", err)
	}
}

// Срыв по сроку остаётся срывом по сроку, даже если codex успел написать событие error.
func TestCodexTimeoutKeepsContextError(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho '{\"type\":\"error\",\"message\":\"Reconnecting... 1/5\"}'\nsleep 10\n"
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	_, err := Ask(context.Background(), Request{Model: "sol", Input: "x", Timeout: 100 * time.Millisecond,
		Approved: approved(t)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err %v, want deadline exceeded", err)
	}
}

func envValue(t *testing.T, path, name string) (values []string) {
	t.Helper()
	for l := range strings.SplitSeq(read(t, path), "\n") {
		if v, ok := strings.CutPrefix(l, name+"="); ok {
			values = append(values, v)
		}
	}
	return values
}

func TestCodexHome(t *testing.T) {
	answer := `{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`
	t.Setenv("CODEX_HOME", "/parent/home")
	dir := fakeCLI(t, "codex", answer)
	if _, err := Ask(context.Background(), Request{Model: "sol", Input: "x", Approved: approved(t)}); err != nil {
		t.Fatal(err)
	}
	if got := envValue(t, dir+"/env", "CODEX_HOME"); !slices.Equal(got, []string{"/parent/home"}) {
		t.Errorf("empty CodexHome: child sees %q, want the parent's", got)
	}
	if _, err := Ask(context.Background(), Request{Model: "sol", Input: "x", CodexHome: "/chosen/home",
		Approved: approved(t)}); err != nil {
		t.Fatal(err)
	}
	if got := envValue(t, dir+"/env", "CODEX_HOME"); !slices.Equal(got, []string{"/chosen/home"}) {
		t.Errorf("CodexHome set: child sees %q", got)
	}
}

func TestCodexHomeWithClaudeIsAnError(t *testing.T) {
	dir := fakeCLI(t, "claude", `{"result":"ответ","is_error":false,"usage":{}}`)
	_, err := Ask(context.Background(), Request{Model: "opus", Input: "x", CodexHome: "/chosen/home",
		Approved: approved(t)})
	if err == nil || !strings.Contains(err.Error(), "CodexHome") {
		t.Errorf("err %v", err)
	}
	if _, statErr := os.Stat(dir + "/args"); statErr == nil {
		t.Error("claude ran despite the error")
	}
}

func TestClaudeSchema(t *testing.T) {
	dir := fakeCLI(t, "claude", `{"result":"","structured_output":{"city":"Париж"},"is_error":false,"usage":{}}`)
	res, err := Ask(context.Background(), Request{Model: "opus", Input: "x", Schema: `{"type":"object"}`,
		Approved: approved(t)})
	if err != nil || res.Text != `{"city":"Париж"}` {
		t.Errorf("%q %v", res.Text, err)
	}
	if !strings.Contains(read(t, dir+"/args"), "--json-schema\n{\"type\":\"object\"}\n") {
		t.Error("no --json-schema")
	}
}

func TestNotApproved(t *testing.T) {
	_, err := Ask(context.Background(), Request{Model: "luna", Input: "x", Approved: approved(t)})
	if err == nil || !strings.Contains(err.Error(), `"luna" is not in`) {
		t.Errorf("err %v", err)
	}
}

func TestTimeout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nsleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	start := time.Now()
	_, err := Ask(context.Background(), Request{Model: "opus", Input: "x", Timeout: 100 * time.Millisecond,
		Approved: approved(t)})
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") || time.Since(start) > 5*time.Second {
		t.Errorf("err %v after %v", err, time.Since(start))
	}
}

// TestLive зовёт настоящие CLI: ONESHOT_LIVE=opus,luna go test ./oneshot -run Live -v; ONESHOT_CODEX_HOME — учётка
// codex, только для моделей на codex.
func TestLive(t *testing.T) {
	models := os.Getenv("ONESHOT_LIVE")
	if models == "" {
		t.Skip("ONESHOT_LIVE not set")
	}
	home := os.Getenv("ONESHOT_CODEX_HOME")
	for m := range strings.SplitSeq(models, ",") {
		res, err := Ask(context.Background(), Request{Model: m, Effort: "low", System: "Отвечай одним словом.",
			Input: "Столица Франции?", Timeout: 3 * time.Minute, CodexHome: home})
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		cost, _ := price.Cost(res.ID, res.Usage)
		t.Logf("%s %s: %q %+v $%.4f", m, res.ID, res.Text, res.Usage, cost)
		res, err = Ask(context.Background(), Request{Model: m, Effort: "low", Input: "Столица Франции?",
			Schema: `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],` +
				`"additionalProperties":false}`, Timeout: 3 * time.Minute, CodexHome: home})
		if err != nil || !strings.Contains(res.Text, `"city"`) {
			t.Fatalf("%s schema: %q %v", m, res.Text, err)
		}
		t.Logf("%s schema: %s %+v", m, res.Text, res.Usage)
	}
}
