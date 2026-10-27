package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

type payload struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Name      string          `json:"name"`
	Content   json.RawMessage `json:"content"`
	Input     json.RawMessage `json:"input"`
	Arguments json.RawMessage `json:"arguments"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	// session_meta и turn_context
	ID               string `json:"id"`
	Cwd              string `json:"cwd"`
	Originator       string `json:"originator"`
	CreatorAccountID string `json:"creator_account_id"`
	Model            string `json:"model"`
	// event_msg: user_message, agent_message, token_count
	Message     string            `json:"message"`
	Images      []json.RawMessage `json:"images"`
	LocalImages []json.RawMessage `json:"local_images"`
	Info        *tokenInfo        `json:"info"`
	RateLimits  *rateLimits       `json:"rate_limits"`
}

type tokenInfo struct {
	Total         tokenUsage  `json:"total_token_usage"`
	Last          *tokenUsage `json:"last_token_usage"`
	ContextWindow int64       `json:"model_context_window"`
}

type tokenUsage struct {
	Input           int64 `json:"input_tokens"`
	CachedInput     int64 `json:"cached_input_tokens"`
	CacheWrite      int64 `json:"cache_write_input_tokens"`
	Output          int64 `json:"output_tokens"`
	ReasoningOutput int64 `json:"reasoning_output_tokens"`
	Total           int64 `json:"total_tokens"`
}

func (u tokenUsage) usage() TokenUsage {
	return TokenUsage{Input: u.Input, CachedInput: u.CachedInput, CacheWrite: u.CacheWrite, Output: u.Output,
		ReasoningOutput: u.ReasoningOutput, Total: u.Total}
}

type rateLimits struct {
	Primary   *rateWindow `json:"primary"`
	Secondary *rateWindow `json:"secondary"`
	PlanType  string      `json:"plan_type"`
}

type rateWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int64   `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

func (w *rateWindow) window() *RateWindow {
	if w == nil {
		return nil
	}
	return &RateWindow{UsedPercent: w.UsedPercent, WindowMinutes: w.WindowMinutes, ResetsAt: w.ResetsAt}
}

// codexParser помнит имена вызовов по call_id (вывод ссылается на вызов), сессию из session_meta (её id и папку
// получают события, папку меняет turn_context), модель хода и учётку последнего session_meta. eventUser и
// eventAgent — в файле есть события user_message и agent_message: реплики и текст агента берутся из них, а не из
// response_item. named — session_meta уже дал сессию, а событий после него ещё не было.
type codexParser struct {
	tools                                    map[string]string
	session, cwd, originator, account, model string
	// firstAccount — учётка первого session_meta для Meta.Account; account — последнего, для Tokens.Account.
	firstAccount          string
	eventUser, eventAgent bool
	named                 bool
}

func newCodexParser() *codexParser { return &codexParser{tools: map[string]string{}} }

// Обвязка codex подкладывает в начало беседы инструкции и окружение сообщениями пользователя — это не реплики;
// <turn_aborted> — её же вставка после прерванного хода.
var codexInjected = []string{"<environment_context>", "# AGENTS.md instructions", "<user_instructions>",
	"<permissions instructions>", "<turn_aborted>"}

// codexHead — сколько байт начала файла смотреть, чтобы узнать rollout codex: тип session_meta стоит в начале строки.
const codexHead = 256

// Строки событий user_message и agent_message: codex пишет их так, тип события — сразу после типа записи.
var (
	userMessageLine  = []byte(`"type":"event_msg","payload":{"type":"user_message"`)
	agentMessageLine = []byte(`"type":"event_msg","payload":{"type":"agent_message"`)
)

// codexMessages — есть ли в rollout события user_message и agent_message. Их пишет только TUI: в rollout exec
// реплики и текст агента есть лишь в response_item, а в TUI response_item дублирует их со вставками обвязки
// (<turn_aborted>, заглушки картинок) и метками на 1 мс позже. Файл читается по смещениям, его позиция не меняется.
func codexMessages(f *os.File) (user, agent bool, err error) {
	st, err := f.Stat()
	if err != nil {
		return false, false, fmt.Errorf("stat: %w", err)
	}
	r := bufio.NewReaderSize(io.NewSectionReader(f, 0, st.Size()), 1<<20)
	for !user || !agent {
		line, err := readLine(r)
		head := line[:min(len(line), codexHead)]
		user = user || bytes.Contains(head, userMessageLine)
		agent = agent || bytes.Contains(head, agentMessageLine)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, false, err
		}
	}
	return user, agent, nil
}

func (p *codexParser) parse(rec *record) []Event {
	events := p.events(rec)
	if len(events) > 0 {
		p.named = false
	}
	for i := range events {
		events[i].SessionID, events[i].Cwd = p.session, p.cwd
	}
	return events
}

func (p *codexParser) events(rec *record) []Event {
	pl := rec.Payload
	ev := func(role Role, who, text string) []Event {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []Event{{Role: role, Who: who, Text: text}}
	}
	switch rec.Type {
	case "session_meta":
		// rollout суб-агента повторяет сразу за своим session_meta родительский: это не новая сессия. session_meta
		// после событий — новая: копия rollout фермы (*.rollout.jsonl) начинает им срез следующей сессии, а
		// бывает, что он идёт после первых token_count (49 из 1997 rollout за 31 день по замеру).
		if pl.ID != "" && !p.named {
			p.session, p.named = pl.ID, true
			if pl.Cwd != "" {
				p.cwd = pl.Cwd
			}
		}
		if p.originator == "" {
			p.originator = pl.Originator
		}
		if pl.CreatorAccountID != "" {
			p.account = pl.CreatorAccountID
			if p.firstAccount == "" {
				p.firstAccount = pl.CreatorAccountID
			}
		}
		return nil
	case "turn_context":
		if pl.Cwd != "" {
			p.cwd = pl.Cwd
		}
		if pl.Model != "" {
			p.model = pl.Model
		}
		return nil
	case "compacted":
		return []Event{{Role: Note, Who: "compact", Text: "context compacted"}}
	case "event_msg":
		return p.eventMsg(pl, ev)
	}
	if rec.Type != "response_item" {
		return nil
	}
	return p.responseItem(pl, ev)
}

// responseItem — сообщения беседы (если файл не пишет их событиями), вызовы и их вывод.
func (p *codexParser) responseItem(pl *payload, ev func(Role, string, string) []Event) []Event {
	switch pl.Type {
	case "message":
		text, images, _ := content(pl.Content)
		withImages := func(es []Event) []Event {
			for i := range es {
				es[i].Images = images
			}
			return es
		}
		switch pl.Role {
		case "assistant":
			if p.eventAgent {
				return nil
			}
			return withImages(ev(Agent, "", text))
		case "user":
			if p.eventUser {
				return nil
			}
			for _, pre := range codexInjected {
				if strings.HasPrefix(strings.TrimSpace(text), pre) {
					return nil
				}
			}
			return withImages(ev(User, "", text))
		}
	case "custom_tool_call":
		p.tools[pl.CallID] = pl.Name
		var out []Event
		for _, c := range execCommands(str(pl.Input)) {
			out = append(out, ev(Cmd, pl.Name, c)...)
		}
		return call(out, pl.CallID, pl.Input)
	case "function_call":
		p.tools[pl.CallID] = pl.Name
		args := str(pl.Arguments)
		return call(ev(Cmd, pl.Name, argsCommand(args)), pl.CallID, argsJSON(args, pl.Arguments))
	case "custom_tool_call_output", "function_call_output":
		text, failed := outputText(pl.Output)
		// Вывод есть и пустой: exec с пустым выводом после шапки «Script completed» — тоже вывод вызова.
		out := call([]Event{{Role: Out, Who: p.tools[pl.CallID], Text: text}}, pl.CallID, pl.Output)
		for i := range out {
			out[i].Error = failed
		}
		return out
	}
	return nil
}

// eventMsg — события event_msg: реплики и текст агента (когда файл их пишет) и расход.
func (p *codexParser) eventMsg(pl *payload, ev func(Role, string, string) []Event) []Event {
	switch pl.Type {
	case "user_message":
		if !p.eventUser {
			return nil
		}
		images := len(pl.Images) + len(pl.LocalImages)
		if strings.TrimSpace(pl.Message) == "" && images > 0 {
			return []Event{{Role: User, Images: images}}
		}
		out := ev(User, "", pl.Message)
		for i := range out {
			out[i].Images = images
		}
		return out
	case "agent_message":
		if !p.eventAgent {
			return nil
		}
		return ev(Agent, "", pl.Message)
	case "token_count":
		if pl.Info == nil && pl.RateLimits == nil {
			return nil
		}
		t := &Tokens{Model: p.model, Account: p.account}
		if pl.Info != nil {
			t.Total, t.ContextWindow = pl.Info.Total.usage(), pl.Info.ContextWindow
			if pl.Info.Last != nil {
				last := pl.Info.Last.usage()
				t.Last = &last
			}
		}
		if l := pl.RateLimits; l != nil {
			t.Limits = &RateLimits{Primary: l.Primary.window(), Secondary: l.Secondary.window(), PlanType: l.PlanType}
		}
		return []Event{{Role: Note, Who: "token_count", Text: t.String(), Tokens: t}}
	}
	return nil
}

// String — расход строкой для показа.
func (t *Tokens) String() string {
	s := fmt.Sprintf("tokens total %d", t.Total.Total)
	if t.Last != nil {
		s += fmt.Sprintf(", last %d", t.Last.Total)
	}
	if t.Limits != nil && t.Limits.Primary != nil {
		s += fmt.Sprintf("; limit %.0f%% of %d min", t.Limits.Primary.UsedPercent, t.Limits.Primary.WindowMinutes)
	}
	return s
}

// call — id вызова и сырой JSON у событий вызова или вывода.
func call(events []Event, id string, raw json.RawMessage) []Event {
	for i := range events {
		events[i].CallID, events[i].Raw = id, raw
	}
	return events
}

// str — JSON-строка поля записи; не строка — пусто.
func str(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// argsJSON — аргументы function_call: сами аргументы, если это JSON, иначе поле записи как есть (JSON-строка).
func argsJSON(args string, field json.RawMessage) json.RawMessage {
	if json.Valid([]byte(args)) {
		return json.RawMessage(args)
	}
	return field
}

// cmdField — поле cmd в коде вызова exec (codex зовёт exec_command из JS): строка в двойных, одинарных или
// обратных кавычках.
var cmdField = regexp.MustCompile(`\bcmd\s*:\s*("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|` + "`(?:[^`\\\\]|\\\\.)*`" + `)`)

// execCommands достаёт команды из JS-кода вызова exec, по событию на команду; не нашлось ни одной непустой
// (exec_command({cmd:""}) — 4 из 67796 вызовов за 10 дней по замеру) — код как есть: вызов не пропадает.
// Строка в двойных кавычках — JSON-строка, в одинарных и обратных — как написана, без кавычек.
func execCommands(code string) []string {
	var cmds []string
	for _, m := range cmdField.FindAllStringSubmatch(code, -1) {
		s := m[1]
		if s[0] != '"' || json.Unmarshal([]byte(s), &s) != nil {
			s = s[1 : len(s)-1]
		}
		if s != "" {
			cmds = append(cmds, s)
		}
	}
	if len(cmds) == 0 {
		return []string{code}
	}
	return cmds
}

// argsCommand — команда из аргументов function_call (старый вид: exec_command, shell).
func argsCommand(args string) string {
	var a struct {
		Cmd     json.RawMessage `json:"cmd"`
		Command json.RawMessage `json:"command"`
	}
	if json.Unmarshal([]byte(args), &a) != nil {
		return args
	}
	for _, raw := range []json.RawMessage{a.Cmd, a.Command} {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		var list []string
		if json.Unmarshal(raw, &list) == nil {
			return strings.Join(list, " ")
		}
	}
	return args
}

// execHeader — шапка, которую exec codex ставит перед выводом команды.
var execHeader = regexp.MustCompile(`^Script (?:completed|failed)[^\n]*\n(?:Wall time [^\n]*\n)?(?:Output:\n)?\n?`)

// outputText — вывод вызова: строка, массив input_text или {"output": "…"}; шапка exec срезается. Failed —
// exec сообщил в шапке, что скрипт упал.
func outputText(raw json.RawMessage) (text string, failed bool) {
	if t, images, _ := content(raw); t != "" || images > 0 {
		return execHeader.ReplaceAllString(t, ""), strings.HasPrefix(t, "Script failed")
	}
	var o struct {
		Output string `json:"output"`
	}
	if json.Unmarshal(raw, &o) == nil && o.Output != "" {
		return o.Output, false
	}
	return string(raw), false
}
