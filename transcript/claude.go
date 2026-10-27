package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aiseeq/akit/price"
)

// record — запись транскрипта: поля Claude Code и codex вместе, по полю payload видно, чья.
type record struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	Timestamp        string          `json:"timestamp"`
	UUID             string          `json:"uuid"`
	SessionID        string          `json:"sessionId"`
	Cwd              string          `json:"cwd"`
	IsSidechain      bool            `json:"isSidechain"`
	PromptID         string          `json:"promptId"`
	RequestID        string          `json:"requestId"`
	Origin           *origin         `json:"origin"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	IsMeta           bool            `json:"isMeta"`
	Message          *message        `json:"message"`
	Content          json.RawMessage `json:"content"` // текст записи system
	Attachment       json.RawMessage `json:"attachment"`
	ToolUseResult    json.RawMessage `json:"toolUseResult"`
	Payload          *payload        `json:"payload"`
}

type origin struct {
	Kind string `json:"kind"`
}

type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ID         string          `json:"id"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Usage      *usage          `json:"usage"`
}

// usage — расход ответа, как его пишет Claude Code.
type usage struct {
	Input         int64 `json:"input_tokens"`
	Output        int64 `json:"output_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
	Split         *struct {
		Write5m int64 `json:"ephemeral_5m_input_tokens"`
		Write1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	Speed string `json:"speed"`
	Geo   string `json:"inference_geo"`
}

func (u *usage) priced() *price.Usage {
	if u == nil {
		return nil
	}
	p := &price.Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheCreation,
		Fast: u.Speed == "fast", Geo: u.Geo}
	if u.Split != nil {
		p.Write5m, p.Write1h = u.Split.Write5m, u.Split.Write1h
	}
	return p
}

// attachment — вложение: тип, реплика из очереди и поля, в которых бывает его сообщение.
type attachment struct {
	Type     string          `json:"type"`
	Prompt   json.RawMessage `json:"prompt"`
	Origin   *origin         `json:"origin"`
	Content  json.RawMessage `json:"content"`
	Text     json.RawMessage `json:"text"`
	Stdout   json.RawMessage `json:"stdout"`
	Snippet  json.RawMessage `json:"snippet"`
	Filename json.RawMessage `json:"filename"`
	Path     json.RawMessage `json:"path"`
}

type part struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// claudeParser помнит имена вызванных инструментов: вывод ссылается на вызов по id.
type claudeParser struct {
	tools map[string]string
}

func newClaudeParser() *claudeParser { return &claudeParser{tools: map[string]string{}} }

// parseLine — события строки с меткой времени записи. Ошибка — строка не JSON (файл дописывается прямо сейчас
// или испорчен) или у записи с событиями кривая метка.
func parseLine(raw []byte, claude *claudeParser, codex *codexParser) ([]Event, error) {
	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("not a JSON record: %w", err)
	}
	var events []Event
	if rec.Payload != nil {
		events = codex.parse(&rec)
	} else {
		events = claude.parse(&rec, raw)
		for i := range events {
			events[i].UUID, events[i].SessionID, events[i].Cwd = rec.UUID, rec.SessionID, rec.Cwd
			events[i].Sidechain, events[i].PromptID = rec.IsSidechain, rec.PromptID
		}
	}
	if len(events) == 0 {
		return nil, nil // у служебных записей метки может не быть, и она не нужна
	}
	t, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("record time: %w", err)
	}
	for i := range events {
		events[i].Time = t
	}
	return events, nil
}

func (p *claudeParser) parse(rec *record, raw []byte) []Event {
	ev := func(role Role, who, text string) Event { return Event{Role: role, Who: who, Text: text} }
	switch rec.Type {
	case "assistant":
		return p.assistant(rec, ev)
	case "user":
		return p.user(rec, ev)
	case "attachment":
		return attached(rec.Attachment, ev)
	case "system":
		// сжатие (compact_boundary и его compactMetadata), «Compacted while idle» (informational), длительность
		// хода, итог хуков: подробности — в записи целиком
		e := ev(Note, "system", str(rec.Content))
		e.Subtype, e.Raw = rec.Subtype, bytes.Clone(raw)
		return []Event{e}
	}
	return nil
}

// attached — вложение: письмо или реплика из очереди — по источнику, остальное — событие attach с вложением целиком.
func attached(raw json.RawMessage, ev func(Role, string, string) Event) []Event {
	var a attachment
	if len(raw) == 0 || json.Unmarshal(raw, &a) != nil || a.Type == "" {
		return nil
	}
	if a.Type == "queued_command" {
		kind := ""
		if a.Origin != nil {
			kind = a.Origin.Kind
		}
		out := fromOrigin(kind, a.Prompt, ev)
		for i := range out {
			out[i].Queued, out[i].Raw = true, raw
		}
		return out
	}
	e := ev(Attach, a.Type, a.text())
	e.Raw = raw
	return []Event{e}
}

// text — сообщение вложения: первое непустое строковое поле из известных.
func (a *attachment) text() string {
	for _, f := range []json.RawMessage{a.Content, a.Text, a.Stdout, a.Snippet, a.Prompt, a.Filename, a.Path} {
		if s := str(f); s != "" {
			return s
		}
	}
	return ""
}

// assistant — части ответа модели. Каждая запись с расходом даёт хотя бы одно событие: расход считают по записям.
func (p *claudeParser) assistant(rec *record, ev func(Role, string, string) Event) []Event {
	m := rec.Message
	if m == nil {
		return nil
	}
	var out []Event
	ps := parts(m.Content)
	for i := range ps {
		pt := &ps[i]
		switch pt.Type {
		case "text":
			if strings.TrimSpace(pt.Text) != "" {
				out = append(out, ev(Agent, "", pt.Text))
			}
		case "thinking":
			out = append(out, ev(Think, "", pt.Thinking))
		case "tool_use":
			p.tools[pt.ID] = pt.Name
			e := ev(Cmd, pt.Name, toolInput(pt.Name, pt.Input))
			e.CallID, e.Raw = pt.ID, pt.Input
			out = append(out, e)
		}
	}
	if len(out) == 0 && m.Usage != nil {
		out = append(out, ev(Note, "reply", "response without text"))
	}
	reply := &Reply{ID: m.ID, RequestID: rec.RequestID, Model: m.Model, StopReason: m.StopReason, Usage: m.Usage.priced()}
	for i := range out {
		out[i].Reply = reply
	}
	return out
}

func (p *claudeParser) user(rec *record, ev func(Role, string, string) Event) []Event {
	if rec.Message == nil {
		return nil
	}
	if rec.IsCompactSummary {
		return []Event{ev(Note, "compact", "context compacted")}
	}
	if rec.Origin != nil && rec.Origin.Kind != "" {
		return fromOrigin(rec.Origin.Kind, rec.Message.Content, ev)
	}
	text, images, ps := content(rec.Message.Content)
	if !hasResult(ps) {
		return prompt(rec, text, images, ev)
	}
	var out []Event
	for i := range ps {
		pt := &ps[i]
		if pt.Type != "tool_result" {
			continue
		}
		name := p.tools[pt.ToolUseID]
		if name == "AskUserQuestion" {
			if answers := askAnswers(rec.ToolUseResult); answers != "" {
				e := ev(Ask, "", answers)
				e.CallID, e.Error, e.Raw, e.Result = pt.ToolUseID, pt.IsError, pt.Content, rec.ToolUseResult
				out = append(out, e)
				continue
			}
		}
		text, images, _ := content(pt.Content)
		e := ev(Out, name, text)
		e.Images = images
		e.CallID, e.Error, e.Raw, e.Result = pt.ToolUseID, pt.IsError, pt.Content, rec.ToolUseResult
		out = append(out, e)
	}
	return out
}

// prompt — запись user без источника и без вывода инструментов: бриф суб-агента, запрос программы (claude -p),
// команда «/…» или вставка обвязки (isMeta). Набирал её не человек в окне сессии — это note, но ход (promptId)
// она начинает, поэтому напоминания в ней не вырезаются: запись из одного напоминания — тоже начало хода.
func prompt(rec *record, text string, images int, ev func(Role, string, string) Event) []Event {
	text = strings.TrimSpace(text)
	if text == "" && images == 0 {
		return nil
	}
	who := "prompt"
	if rec.IsMeta {
		who = "meta"
	}
	e := ev(Note, who, text)
	e.Images = images
	return []Event{e}
}

// Письмо приходит строкой «Another Claude session sent a message:» и тегом с именем отправителя; в очереди — без строки.
var fromName = regexp.MustCompile(`^\s*(?:Another Claude session sent a message:\s*)?<cross-session-message[^>]*?from-name="([^"]*)"[^>]*>\n?`)

// fromAddr — адрес отправителя в теге письма.
var fromAddr = regexp.MustCompile(`<cross-session-message[^>]*?\sfrom="([^"]*)"`)

// reminder — напоминание обвязки Claude Code внутри реплики: его никто не набирал.
var reminder = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// fromOrigin — событие по источнику реплики: человек, другая сессия или уведомление. Реплика человека, в которой
// после напоминаний не осталось ни слов, ни картинок, — не реплика.
func fromOrigin(kind string, raw json.RawMessage, ev func(Role, string, string) Event) []Event {
	text, images, _ := content(raw)
	var e Event
	switch kind {
	case "human":
		text = strings.TrimSpace(reminder.ReplaceAllString(text, ""))
		if text == "" && images == 0 {
			return nil
		}
		e = ev(User, "", text)
	case "peer":
		text = strings.TrimSpace(reminder.ReplaceAllString(text, ""))
		who, from := "", ""
		if m := fromName.FindStringSubmatchIndex(text); m != nil {
			who = text[m[2]:m[3]]
			if a := fromAddr.FindStringSubmatch(text[:m[1]]); a != nil {
				from = a[1]
			}
			text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text[m[1]:]), "</cross-session-message>"))
		}
		e = ev(Peer, who, text)
		e.From = from
	default:
		e = ev(Note, kind, text)
	}
	e.Images = images
	return []Event{e}
}

// askAnswers — ответы окна вопроса: «ответ ← вопрос» по строке на вопрос; ответ первым, чтобы его не отрезала
// короткая строка.
func askAnswers(raw json.RawMessage) string {
	var r struct {
		Answers map[string]string `json:"answers"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &r) != nil || len(r.Answers) == 0 {
		return ""
	}
	qs := make([]string, 0, len(r.Answers))
	for q := range r.Answers {
		qs = append(qs, q)
	}
	sort.Strings(qs)
	lines := make([]string, 0, len(qs))
	for _, q := range qs {
		lines = append(lines, r.Answers[q]+" ← "+q)
	}
	return strings.Join(lines, "\n")
}

// toolInput — ввод инструмента для показа: команда Bash как есть, письмо — «→ кому: текст», иначе JSON.
func toolInput(name string, raw json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return string(raw)
	}
	str := func(k string) string { s, _ := in[k].(string); return s }
	switch name {
	case "Bash":
		return str("command")
	case "SendMessage":
		return "→ " + str("to") + ": " + str("message")
	case "Read", "Write", "Edit":
		return str("file_path")
	}
	return string(raw)
}

func parts(raw json.RawMessage) []part {
	var ps []part
	if json.Unmarshal(raw, &ps) != nil {
		return nil
	}
	return ps
}

func hasResult(ps []part) bool {
	for i := range ps {
		if ps[i].Type == "tool_result" {
			return true
		}
	}
	return false
}

// content — содержимое, разобранное один раз: текст (строка как есть, у массива — текстовые части через перевод
// строки), число картинок и части массива.
func content(raw json.RawMessage) (text string, images int, ps []part) {
	if len(raw) > 0 && raw[0] == '"' {
		if json.Unmarshal(raw, &text) == nil {
			return text, 0, nil
		}
	}
	ps = parts(raw)
	var texts []string
	for i := range ps {
		switch ps[i].Type {
		case "text", "input_text", "output_text":
			texts = append(texts, ps[i].Text)
		case "image", "input_image":
			images++
		}
	}
	return strings.Join(texts, "\n"), images, ps
}
