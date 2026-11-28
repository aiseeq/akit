package transcript

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// ErrFollowRestart — rollout codex впервые записал событие user_message или agent_message: по Read реплики и текст
// агента всего файла берутся теперь из них, и события уже разобранных строк из response_item неверны. Разбор
// надо начать новым Follow с начала файла.
var ErrFollowRestart = errors.New("transcript: rollout began to write message events; parse it again from the start")

// Follow разбирает файл по мере дописывания, порциями полных строк. После каждой порции у него те же события и
// Meta, что Read даёт для файла такой длины: состояние разбора (имена инструментов по id вызова, сессия и модель
// codex) живёт между порциями, а какие события пишет rollout, Follow узнаёт из уже прочитанных строк. Один
// Follow — один файл с его начала.
type Follow struct {
	rd          reader
	head        []byte // первые codexHead байт файла
	user, agent bool
	// fed и prescanned — сколько байт с начала файла пришло через Feed и через Prescan
	fed, prescanned int
	// skip — строки с первой по эту только ведут состояние разбора, без событий (SkipThrough)
	skip int
}

// OnCodexCommands registers a callback for completed Codex shell commands (item_completed of a CommandExecution),
// the same results ReadWithCodexCommandsDiscardOutput gives, without Output. It must be set before the first line is
// fed. For a line the callback runs before the line's events; lines up to SkipThrough do not reach it. A Follow
// started anew after ErrFollowRestart reads from the first line and gives the earlier results again.
func (f *Follow) OnCodexCommands(fn func(CodexCommandResult)) error {
	if f.rd.lineNo > 0 {
		return errors.New("transcript: codex command callback after the first feed")
	}
	f.rd.commandFn = fn
	return nil
}

// SkipThrough — строки с первой по n-ю (их события у вызывающего уже есть) Follow не разбирает целиком, а только
// ведёт по ним состояние, как ReadWithTools для отброшенных фильтром строк: имена инструментов по id вызова и
// состояние codex. События этих строк не отдаются, у следующих — те же, что дал бы полный разбор. Meta.Bad считает
// только строки после n-й. Только до первого Feed.
func (f *Follow) SkipThrough(n int) error {
	if f.rd.lineNo > 0 {
		return errors.New("transcript: skip after the first feed")
	}
	if n <= 0 {
		return nil
	}
	f.skip = n
	f.rd.toolNames = map[string]struct{}{}
	f.rd.keepWithTools = func([]byte, ToolNames) bool { return false }
	f.rd.skipping = true
	return nil
}

// endSkip — пропуск кончился: отложенные вызовы применяются по порядку строк, дальше разбор как у Read.
func (f *Follow) endSkip() {
	f.rd.flushCalls()
	f.rd.keepWithTools, f.rd.toolNames, f.rd.skipping, f.skip = nil, nil, false, 0
}

// NewFollow — разбор файла с его первой строки.
func NewFollow() *Follow {
	return &Follow{rd: reader{claude: newClaudeParser(), codex: newCodexParser()}}
}

// Feed разбирает следующие полные строки файла: data кончается '\n', недописанный хвост вызывающий держит у себя.
// События — в fn по порядку, с Line. Ошибка — порция без '\n' в конце, строка длиннее предела Read или
// ErrFollowRestart; при ошибке порция не разбирается вовсе.
func (f *Follow) Feed(data []byte, fn func(Event)) error {
	if len(data) == 0 {
		return nil
	}
	head, user, agent, err := f.scan(data, f.fed)
	if err != nil {
		return err
	}
	// Read смотрит, какие события пишет rollout, только у файла, начало которого — session_meta.
	codex := bytes.Contains(head, []byte(`"type":"session_meta"`))
	eventUser, eventAgent := codex && user, codex && agent
	if f.rd.lineNo > 0 && (eventUser != f.rd.codex.eventUser || eventAgent != f.rd.codex.eventAgent) {
		return ErrFollowRestart
	}
	f.head, f.user, f.agent = head, user, agent
	f.fed += len(data)
	f.rd.codex.eventUser, f.rd.codex.eventAgent = eventUser, eventAgent
	f.rd.fn = fn
	defer func() { f.rd.fn = nil }()
	for rest := data; len(rest) > 0; {
		if f.skip > 0 && f.rd.lineNo >= f.skip {
			f.endSkip()
		}
		line, tail, _ := bytes.Cut(rest, []byte{'\n'})
		// как bufio.Reader.ReadLine у Read: конец строки — "\n" или "\r\n"
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) > 0 {
			f.rd.line(line)
		} else {
			f.rd.lineNo++ // пустая строка — тоже строка файла
		}
		rest = tail
	}
	if f.skip > 0 {
		// отложенные вызовы держат копии строк: применённые сейчас, они не копятся в памяти между порциями
		f.rd.flushCalls()
	}
	return nil
}

// Prescan — строки, которые Follow потом получит через Feed (полные строки с начала файла, можно частями): по
// ним он заранее узнаёт, какие события пишет rollout, как Read по всему файлу, и на этих строках уже не вернёт
// ErrFollowRestart. Так большой файл разбирается частями без перезапуска. Только до первого Feed.
func (f *Follow) Prescan(data []byte) error {
	if f.rd.lineNo > 0 {
		return errors.New("transcript: prescan after the first feed")
	}
	head, user, agent, err := f.scan(data, f.prescanned)
	if err != nil {
		return err
	}
	f.head, f.user, f.agent = head, user, agent
	f.prescanned += len(data)
	return nil
}

// scan — начало файла и признаки событий сообщений с учётом data, которая лежит в файле с байта at; состояние
// Follow не меняет.
func (f *Follow) scan(data []byte, at int) (head []byte, user, agent bool, err error) {
	if len(data) > 0 && data[len(data)-1] != '\n' {
		return nil, false, false, errors.New("transcript: follow chunk does not end with a newline")
	}
	head = f.head
	if n := len(head); n < codexHead && at <= n && at+len(data) > n {
		head = append(bytes.Clone(head), data[n-at:min(len(data), codexHead-at)]...)
	}
	user, agent = f.user, f.agent
	for rest := data; len(rest) > 0; {
		line, tail, _ := bytes.Cut(rest, []byte{'\n'})
		if len(line) > maxLine {
			return nil, false, false, fmt.Errorf("transcript: line %d longer than %d bytes", f.rd.lineNo+1, maxLine)
		}
		lh := line[:min(len(line), codexHead)]
		user = user || bytes.Contains(lh, userMessageLine)
		agent = agent || bytes.Contains(lh, agentMessageLine)
		rest = tail
	}
	return head, user, agent, nil
}

// MessageEvents — пишет ли rollout события user_message и agent_message, по строкам, уже пришедшим через Prescan
// и Feed: от этого зависят события всех его строк (см. ErrFollowRestart).
func (f *Follow) MessageEvents() (user, agent bool) {
	codex := bytes.Contains(f.head, []byte(`"type":"session_meta"`))
	return codex && f.user, codex && f.agent
}

// Lines — сколько строк файла уже разобрано.
func (f *Follow) Lines() int { return f.rd.lineNo }

// Meta — то, что Read сообщил бы о файле из разобранных строк.
func (f *Follow) Meta() Meta {
	m := f.rd.meta
	m.Originator, m.Account = f.rd.codex.originator, f.rd.codex.firstAccount
	return m
}

// LineEndsAfter tells whether a line of the file ends between off and size: a reader that has the file up to off
// has a complete line more to read.
func LineEndsAfter(path string, off, size int64) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("read transcript: %w", err)
	}
	defer f.Close()
	b := make([]byte, 64<<10)
	for off < size {
		n, err := f.ReadAt(b[:min(int64(len(b)), size-off)], off)
		if bytes.IndexByte(b[:n], '\n') >= 0 {
			return true, nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, fmt.Errorf("read transcript: %w", err)
		}
		off += int64(n)
	}
	return false, nil
}

// Inode is the inode of a stat: a file written anew at the same path has another one. 0 where the system has none.
func Inode(st os.FileInfo) uint64 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return s.Ino
	}
	return 0
}
