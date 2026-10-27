// Package transcript читает транскрипт сессии агента как поток событий: реплики человека, ответы агента, вызовы
// инструментов и их вывод, письма других сессий. Понимает транскрипты Claude Code (~/.claude/projects/…/<id>.jsonl)
// и codex (rollout-*.jsonl, в том числе бойцов worker). Номер строки события — ссылка на подробности.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aiseeq/akit/price"
)

// Role — чьё событие.
type Role string

// Роли событий.
const (
	User   Role = "user"   // реплика человека (у codex — сообщение пользователя сессии, обычно бриф запустившей сессии)
	Ask    Role = "ask"    // ответ человека в окне вопроса AskUserQuestion
	Peer   Role = "peer"   // письмо другой сессии; Who — её имя
	Agent  Role = "agent"  // текст ответа агента
	Think  Role = "think"  // рассуждение агента (thinking); текст бывает пуст: обвязка его не сохраняет
	Cmd    Role = "cmd"    // вызов инструмента: команда Bash или имя инструмента с его вводом
	Out    Role = "out"    // вывод инструмента
	Attach Role = "attach" // вложение Claude Code в контекст (хук, диагностика, снимок…): Who — тип, Raw — целиком
	// Note — служебное: уведомления фоновых задач, сжатие контекста, бриф и запрос программы (Who «prompt»),
	// вставка обвязки (Who «meta»).
	Note Role = "note"
)

// Roles — все роли по порядку, для справки и разбора --role.
var Roles = []Role{User, Ask, Peer, Agent, Think, Cmd, Out, Attach, Note}

// Event — одно событие транскрипта. События одной записи (части одного ответа агента) делят Line и UUID.
type Event struct {
	Line int // номер строки в файле, с 1
	Time time.Time
	Role Role
	Who  string // для peer — имя отправителя, для cmd и out — инструмент
	From string // для peer — адрес отправителя (from, uds:…): SendMessage пишет в to имя или адрес
	// Text — слова: напоминания обвязки (<system-reminder>) из реплик человека и писем вырезаны, картинки — не текст,
	// их число в Images.
	Text   string
	Images int
	// Поля записи Claude Code; у codex UUID пуст, SessionID и Cwd — из session_meta, Cwd меняет turn_context.
	// Cwd — папка сессии в момент записи: меняется внутри сессии.
	UUID, SessionID, Cwd string
	// Sidechain — запись боковой ветки (суб-агент в транскрипте сессии, старый формат): не разговор сессии.
	Sidechain bool
	// PromptID — ход Claude Code: есть у событий записей user; ответы хода — после его записи user.
	PromptID string
	// CallID — вызов инструмента: у cmd — его id, у out — id вызова, чей это вывод. Error — вывод ошибки
	// (is_error; у codex — «Script failed»).
	CallID string
	Error  bool
	// Raw — JSON как в записи: у cmd — ввод инструмента, у out и ask — содержимое вывода, у attach и реплики из
	// очереди — вложение, у note из записи system — запись целиком.
	Raw json.RawMessage
	// Result — toolUseResult записи Claude Code у out и ask: подробности вывода (persistedOutputPath, ответы окна…).
	Result json.RawMessage
	// Subtype — у note из записи Claude Code type=system (Who «system») её subtype: compact_boundary,
	// informational («Compacted while idle…»), turn_duration, stop_hook_summary…
	Subtype string
	// Queued — реплика пришла из очереди (вложение queued_command): набрана, пока агент работал.
	Queued bool
	// Reply — ответ модели, к которому относится событие; есть у событий записи ответа Claude Code.
	Reply *Reply
	// Tokens — расход codex (событие token_count, note «token_count»).
	Tokens *Tokens
}

// Tokens — расход сессии codex на момент события: всего с начала и последнего запроса, лимит подписки.
type Tokens struct {
	Total TokenUsage
	// Last — расход последнего запроса; nil — last_token_usage в событии нет (не то же, что нули).
	Last          *TokenUsage
	ContextWindow int64
	// Model — модель хода (turn_context) на момент события.
	Model string
	// Account — учётка последнего session_meta до события (в копии фермы у срезов они разные).
	Account string
	// Limits — окна лимита подписки; nil — в событии их нет.
	Limits *RateLimits
}

// TokenUsage — токены codex как в token_count; ReasoningOutput входит в Output.
type TokenUsage struct {
	Input, CachedInput, CacheWrite, Output, ReasoningOutput, Total int64
}

// RateLimits — лимит подписки codex: основное и второе окно, тариф.
type RateLimits struct {
	Primary, Secondary *RateWindow
	PlanType           string
}

// RateWindow — окно лимита: занятая доля в процентах, длина окна и сброс (секунды Unix).
type RateWindow struct {
	UsedPercent   float64
	WindowMinutes int64
	ResetsAt      int64
}

// Reply — ответ модели. Claude Code пишет ответ по записи на часть, расход повторяется в каждой и растёт к
// последней: один ответ — одна пара ID и RequestID, расход — с наибольшим Output.
type Reply struct {
	ID, RequestID, Model, StopReason string
	Usage                            *price.Usage // nil — расхода в записи нет
}

// Shown — текст для показа: слова и «[image]» на каждую картинку.
func (e Event) Shown() string {
	if e.Images == 0 {
		return e.Text
	}
	marks := strings.TrimSpace(strings.Repeat("[image] ", e.Images))
	if e.Text == "" {
		return marks
	}
	return e.Text + "\n" + marks
}

// Meta — что известно о файле целиком.
type Meta struct {
	Name string // имя сессии (agent-name у Claude Code), может быть пустым
	// Originator — кто вёл сессию codex (session_meta): codex-tui — человек в терминале, codex_exec — программа.
	Originator string
	// Account — учётка codex (creator_account_id первого session_meta).
	Account string
	Bad     int // строк, которые не разобрать: не JSON или кривая метка времени
	// FirstBad — ошибка первой из них, с номером строки.
	FirstBad error
}

// maxLine — потолок длины строки транскрипта: запись с вложениями бывает в мегабайты.
const maxLine = 256 << 20

// Read читает файл и отдаёт события по порядку. keep, если задан, получает сырую строку и может отбросить её до
// разбора JSON: так поиск по гигабайтам не разбирает лишнее. Испорченная строка (файл дописывается прямо сейчас)
// пропускается и считается в Meta.Bad.
func Read(path string, keep func(raw []byte) bool, fn func(Event)) (Meta, error) {
	f, err := os.Open(path)
	if err != nil {
		return Meta{}, fmt.Errorf("transcript: %w", err)
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	rd := reader{claude: newClaudeParser(), codex: newCodexParser(), keep: keep, fn: fn}
	head, err := r.Peek(codexHead)
	if err != nil && !errors.Is(err, io.EOF) { // EOF — файл короче codexHead
		return Meta{}, fmt.Errorf("transcript %s: %w", path, err)
	}
	if bytes.Contains(head, []byte(`"type":"session_meta"`)) {
		rd.codex.eventUser, rd.codex.eventAgent, err = codexMessages(f)
		if err != nil {
			return Meta{}, fmt.Errorf("transcript %s: %w", path, err)
		}
	}
	for {
		raw, err := readLine(r)
		if len(raw) > 0 {
			rd.line(raw)
		}
		if err != nil {
			rd.meta.Originator, rd.meta.Account = rd.codex.originator, rd.codex.firstAccount
			if errors.Is(err, io.EOF) {
				return rd.meta, nil
			}
			return rd.meta, fmt.Errorf("transcript %s: %w", path, err)
		}
	}
}

// toolResult — часть с выводом инструмента в записи Claude Code. Без экранирования эти байты бывают только в
// структуре записи: в тексте кавычки экранированы.
var toolResult = []byte(`"type":"tool_result"`)

// WithoutResults — keep для Read: отбрасывает до разбора записи с выводом инструментов (события out и ask). Счёту
// расхода и ходов они не нужны, а это большая часть байт транскрипта: на 150 самых больших файлах чтение
// быстрее в 2,3 раза.
func WithoutResults(raw []byte) bool { return !bytes.Contains(raw, toolResult) }

// reader — состояние чтения одного файла.
type reader struct {
	meta   Meta
	claude *claudeParser
	codex  *codexParser
	keep   func([]byte) bool
	fn     func(Event)
	lineNo int
}

func (rd *reader) line(raw []byte) {
	rd.lineNo++
	if name, ok := agentName(raw); ok {
		rd.meta.Name = name
		return
	}
	if skipLine(raw) || (rd.keep != nil && !rd.keep(raw)) {
		return
	}
	events, err := parseLine(raw, rd.claude, rd.codex)
	if err != nil {
		rd.meta.Bad++
		if rd.meta.FirstBad == nil {
			rd.meta.FirstBad = fmt.Errorf("line %d: %w", rd.lineNo, err)
		}
		return
	}
	for i := range events {
		events[i].Line = rd.lineNo
		rd.fn(events[i])
	}
}

func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			return nil, fmt.Errorf("line longer than %d bytes", maxLine)
		}
		if err != nil || !isPrefix {
			return buf, err
		}
	}
}

// skipLine — служебные записи Claude Code, в которых нет событий: их тип стоит в начале строки.
func skipLine(raw []byte) bool {
	rest, ok := bytes.CutPrefix(raw, []byte(`{"type":"`))
	if !ok {
		return false
	}
	for _, t := range []string{"api-request", "permission-mode", "mode", "custom-title", "atis-latch", "last-prompt",
		"queue-operation", "file-history-snapshot", "summary"} {
		if bytes.HasPrefix(rest, []byte(t+`"`)) || bytes.HasPrefix(rest, []byte(t+"-")) {
			return true
		}
	}
	return false
}

// agentName узнаёт запись {"type":"agent-name","agentName":"…"} без разбора JSON.
func agentName(raw []byte) (string, bool) {
	rest, ok := bytes.CutPrefix(raw, []byte(`{"type":"agent-name","agentName":"`))
	if !ok {
		return "", false
	}
	name, _, ok := bytes.Cut(rest, []byte(`"`))
	return string(name), ok
}

// Short — текст в одну строку не длиннее width рун; переводы строк — «⏎», отрезанное — «…(+N)».
func Short(text string, width int) string {
	text = strings.TrimSpace(text)
	text = strings.Join(strings.Fields(strings.ReplaceAll(text, "\n", " ⏎ ")), " ")
	if width <= 0 {
		return text
	}
	r := []rune(text)
	if len(r) <= width {
		return text
	}
	return string(r[:width]) + fmt.Sprintf("…(+%d)", len(r)-width)
}

// Parser разбирает транскрипт по строкам для того, кто читает файл сам (например, только дописанное с
// запомненного смещения). Между вызовами он помнит имена инструментов по id вызова: out и ask узнаются, если
// запись вызова прошла через тот же Parser. Line событий не ставит. У codex реплики и текст агента берутся из
// response_item: какие события есть в файле (codexMessages), Parser не видит. Один Parser — один файл.
type Parser struct {
	claude *claudeParser
	codex  *codexParser
}

// NewParser — разбор одного файла с начала или с любой записи.
func NewParser() *Parser { return &Parser{claude: newClaudeParser(), codex: newCodexParser()} }

// Parse — события одной строки (без перевода строки). Служебная запись без событий — (nil, nil); ошибка —
// строка не JSON (запись дописывается прямо сейчас) или кривая метка времени.
func (p *Parser) Parse(raw []byte) ([]Event, error) {
	if _, ok := agentName(raw); ok || skipLine(raw) {
		return nil, nil
	}
	return parseLine(raw, p.claude, p.codex)
}
