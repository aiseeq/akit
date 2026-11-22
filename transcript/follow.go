package transcript

import (
	"bytes"
	"errors"
	"fmt"
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

// Lines — сколько строк файла уже разобрано.
func (f *Follow) Lines() int { return f.rd.lineNo }

// Meta — то, что Read сообщил бы о файле из разобранных строк.
func (f *Follow) Meta() Meta {
	m := f.rd.meta
	m.Originator, m.Account = f.rd.codex.originator, f.rd.codex.firstAccount
	return m
}
