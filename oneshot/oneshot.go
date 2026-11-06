// Package oneshot — разовый вызов одобренной модели через её CLI: текст на вход, последний ответ и расход на выход.
// opus — `claude -p` без инструментов, настроек и MCP; sol и luna — `codex exec` без пользовательского конфига,
// правил, доков проекта и инструментов, в песочнице только для чтения. Подписки те же, что у агентов.
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
	"slices"
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
	// CodexHome — абсолютный путь, CODEX_HOME дочернего codex: так вызывающий выбирает учётку; пусто — окружение
	// процесса. С моделью claude — ошибка: у неё нет такой настройки, и вызов ушёл бы не туда, куда просили.
	CodexHome string
}

// Result — ответ модели.
type Result struct {
	Text string
	// ID — id модели из списка одобренных: по нему price.Cost считает цену.
	ID string
	// Usage — токены вызова: Input без кэша, CacheRead — из кэша, CacheWrite — запись в кэш (только у claude).
	Usage price.Usage
}

// Ask отдаёт запрос модели и ждёт её последний ответ. Ошибка CLI несёт хвост его stderr, а у codex при ненулевом
// коде — текст отказа из событий stdout, если он там есть.
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
		if req.CodexHome != "" {
			return Result{}, fmt.Errorf("oneshot: CodexHome is set for model %q, which does not run on codex", req.Model)
		}
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
	out, stderr, err := run(ctx, exec.CommandContext(ctx, "claude", args...), req.Dir, req.Input)
	var r claudeOutput
	parseErr := json.Unmarshal(out, &r)
	u := price.Usage{Input: r.Usage.Input, CacheWrite: r.Usage.CacheWrite, CacheRead: r.Usage.CacheRead,
		Output: r.Usage.Output}
	if err != nil {
		// Отказ («исчерпан бюджет») claude пишет в JSON на stdout с кодом 1, stderr при этом пуст.
		why := r.reason()
		if parseErr != nil || !r.IsError || why == "" {
			why = stderr
		}
		return "", u, cliError("claude", err, why)
	}
	if parseErr != nil {
		return "", price.Usage{}, fmt.Errorf("oneshot: claude output is not JSON: %w", parseErr)
	}
	if r.IsError {
		return "", u, fmt.Errorf("oneshot: claude returned an error: %s", short(r.reason()))
	}
	if len(r.StructuredOutput) > 0 && string(r.StructuredOutput) != "null" {
		return string(r.StructuredOutput), u, nil
	}
	return r.Result, u, nil
}

// claudeOutput — ответ `claude -p --output-format json`, нужные поля.
type claudeOutput struct {
	Result string `json:"result"`
	// Errors — причины отказа; result при этом бывает пуст.
	Errors []string `json:"errors"`
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

// reason — текст отказа: result и errors, без пустых.
func (r claudeOutput) reason() string {
	all := slices.DeleteFunc(slices.Concat([]string{r.Result}, r.Errors), func(s string) bool { return s == "" })
	return strings.Join(all, "; ")
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
	cmd := exec.CommandContext(ctx, "codex", append(args, "-")...)
	if req.CodexHome != "" {
		cmd.Env = []string{"CODEX_HOME=" + req.CodexHome}
	}
	out, stderr, err := run(ctx, cmd, req.Dir, req.Input)
	if err != nil {
		events, parseErr := parseCodex(out)
		return "", events.usage, cliError("codex", err, events.exitReason(stderr, parseErr))
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

// codexEvents — что вынесено из событий codex одного вызова.
type codexEvents struct {
	msg    string // последнее сообщение агента
	usage  price.Usage
	failed []string // turn.failed — ход не удался
	errs   []string // error — и переподключения, после которых ответ бывает
}

// reasons — тексты отказа из событий: сначала error, в конце turn.failed, потому что short оставляет хвост. Отказ
// codex пишет обоими событиями с одним текстом: повтор подряд убирается.
func (e codexEvents) reasons() []string {
	all := slices.DeleteFunc(slices.Concat(e.errs, e.failed), func(s string) bool { return s == "" })
	return slices.Compact(all)
}

// exitReason — причина ненулевого кода. Отказ («кончились кредиты») codex пишет событием в stdout, stderr при этом
// пуст. Текст turn.failed называет причину сам; одни события error могут быть переподключениями, и тогда причина
// в stderr, поэтому он остаётся.
func (e codexEvents) exitReason(stderr string, parseErr error) string {
	why := e.reasons()
	if strings.Join(e.failed, "") == "" && stderr != "" {
		why = append(why, stderr)
	}
	if parseErr != nil {
		why = append(why, parseErr.Error())
	}
	return strings.Join(why, "; ")
}

// parseCodex разбирает вывод `codex exec --json`. Кэш codex входит в input_tokens, reasoning — в output_tokens.
func parseCodex(out []byte) (codexEvents, error) {
	var ev codexEvents
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
				ev.msg = e.Item.Text
			}
		case "turn.completed":
			ev.usage.Input += e.Usage.Input - e.Usage.CachedInput
			ev.usage.CacheRead += e.Usage.CachedInput
			ev.usage.Output += e.Usage.Output
		case "turn.failed":
			ev.failed = append(ev.failed, e.Error.Message)
		case "error":
			ev.errs = append(ev.errs, e.Message)
		}
	}
	if err := sc.Err(); err != nil {
		return ev, fmt.Errorf("oneshot: read codex output: %w", err)
	}
	return ev, nil
}

// codexAnswer — ответ вызова с кодом 0: последнее сообщение агента и расход, или отказ из событий.
func codexAnswer(out []byte) (string, price.Usage, error) {
	ev, err := parseCodex(out)
	if err != nil {
		return "", ev.usage, err
	}
	if len(ev.failed) > 0 || ev.msg == "" && len(ev.errs) > 0 {
		return "", ev.usage, fmt.Errorf("oneshot: codex failed: %s", short(strings.Join(ev.reasons(), "; ")))
	}
	if ev.msg == "" {
		return "", ev.usage, errors.New("oneshot: codex gave no agent message")
	}
	return ev.msg, ev.usage, nil
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

// run запускает CLI с текстом на stdin и отдаёт stdout и stderr и при сбое: причина бывает в любом из них. cmd.Env
// дополняет окружение env(), при повторе имени верх берёт дополнение.
func run(ctx context.Context, cmd *exec.Cmd, dir, stdin string) (out []byte, stderrText string, err error) {
	cmd.Dir = dir
	cmd.Env = append(env(), cmd.Env...)
	// По таймауту контекст убивает сам CLI, а его потомки держат stdout: без WaitDelay вызов ждал бы их.
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err = cmd.Output()
	stderrText = strings.TrimSpace(stderr.String())
	if err != nil && ctx.Err() != nil {
		return out, stderrText, ctx.Err() // срыв по сроку важнее кода, с которым убитый CLI вышел
	}
	return out, stderrText, err
}

// cliError — сбой запуска CLI с его причиной, сокращённой до хвоста.
func cliError(name string, err error, why string) error {
	return fmt.Errorf("oneshot: %s: %w: %s", name, err, short(why))
}

// short — хвост сообщения не длиннее 300 рун.
func short(s string) string {
	r := []rune(s)
	if len(r) <= 300 {
		return s
	}
	return "…" + string(r[len(r)-300:])
}
