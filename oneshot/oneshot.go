// Package oneshot — разовый вызов одобренной модели через её CLI: текст на вход, последний ответ и расход на выход.
// opus — `claude -p` без инструментов, настроек и MCP; sol и luna — `codex exec` без пользовательского конфига,
// правил, доков проекта и инструментов, в песочнице только для чтения. Подписки те же, что у агентов.
// и других потребителей.
package oneshot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/akit/price"
)

// Request — что и какой модели отдать.
type Request struct {
	// Model — имя из списка одобренных (opus, sol, luna); id для CLI берётся оттуда же.
	Model string
	// Effort — усилие рассуждения как его понимает CLI модели (low, medium, high…); пусто — по умолчанию CLI.
	Effort string
	// System — системный промпт; у codex — файлом model_instructions_file вместо встроенных инструкций.
	System string
	// Schema — JSON Schema ответа строкой: у claude --json-schema, у codex --output-schema; пусто — свободный текст.
	Schema string
	// Input — текст запроса, уходит на stdin.
	Input string
	// Dir — папка запуска; пусто — новая пустая временная, без CLAUDE.md и AGENTS.md, после вызова удаляется.
	Dir string
	// Timeout — потолок вызова; 0 — только ctx.
	Timeout time.Duration
	// Approved — файл одобренных моделей; пусто — ~/.config/models/approved.
	Approved string
}

// Result — ответ модели.
type Result struct {
	Text string
	// ID — id модели из списка одобренных: по нему price.Cost считает цену.
	ID string
	// Usage — токены вызова: Input без кэша, CacheRead — из кэша, CacheWrite — запись в кэш (только у claude).
	Usage price.Usage
}

// Ask отдаёт запрос модели и ждёт её последний ответ. Ошибка CLI несёт хвост его stderr.
func Ask(ctx context.Context, req Request) (Result, error) {
	path := req.Approved
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Result{}, fmt.Errorf("oneshot: %w", err)
		}
		path = filepath.Join(home, ".config", "models", "approved")
	}
	models, err := Approved(path)
	if err != nil {
		return Result{}, err
	}
	id, ok := models[req.Model]
	if !ok {
		return Result{}, fmt.Errorf("oneshot: model %q is not in %s", req.Model, path)
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	tmp, err := os.MkdirTemp("", "oneshot-") // файлы вызова (инструкции, схема) и папка запуска по умолчанию
	if err != nil {
		return Result{}, fmt.Errorf("oneshot: %w", err)
	}
	defer os.RemoveAll(tmp)
	if req.Dir == "" {
		req.Dir = tmp
	}
	res := Result{ID: id}
	if req.Model == "opus" {
		res.Text, res.Usage, err = askClaude(ctx, req)
	} else {
		res.Text, res.Usage, err = askCodex(ctx, req, id, tmp)
	}
	return res, err
}

// Approved читает список одобренных моделей: строки «<имя> <id>», комментарии с «#». Модели вне списка не
// вызываются нигде.
func Approved(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("oneshot: approved models: %w", err)
	}
	models := map[string]string{}
	for l := range strings.SplitSeq(string(data), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Fields(l)
		if len(f) != 2 {
			return nil, fmt.Errorf("oneshot: %s: malformed line %q", path, l)
		}
		models[f[0]] = f[1]
	}
	return models, nil
}

func askClaude(ctx context.Context, req Request) (string, price.Usage, error) {
	args := []string{"-p", "--model", req.Model, "--tools", "", "--setting-sources", "", "--strict-mcp-config",
		"--no-session-persistence", "--disable-slash-commands", "--output-format", "json"}
	if req.System != "" {
		args = append(args, "--system-prompt", req.System)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if req.Schema != "" {
		args = append(args, "--json-schema", req.Schema)
	}
	out, err := run(ctx, exec.CommandContext(ctx, "claude", args...), req.Dir, req.Input)
	if err != nil {
		return "", price.Usage{}, err
	}
	var r struct {
		Result string `json:"result"`
		// StructuredOutput — ответ по --json-schema.
		StructuredOutput json.RawMessage `json:"structured_output"`
		IsError          bool            `json:"is_error"`
		Usage            struct {
			Input      int64 `json:"input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			Output     int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", price.Usage{}, fmt.Errorf("oneshot: claude output is not JSON: %w", err)
	}
	u := price.Usage{Input: r.Usage.Input, CacheWrite: r.Usage.CacheWrite, CacheRead: r.Usage.CacheRead,
		Output: r.Usage.Output}
	if r.IsError {
		return "", u, fmt.Errorf("oneshot: claude returned an error: %s", short(r.Result))
	}
	if len(r.StructuredOutput) > 0 && string(r.StructuredOutput) != "null" {
		return string(r.StructuredOutput), u, nil
	}
	return r.Result, u, nil
}

// codexOff — возможности codex, которые разовому ответу не нужны: без них ответ без вызова инструментов, а вход
// меньше вдвое (замер на gpt-6-luna: 12,8K входа с инструментами и инструкциями во входе, 6,0K без).
var codexOff = []string{"apps", "browser_use", "browser_use_external", "computer_use", "image_generation",
	"multi_agent", "plugins", "skill_search", "shell_tool", "unified_exec", "view_image", "goals", "sleep_tool",
	"collaboration_modes", "tool_suggest", "in_app_browser", "mentions_v2", "hooks", "steer"}

func askCodex(ctx context.Context, req Request, id, tmp string) (string, price.Usage, error) {
	args := []string{"exec", "--json", "--skip-git-repo-check", "--ephemeral", "--ignore-user-config",
		"--ignore-rules", "--sandbox", "read-only", "-m", id, "-c", `web_search="disabled"`,
		"-c", "project_doc_max_bytes=0", "-c", "include_apply_patch_tool=false"}
	for _, f := range codexOff {
		args = append(args, "--disable", f)
	}
	if req.Effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", req.Effort))
	}
	if req.System != "" {
		p := filepath.Join(tmp, "instructions.md")
		if err := os.WriteFile(p, []byte(req.System), 0o600); err != nil {
			return "", price.Usage{}, fmt.Errorf("oneshot: %w", err)
		}
		args = append(args, "-c", fmt.Sprintf("model_instructions_file=%q", p))
	}
	if req.Schema != "" {
		p := filepath.Join(tmp, "schema.json")
		if err := os.WriteFile(p, []byte(req.Schema), 0o600); err != nil {
			return "", price.Usage{}, fmt.Errorf("oneshot: %w", err)
		}
		args = append(args, "--output-schema", p)
	}
	out, err := run(ctx, exec.CommandContext(ctx, "codex", append(args, "-")...), req.Dir, req.Input)
	if err != nil {
		return "", price.Usage{}, err
	}
	return codexAnswer(out)
}

// codexEvent — строка `codex exec --json`, нужные поля.
type codexEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Item    struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage struct {
		Input       int64 `json:"input_tokens"`
		CachedInput int64 `json:"cached_input_tokens"`
		Output      int64 `json:"output_tokens"`
	} `json:"usage"`
}

// codexAnswer — последнее сообщение агента и расход из событий codex. Кэш codex входит в input_tokens, reasoning —
// в output_tokens.
func codexAnswer(out []byte) (string, price.Usage, error) {
	var msg string
	var u price.Usage
	var failed, errs []string // turn.failed — ход не удался; error — и переподключения, после которых ответ бывает
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var e codexEvent
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case "item.completed":
			if e.Item.Type == "agent_message" {
				msg = e.Item.Text
			}
		case "turn.completed":
			u.Input += e.Usage.Input - e.Usage.CachedInput
			u.CacheRead += e.Usage.CachedInput
			u.Output += e.Usage.Output
		case "turn.failed":
			failed = append(failed, e.Error.Message)
		case "error":
			errs = append(errs, e.Message)
		}
	}
	if err := sc.Err(); err != nil {
		return "", u, fmt.Errorf("oneshot: read codex output: %w", err)
	}
	if len(failed) > 0 || msg == "" && len(errs) > 0 {
		return "", u, fmt.Errorf("oneshot: codex failed: %s", short(strings.Join(append(errs, failed...), "; ")))
	}
	if msg == "" {
		return "", u, errors.New("oneshot: codex gave no agent message")
	}
	return msg, u, nil
}

// env — окружение без признаков сессии-родителя: вложенный claude иначе принял бы чужое имя сессии и CLAUDE_PID.
func env() []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "CLAUDE_CODE_SESSION_NAME", "CLAUDE_PID", "CLAUDE_CODE_SESSION_ID", "CLAUDECODE":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// run запускает CLI с текстом на stdin; ошибка несёт хвост stderr.
func run(ctx context.Context, cmd *exec.Cmd, dir, stdin string) ([]byte, error) {
	cmd.Dir = dir
	cmd.Env = env()
	// По таймауту контекст убивает сам CLI, а его потомки держат stdout: без WaitDelay вызов ждал бы их.
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("oneshot: %s: %w: %s", cmd.Args[0], err, short(strings.TrimSpace(stderr.String())))
	}
	return out, nil
}

// short — хвост сообщения не длиннее 300 рун.
func short(s string) string {
	r := []rune(s)
	if len(r) <= 300 {
		return s
	}
	return "…" + string(r[len(r)-300:])
}
