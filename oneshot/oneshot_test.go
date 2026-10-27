package oneshot

import (
	"context"
	"os"
	"path/filepath"
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

// TestLive зовёт настоящие CLI: ONESHOT_LIVE=opus,luna go test ./oneshot -run Live -v.
func TestLive(t *testing.T) {
	models := os.Getenv("ONESHOT_LIVE")
	if models == "" {
		t.Skip("ONESHOT_LIVE not set")
	}
	for m := range strings.SplitSeq(models, ",") {
		res, err := Ask(context.Background(), Request{Model: m, Effort: "low", System: "Отвечай одним словом.",
			Input: "Столица Франции?", Timeout: 3 * time.Minute})
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		cost, _ := price.Cost(res.ID, res.Usage)
		t.Logf("%s %s: %q %+v $%.4f", m, res.ID, res.Text, res.Usage, cost)
		res, err = Ask(context.Background(), Request{Model: m, Effort: "low", Input: "Столица Франции?",
			Schema: `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],` +
				`"additionalProperties":false}`, Timeout: 3 * time.Minute})
		if err != nil || !strings.Contains(res.Text, `"city"`) {
			t.Fatalf("%s schema: %q %v", m, res.Text, err)
		}
		t.Logf("%s schema: %s %+v", m, res.Text, res.Usage)
	}
}
