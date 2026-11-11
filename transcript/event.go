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
	"slices"
	"strings"
	"time"
	"unicode/utf8"

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
	Text string
	// WrapperText — отдельные текстовые части JS-обвязки после вывода r.output.
	WrapperText    string
	HasWrapperText bool
	// Answer — только ответы человека в AskUserQuestion, без текста вопросов.
	Answer string
	Images int
	// Поля записи Claude Code; у codex UUID пуст, SessionID и Cwd — из session_meta, Cwd меняет turn_context.
	// Cwd — папка сессии в момент записи: меняется внутри сессии.
	UUID, SessionID, Cwd string
	// MessageID — message.id assistant-записи Claude Code.
	MessageID string
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

const imageMarker = "[image]"

// Shown — текст для показа: слова и «[image]» на каждую картинку.
func (e Event) Shown() string {
	if e.Images == 0 {
		return e.Text
	}
	marks := strings.TrimSpace(strings.Repeat(imageMarker+" ", e.Images))
	if e.Text == "" {
		return marks
	}
	return e.Text + "\n" + marks
}

// SyntheticText перечисляет фрагменты, создаваемые обвязкой при построении событий.
func SyntheticText() string {
	return imageMarker + " " + promptWho + " " + metaWho + " " + compactWho + " " + compactText + " " +
		replyWho + " " + emptyReply + " " + systemWho + " " + tokenTotalFormat + tokenLastFormat + tokenLimitFormat
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
	return read(path, keep, nil, fn, nil)
}

// ToolNames даёт фильтру известные и возможные имена инструментов из предыдущих строк.
// Возможные имена допускают лишние совпадения; набор действует только во время вызова фильтра.
type ToolNames struct {
	names     map[string]struct{}
	plainType *bool
	noUnicode *bool
}

// NoUnicodeEscapes сообщает доказанный фильтром факт: в строке нет JSON-экранирования Unicode.
func (n ToolNames) NoUnicodeEscapes() {
	if n.noUnicode != nil {
		*n.noUnicode = true
	}
}

// PlainType сообщает доказанный фильтром факт: экранирование не может скрывать тип события.
func (n ToolNames) PlainType() {
	if n.plainType != nil {
		*n.plainType = true
	}
}

// AddPossible добавляет возможное имя для консервативного отбора.
func (n *ToolNames) AddPossible(name string) {
	if n.names == nil {
		n.names = make(map[string]struct{})
	}
	n.names[name] = struct{}{}
}

// Any сообщает, есть ли запомненное имя, для которого выполняется условие.
func (n ToolNames) Any(match func(string) bool) bool {
	for name := range n.names {
		if match(name) {
			return true
		}
	}
	return false
}

// RemovesReminders сообщает, вырезает ли разбор этой записи напоминания из её текста.
func RemovesReminders(raw []byte) bool {
	claude := newClaudeParser()
	_, codex, err := parseLineKind(raw, claude, newCodexParser())
	return err == nil && !codex && claude.remindersRemoved
}

// ReadWithTools читает как Read, передавая фильтру также имена инструментов из предыдущих строк.
// Если фильтр отбросил вызов Claude Code, его имена всё равно сохраняются для последующих результатов.
func ReadWithTools(path string, keep func([]byte, ToolNames) bool, fn func(Event)) (Meta, error) {
	return read(path, nil, keep, fn, nil)
}

// ReadWithCodexCommandsDiscardOutput reads command completions without retaining their output.
func ReadWithCodexCommandsDiscardOutput(path string, fn func(Event), commandFn func(CodexCommandResult)) (Meta, error) {
	return read(path, nil, nil, fn, commandFn)
}

func read(path string, keep func([]byte) bool, keepWithTools func([]byte, ToolNames) bool, fn func(Event), commandFn func(CodexCommandResult)) (Meta, error) {
	f, err := os.Open(path)
	if err != nil {
		return Meta{}, fmt.Errorf("transcript: %w", err)
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	rd := reader{claude: newClaudeParser(), codex: newCodexParser(), keep: keep, keepWithTools: keepWithTools, fn: fn, commandFn: commandFn}
	if keepWithTools != nil {
		rd.toolNames = make(map[string]struct{})
	}
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
		var raw []byte
		var err error
		if keepWithTools == nil {
			raw, err = readLine(r)
		} else {
			raw, err = readLineView(r)
		}
		if len(raw) > 0 {
			rd.line(raw)
		} else if err == nil {
			rd.lineNo++ // a blank line is still a line of the file: numbers match sed -n
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
	meta          Meta
	claude        *claudeParser
	codex         *codexParser
	keep          func([]byte) bool
	keepWithTools func([]byte, ToolNames) bool
	toolNames     map[string]struct{}
	plainType     bool
	noUnicode     bool
	pending       []pendingCall
	pendingByID   map[string][]int
	indexed       int
	pendingBytes  int
	fn            func(Event)
	commandFn     func(CodexCommandResult)
	lineNo        int
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
	rd.plainType, rd.noUnicode = false, false
	if rd.keepWithTools != nil && !rd.keepWithTools(raw, ToolNames{rd.toolNames, &rd.plainType, &rd.noUnicode}) {
		rd.rememberCodex(raw)
		rd.deferCall(raw, rd.plainType, rd.noUnicode)
		return
	}
	var rec record
	parsed := false
	if len(rd.pending) > 0 {
		if MayHideType(raw) {
			rd.flushCalls()
		} else if bytes.Contains(raw, []byte(`"tool_result"`)) && json.Unmarshal(raw, &rec) == nil {
			rd.resolveCalls(&rec)
			parsed = true
		}
	}
	var events []Event
	var codex bool
	var err error
	if parsed {
		events, codex, err = parseRecordKind(&rec, raw, rd.claude, rd.codex)
	} else if rd.commandFn != nil {
		if err = json.Unmarshal(raw, &rec); err != nil {
			err = fmt.Errorf("not a JSON record: %w", err)
		} else {
			events, codex, err = parseRecordKind(&rec, raw, rd.claude, rd.codex)
		}
	} else {
		events, codex, err = parseLineKind(raw, rd.claude, rd.codex)
	}
	if err != nil {
		rd.badLine(raw, codex, err)
		return
	}
	if rd.commandFn != nil {
		if result, ok := codexCommandResultFromPayload(rec.Type, rec.Payload, rd.lineNo); ok {
			rd.commandFn(result)
		}
	}
	rd.emitParsed(raw, codex, events)
}

// Отсев не должен терять состояние Codex: вызовы, контекст и отметку первого события после session_meta.
func (rd *reader) rememberCodex(raw []byte) {
	if !containsPayload(raw) && (rd.noUnicode || !MayHideType(raw)) {
		return
	}
	if !rd.codex.named && !bytes.Contains(raw, []byte(`"session_meta"`)) &&
		!bytes.Contains(raw, []byte(`"turn_context"`)) && !bytes.Contains(raw, []byte(`_call`)) &&
		(rd.noUnicode || !MayHideType(raw)) {
		return
	}
	var rec record
	if json.Unmarshal(raw, &rec) != nil || rec.Payload == nil {
		return
	}
	rd.codex.parse(&rec)
	for _, name := range rd.codex.tools {
		rd.toolNames[name] = struct{}{}
	}
}

// containsPayload узнаёт имя поля с любым регистром, принимаемым encoding/json.
func containsPayload(raw []byte) bool {
	for offset := 0; offset < len(raw); {
		n := bytes.IndexByte(raw[offset:], '"')
		if n < 0 {
			return false
		}
		i := offset + n
		if i+9 <= len(raw) && (raw[i+1] == 'p' || raw[i+1] == 'P') && bytes.EqualFold(raw[i:i+9], []byte(`"payload"`)) {
			return true
		}
		offset = i + 1
	}
	return false
}

func (rd *reader) badLine(raw []byte, codex bool, err error) {
	if len(rd.pending) > 0 {
		rd.flushCalls()
		if !codex {
			rd.rememberTools(raw)
		}
	}
	if rd.toolNames != nil {
		rd.collectToolNames()
	}
	rd.meta.Bad++
	if rd.meta.FirstBad == nil {
		rd.meta.FirstBad = fmt.Errorf("line %d: %w", rd.lineNo, err)
	}
}

func (rd *reader) emitParsed(raw []byte, codex bool, events []Event) {
	hasCall := false
	var changes []savedCall
	for i := range events {
		if rd.toolNames != nil && events[i].Role == Cmd {
			rd.toolNames[events[i].Who] = struct{}{}
			hasCall = true
			if !codex && len(rd.pending) > 0 {
				changes = append(changes, savedCall{events[i].CallID, events[i].Who})
			}
		}
		events[i].Line = rd.lineNo
		rd.fn(events[i])
	}
	if len(changes) > 0 {
		rd.pending = append(rd.pending, pendingCall{changes: changes, decoded: true})
		if len(rd.pending) > 32768 {
			rd.flushCalls()
		}
	}
	// Вызов Codex с пустой командой запоминает имя, но не создаёт событие Cmd.
	if rd.toolNames != nil && !hasCall && codex && (MayHideType(raw) ||
		bytes.Contains(raw, []byte(`"function_call"`)) || bytes.Contains(raw, []byte(`"custom_tool_call"`))) {
		rd.collectToolNames()
	}
}

func (rd *reader) collectToolNames() {
	for _, names := range []map[string]string{rd.claude.tools, rd.codex.tools} {
		for _, name := range names {
			rd.toolNames[name] = struct{}{}
		}
	}
}

type savedCall struct{ id, name string }
type pendingCall struct {
	raw       []byte
	changes   []savedCall
	decoded   bool
	noUnicode bool
}

// Отброшенные вызовы индексируются по возможным id до подходящего результата. Уже разобранные вызовы
// записываются как изменения, чтобы повторное применение сохраняло порядок при повторных id.
func (rd *reader) deferCall(raw []byte, plainType, noUnicode bool) {
	if !bytes.Contains(raw, []byte(`"tool_use"`)) && (plainType || !MayHideType(raw)) {
		return
	}
	if len(raw) > 64<<20 {
		rd.flushCalls()
		rd.rememberTools(raw)
		return
	}
	rd.pending = append(rd.pending, pendingCall{raw: bytes.Clone(raw), noUnicode: noUnicode})
	rd.pendingBytes += len(raw)
	if rd.pendingBytes > 64<<20 || len(rd.pending) > 32768 {
		rd.flushCalls()
	}
}

// indexCalls откладывает поиск id до первого результата; вызовы после последнего результата не разбираются.
func (rd *reader) indexCalls() bool {
	if rd.pendingByID == nil {
		rd.pendingByID = make(map[string][]int)
	}
	for rd.indexed < len(rd.pending) {
		index := rd.indexed
		call := &rd.pending[index]
		var ids []string
		if call.decoded {
			for _, change := range call.changes {
				ids = append(ids, change.id)
			}
		} else {
			if !call.noUnicode && MayHideType(call.raw) {
				rd.flushCalls()
				return false
			}
			var ok bool
			ids, ok = fastIDs(call.raw)
			if !ok {
				rd.flushCalls()
				return false
			}
		}
		for _, id := range ids {
			if id != "" {
				rd.pendingByID[id] = append(rd.pendingByID[id], index)
			}
		}
		rd.indexed++
	}
	return true
}

func (rd *reader) decodeCall(index int) {
	call := &rd.pending[index]
	if call.decoded {
		return
	}
	tmp := reader{claude: newClaudeParser(), toolNames: make(map[string]struct{})}
	tmp.rememberTools(call.raw)
	for id, name := range tmp.claude.tools {
		call.changes = append(call.changes, savedCall{id, name})
	}
	for name := range tmp.toolNames {
		rd.toolNames[name] = struct{}{}
	}
	rd.pendingBytes -= len(call.raw)
	call.raw = nil
	call.decoded = true
}

func (rd *reader) resolveCalls(rec *record) {
	ids, ok := resultIDs(rec)
	if !ok || len(ids) == 0 {
		rd.flushCalls()
		return
	}
	if slices.Contains(ids, "") {
		rd.flushCalls()
		return
	}
	if !rd.indexCalls() {
		return
	}
	for _, id := range ids {
		for _, index := range rd.pendingByID[id] {
			rd.decodeCall(index)
			for _, change := range rd.pending[index].changes {
				if change.id == id {
					rd.claude.tools[id] = change.name
				}
			}
		}
		delete(rd.pendingByID, id)
	}
}

func (rd *reader) flushCalls() {
	for i := range rd.pending {
		rd.decodeCall(i)
		for _, change := range rd.pending[i].changes {
			rd.claude.tools[change.id] = change.name
			rd.toolNames[change.name] = struct{}{}
		}
	}
	rd.pending = nil
	rd.pendingByID = nil
	rd.indexed = 0
	rd.pendingBytes = 0
}

// fastIDs допускает лишние значения id из вложенных полей; экранированные имена полей идут полным разбором.
func fastIDs(raw []byte) ([]string, bool) {
	var ids []string
	for _, key := range []string{`"id"`, `"iD"`, `"Id"`, `"ID"`} {
		for offset := 0; offset < len(raw); {
			n := bytes.Index(raw[offset:], []byte(key))
			if n < 0 {
				break
			}
			offset += n + len(key)
			id, found, ok := fastIDValue(raw, offset)
			if !ok {
				return nil, false
			}
			if found {
				ids = append(ids, id)
			}
		}
	}
	return ids, true
}

func fastIDValue(raw []byte, pos int) (string, bool, bool) {
	pos = jsonSpace(raw, pos)
	if pos == len(raw) || raw[pos] != ':' {
		return "", false, true
	}
	pos = jsonSpace(raw, pos+1)
	if pos == len(raw) {
		return "", false, false
	}
	if raw[pos] != '"' {
		return "", false, true
	}
	end := jsonStringEnd(raw, pos)
	if end == 0 {
		return "", false, false
	}
	if bytes.IndexByte(raw[pos+1:end-1], '\\') < 0 && utf8.Valid(raw[pos+1:end-1]) {
		return string(raw[pos+1 : end-1]), true, true
	}
	var id string
	if json.Unmarshal(raw[pos:end], &id) != nil {
		return "", false, false
	}
	return id, true, true
}

func jsonSpace(raw []byte, pos int) int {
	for pos < len(raw) && (raw[pos] == ' ' || raw[pos] == '\t' || raw[pos] == '\r' || raw[pos] == '\n') {
		pos++
	}
	return pos
}

// resultIDs извлекает id именно частей tool_result; отсутствие id означает пустое имя вызова.
func resultIDs(rec *record) ([]string, bool) {
	if rec.Message == nil || len(rec.Message.Content) == 0 || rec.Message.Content[0] != '[' {
		return nil, false
	}
	rec.decodedParts = parts(rec.Message.Content)
	rec.partsReady = true
	var ids []string
	for i := range rec.decodedParts {
		part := &rec.decodedParts[i]
		if part.Type == "tool_result" {
			ids = append(ids, part.ToolUseID)
		}
	}
	return ids, len(ids) > 0
}

func jsonStringEnd(raw []byte, start int) int {
	if start < 0 || start >= len(raw) || raw[start] != '"' {
		return 0
	}
	for i := start + 1; i < len(raw); {
		n := bytes.IndexByte(raw[i:], '"')
		if n < 0 {
			return 0
		}
		end := i + n
		backslashes := 0
		for j := end - 1; j > start && raw[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return end + 1
		}
		i = end + 1
	}
	return 0
}

// MayHideType замечает JSON-экранирование ASCII-символов, которые могут входить в имена полей и типов.
func MayHideType(raw []byte) bool {
	for offset := 0; offset < len(raw); {
		n := bytes.Index(raw[offset:], []byte(`\u`))
		if n < 0 {
			return false
		}
		i := offset + n
		if i+6 > len(raw) || escapedTypeRune(raw[i+2:i+6]) {
			return true
		}
		offset = i + 6
	}
	return false
}

func escapedTypeRune(encoded []byte) bool {
	var value rune
	for _, b := range encoded {
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value += rune(b - '0')
		case b >= 'a' && b <= 'f':
			value += rune(b - 'a' + 10)
		case b >= 'A' && b <= 'F':
			value += rune(b - 'A' + 10)
		default:
			return true
		}
	}
	return value == '_' || value >= '0' && value <= '9' ||
		value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

// rememberTools использует тот же разбор, что и чтение событий, включая повторные поля JSON.
func (rd *reader) rememberTools(raw []byte) {
	if !bytes.Contains(raw, []byte(`"tool_use"`)) && !MayHideType(raw) {
		return
	}
	var rec record
	if json.Unmarshal(raw, &rec) != nil || rec.Payload != nil {
		return
	}
	rd.claude.parse(&rec, raw)
	for _, name := range rd.claude.tools {
		rd.toolNames[name] = struct{}{}
	}
}

// readLineView использует буфер читателя для короткой строки; длинную собирает как readLine.
// Полученный срез действует до следующего чтения.
func readLineView(r *bufio.Reader) ([]byte, error) {
	chunk, prefix, err := r.ReadLine()
	if !prefix || err != nil {
		return chunk, err
	}
	buf := bytes.Clone(chunk)
	for {
		chunk, prefix, err = r.ReadLine()
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			return nil, fmt.Errorf("line longer than %d bytes", maxLine)
		}
		if err != nil || !prefix {
			return buf, err
		}
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
