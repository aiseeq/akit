package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/aiseeq/akit/transcript"
)

// commandResult — завершение одной команды codex (event_msg item_completed, item CommandExecution): её код
// выхода и её собственный вывод. Скрипт exec зовёт несколько команд, а его общий вывод говорит только, что
// скрипт дошёл до конца, — код и вывод каждой команды есть только здесь.
type commandResult struct {
	line int
	at   time.Time
	cmd  string
	code int
	out  string
}

// codexCommandResults — завершения команд из файла codex по порядку строк; код известен (не меньше нуля).
func codexCommandResults(path string) ([]commandResult, error) {
	items, err := transcript.CodexCommandResults(path)
	if err != nil {
		return nil, err
	}
	res := make([]commandResult, 0, len(items))
	for _, item := range items {
		res = append(res, commandResult{line: item.Line, at: item.Completed, cmd: item.Command, code: item.ExitCode, out: item.Output})
	}
	return res, nil
}

// attachCommandResults даёт каждому вызову команды codex свой вывод: вызов получает собственный id, а вывод с
// кодом из его завершения встаёт в события на строку завершения. Завершение ищется после вызова с той же
// командой и до следующего вызова той же команды; одинаковые команды одного скрипта берут завершения по
// порядку. Вызов без завершения (прерван, обвязка не записала) остаётся при общем выводе скрипта.
func attachCommandResults(evs []transcript.Event, res []commandResult) []transcript.Event {
	if len(res) == 0 {
		return evs
	}
	byCmd := map[string][]int{}
	for i, r := range res {
		byCmd[r.cmd] = append(byCmd[r.cmd], i)
	}
	callLines := commandCallLines(evs)
	used := make([]bool, len(res))
	var added []transcript.Event
	for i := range evs {
		e := &evs[i]
		if e.Role != transcript.Cmd || e.CallID == "" {
			continue
		}
		ls := callLines[e.Text]
		bound := -1
		if k := sort.SearchInts(ls, e.Line+1); k < len(ls) {
			bound = ls[k]
		}
		for _, ri := range byCmd[e.Text] {
			r := res[ri]
			if used[ri] || r.line <= e.Line {
				continue
			}
			if bound >= 0 && r.line >= bound {
				break
			}
			used[ri] = true
			e.CallID = fmt.Sprintf("%s#%d", e.CallID, i)
			added = append(added, transcript.Event{Line: r.line, Time: r.at, Role: transcript.Out, Who: e.Who,
				Text: r.out, SessionID: e.SessionID, Cwd: e.Cwd, CallID: e.CallID,
				Result: json.RawMessage(`{"exit_code":` + strconv.Itoa(r.code) + `}`)})
			break
		}
	}
	if len(added) == 0 {
		return evs
	}
	sort.SliceStable(added, func(a, b int) bool { return added[a].Line < added[b].Line })
	all := make([]transcript.Event, 0, len(evs)+len(added))
	j := 0
	for i := range evs {
		for j < len(added) && added[j].Line < evs[i].Line {
			all = append(all, added[j])
			j++
		}
		all = append(all, evs[i])
	}
	return append(all, added[j:]...)
}

// commandCallLines — строки вызовов каждой команды по порядку, без повторов (вызовы одного скрипта делят строку).
func commandCallLines(evs []transcript.Event) map[string][]int {
	callLines := map[string][]int{}
	for i := range evs {
		e := &evs[i]
		if e.Role == transcript.Cmd && e.CallID != "" {
			ls := callLines[e.Text]
			if len(ls) == 0 || ls[len(ls)-1] != e.Line {
				callLines[e.Text] = append(ls, e.Line)
			}
		}
	}
	return callLines
}
