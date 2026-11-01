package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aiseeq/akit/transcript"
)

func TestRequiredLiterals(t *testing.T) {
	tests := []struct {
		pattern string
		want    [][]string
	}{
		{`hello`, [][]string{{"hello"}}},
		{`a|b|c`, [][]string{{"a", "b", "c"}}},
		{`[ab]`, [][]string{{"a", "b"}}},
		{`[a-z]`, nil},
		{`graft (commit|gate)`, [][]string{{"graft"}, {"commit", "gate"}}},
		{`.|\w+|x{2}`, nil},
		{`foo.*bar`, [][]string{{"foo"}, {"bar"}}},
		{`a"b`, [][]string{{"a"}, {"b"}}},
	}
	for _, tt := range tests {
		re, err := syntax.Parse(tt.pattern, syntax.Perl)
		if err != nil {
			t.Fatalf("%q: %v", tt.pattern, err)
		}
		got := requiredLiterals(re)
		if !equalNeedles(got, tt.want) {
			t.Errorf("%q: got %#v, want %#v", tt.pattern, got, tt.want)
		}
	}
}

func equalNeedles(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

func FuzzHasFolded(f *testing.F) {
	for _, pair := range [][2]string{{"wfor", "WFor"}, {"ss", "ſS"}, {"k", "K"}, {"ab", "xAbY"}, {"я", "Я"}} {
		f.Add(pair[0], pair[1])
	}
	f.Fuzz(func(t *testing.T, pattern, raw string) {
		if len(pattern) > 128 || len(raw) > 4096 || !utf8.ValidString(pattern) {
			t.Skip()
		}
		re, err := regexp.Compile("(?i)" + regexp.QuoteMeta(pattern))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := hasFolded([]byte(raw), pattern), re.MatchString(raw); got != want {
			t.Fatalf("pattern %q raw %q: got %t, want %t", pattern, raw, got, want)
		}
	})
}

func FuzzTranscriptPrefilter(f *testing.F) {
	for _, pattern := range []string{`K`, `ſ`, `ожидани`, `a|b|c`, `graft (commit|gate)`, `foo.*bar`, `sub -n`, `\\w+`, `footail`, `foo`, `𐐀`, `ι`} {
		for _, text := range []string{"plain synthetic text", "graft gate", "foo<system-reminder>x</system-reminder>bar", "ſK sub -n", "foo"} {
			f.Add(pattern, text)
		}
	}
	f.Fuzz(func(t *testing.T, pattern, text string) {
		if len(pattern) > 80 || len(text) > 4096 {
			t.Skip()
		}
		filter, err := newFilter(options{grep: pattern})
		if err != nil {
			return
		}
		for _, rows := range [][]string{fuzzTranscriptRows(text), fuzzCodexRows(text)} {
			got, err := readMatched(t, rows, true, filter)
			if err != nil {
				t.Fatal(err)
			}
			want, err := readMatched(t, rows, false, filter)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("pattern %q text %q: filtered=%q full=%q", pattern, text, got, want)
			}
		}
	})
}

// FuzzTranscriptEscaped проверяет экранирование латиницы в ключах и строковых значениях JSON.
func FuzzTranscriptEscaped(f *testing.F) {
	for _, seed := range []struct {
		pattern, text string
		mode          uint8
	}{
		{"hello", "hello", 3}, {"wfor", "hello", 2}, {"foobar", "foo", 1},
		{"target", "target", 7}, {" Bash ", "hello", 3},
	} {
		f.Add(seed.pattern, seed.text, seed.mode)
	}
	f.Fuzz(func(t *testing.T, pattern, value string, mode uint8) {
		if len(pattern) > 80 || len(value) > 2048 {
			t.Skip()
		}
		filter, err := newFilter(options{grep: pattern})
		if err != nil {
			return
		}
		for _, rows := range [][]string{fuzzTranscriptRows(value), fuzzCodexRows(value)} {
			for i := range rows {
				rows[i] = escapeLatinJSON(rows[i], mode)
			}
			got, err := readMatched(t, rows, true, filter)
			if err != nil {
				t.Fatal(err)
			}
			want, err := readMatched(t, rows, false, filter)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("pattern %q value %q mode %d: filtered=%q full=%q", pattern, value, mode, got, want)
			}
		}
	})
}

// escapeLatinJSON оставляет JSON-разделители и существующие escape-последовательности без изменений.
func escapeLatinJSON(raw string, mode uint8) string {
	var out strings.Builder
	for i := 0; i < len(raw); {
		if raw[i] != '"' {
			out.WriteByte(raw[i])
			i++
			continue
		}
		start := i
		i++
		for i < len(raw) {
			if raw[i] == '\\' {
				i += 2
				continue
			}
			if raw[i] == '"' {
				break
			}
			i++
		}
		if i >= len(raw) {
			out.WriteString(raw[start:])
			break
		}
		end := i
		j := end + 1
		for j < len(raw) && (raw[j] == ' ' || raw[j] == '\t') {
			j++
		}
		key := j < len(raw) && raw[j] == ':'
		escape := key && mode&1 != 0 || !key && mode&2 != 0
		out.WriteByte('"')
		for j = start + 1; j < end; j++ {
			b := raw[j]
			if b == '\\' && j+1 < end {
				out.WriteByte(b)
				j++
				out.WriteByte(raw[j])
				continue
			}
			if escape && (b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z') && (mode&4 == 0 || j%2 == 0) {
				fmt.Fprintf(&out, `\u%04x`, b)
			} else {
				out.WriteByte(b)
			}
		}
		out.WriteByte('"')
		i = end + 1
	}
	return out.String()
}

func fuzzTranscriptRows(text string) []string {
	quoted, _ := json.Marshal(text)
	q := string(quoted)
	var reminder bytes.Buffer
	enc := json.NewEncoder(&reminder)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(text + "<system-reminder>noise</system-reminder>tail")
	return []string{
		`{"type":"user","timestamp":"2001-02-03T10:00:00Z","origin":{"kind":"human"},"message":{"role":"user","content":` + string(quoted) + `}}`,
		`{"type":"assistant","timestamp":"2001-02-03T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":` + string(quoted) + `}]}}`,
		`{"type":"assistant","timestamp":"2001-02-03T10:00:02Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"bash","name":"Bash","input":{"command":` + q + `}}]}}`,
		`{"type":"assistant","timestamp":"2001-02-03T10:00:03Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"other","name":"Other","input":{"value":` + q + `}}]}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:04Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"other","content":` + q + `}]}}`,
		`{"type":"assistant","timestamp":"2001-02-03T10:00:05Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"mail","name":"SendMessage","input":{"to":"main","message":` + q + `}}]}}`,
		`{"type":"assistant","timestamp":"2001-02-03T10:00:06Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":{}}]}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:07Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"ask","content":"answer"}]},"toolUseResult":{"answers":{` + q + `:"answer"}}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:08Z","origin":{"kind":"human"},"message":{"role":"user","content":` + strings.TrimSpace(reminder.String()) + `}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:08Z","origin":{"kind":"human"},"message":{"content":"\ud801\udc00"}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:08Z","message":{"content":` + strings.TrimSpace(reminder.String()) + `}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:08Z","origin":{"kind":"peer"},"message":{"content":` + strings.TrimSpace(reminder.String()) + `}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:08Z","origin":{"kind":"task-notification"},"message":{"content":` + strings.TrimSpace(reminder.String()) + `}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:09Z","origin":{"kind":"human"},"message":{"role":"user","content":[{"type":"image","source":{}}]}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:10Z","isCompactSummary":true,"message":{"content":"{}"}}`,
		`{"timestamp":"2001-02-03T10:00:11Z","type":"compacted","payload":{"message":""}}`,
		`{"timestamp":"2001-02-03T10:00:12Z","type":"response_item","payload":{"type":"message","role":"assistant","content":` + q + `}}`,
		`{"timestamp":"2001-02-03T10:00:13Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"cc","name":"exec","input":` + q + `}}`,
		`{"timestamp":"2001-02-03T10:00:14Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"cc","output":` + q + `}}`,
		`{"timestamp":"2001-02-03T10:00:15Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"image","url":"x"},{"type":"text","text":` + q + `}]}}`,
	}
}

func fuzzCodexRows(text string) []string {
	quoted, _ := json.Marshal(text)
	q := string(quoted)
	return []string{
		`{"type":"session_meta","payload":{"id":"synthetic"}}`,
		`{"timestamp":"2001-02-03T10:00:00Z","type":"event_msg","payload":{"type":"user_message","message":` + q + `,"images":[{}]}}`,
		`{"timestamp":"2001-02-03T10:00:01Z","type":"event_msg","payload":{"type":"agent_message","message":` + q + `}}`,
		`{"type":"response_item","timestamp":"2001-02-03T10:00:02Z","payload":{"type":"function_call","call_id":"c","name":"Read","arguments":` + q + `}}`,
		`{"type":"response_item","timestamp":"2001-02-03T10:00:03Z","payload":{"type":"function_call_output","call_id":"c","output":` + q + `}}`,
		`{"type":"compacted","timestamp":"2001-02-03T10:00:04Z","payload":{}}`,
	}
}

func readMatched(t *testing.T, rows []string, keep bool, f *filter) ([]string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0600); err != nil {
		return nil, fmt.Errorf("write test transcript: %w", err)
	}
	var got []string
	collect := func(ev transcript.Event) {
		if f.match(ev) {
			encoded, err := json.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, string(encoded))
		}
	}
	var err error
	if !keep {
		_, err = transcript.Read(path, nil, collect)
	} else {
		_, err = transcript.ReadWithTools(path, f.keepWithState, collect)
	}
	return got, err
}

func TestKeepRawTranscriptCases(t *testing.T) {
	tests := []struct {
		name, pattern string
		rows          []string
	}{
		{"напоминание между разными JSON-строками", `target`, []string{`{"type":"user","timestamp":"2001-02-03T10:00:00Z","origin":{"kind":"human"},"message":{"content":[{"type":"text","text":"<system-reminder>"},{"type":"text","text":"target"}]},"other":"</system-reminder>"}`}},
		{"напоминание в prompt", `target`, []string{`{"type":"user","timestamp":"2001-02-03T10:00:00Z","message":{"content":"<system-reminder>target</system-reminder>"}}`}},
		{"напоминание в уведомлении", `target`, []string{`{"type":"user","timestamp":"2001-02-03T10:00:00Z","origin":{"kind":"task-notification"},"message":{"content":"<system-reminder>target</system-reminder>"}}`}},
		{"имя инструмента с пробелами", `hello`, []string{`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"content":[{"type":"tool_use","id":"a","name":" Bash ","input":{}}]}}`, `{"type":"user","timestamp":"2001-02-03T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"a","content":"hello"}]}}`}},
		{"экранированный тип результата", `wfor`, []string{`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"content":[{"type":"tool_use","id":"a","name":"wfor","input":{}}]}}`, `{"type":"user","timestamp":"2001-02-03T10:00:01Z","message":{"content":[{"type":"tool_re\u0073ult","tool_use_id":"a","content":"hello"}]}}`}},
		{"экранированный ключ payload", `wfor`, []string{`{"type":"session_meta","payload":{"id":"synthetic"}}`, `{"timestamp":"2001-02-03T10:00:00Z","type":"response_item","payload":{"type":"function_call","name":"wfor","arguments":"{}","call_id":"a"}}`, `{"timestamp":"2001-02-03T10:00:01Z","type":"response_item","\u0070ayload":{"type":"function_call_output","call_id":"a","output":"hello"}}`}},
		{"экранированный тип вызова", `hello`, []string{`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"content":[{"type":"tool_u\u0073e","id":"a","name":"Bash","input":{"command":"true"}}]}}`, `{"type":"user","timestamp":"2001-02-03T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"a","content":"hello"}]}}`}},
		{"стык Who", `^Bash graft`, []string{`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"Bash","input":{"command":"graft gate"}}]}}`}},
		{"стык Who в результате", `^Bash graft`, []string{`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"Bash","input":{"command":"true"}}]}}`, `{"type":"user","timestamp":"2001-02-03T10:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":"graft started"}]}}`}},
		{"стык пустого Who", `^ see`, []string{`{"type":"user","timestamp":"2001-02-03T10:00:00Z","origin":{"kind":"human"},"message":{"role":"user","content":"see"}}`}},
		{"картинка", `\[image\]`, []string{`{"type":"user","timestamp":"2001-02-03T10:00:00Z","origin":{"kind":"human"},"message":{"role":"user","content":[{"type":"image","source":{}}]}}`}},
		{"картинка Codex", `\[image\]`, []string{`{"type":"session_meta","payload":{"id":"synthetic"}}`, `{"timestamp":"2001-02-03T10:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"","images":[{}]}}`}},
		{"компакция", `context compacted`, []string{`{"type":"user","timestamp":"2001-02-03T10:00:00Z","isCompactSummary":true,"message":{"content":"{}"}}`, `{"timestamp":"2001-02-03T10:00:01Z","type":"compacted","payload":{"message":""}}`}},
		{"ответ без текста", `response without text`, []string{`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"role":"assistant","content":[],"usage":{"input_tokens":1}}}`}},
		{"письмо", `→ main: готово`, []string{`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"SendMessage","input":{"to":"main","message":"готово"}}]}}`}},
		{"ответ окна вопроса", `ответ ← Куда`, []string{`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"AskUserQuestion","input":{}}]}}`, `{"type":"user","timestamp":"2001-02-03T10:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":"ответ"}]},"toolUseResult":{"answers":{"Куда":"ответ"}}}`}},
		{"вырезанное напоминание", `foobar`, []string{`{"type":"user","timestamp":"2001-02-03T10:00:00Z","origin":{"kind":"human"},"message":{"role":"user","content":"foo<system-reminder>шум</system-reminder>bar"}}`}},
		{"несколько напоминаний", `foobar`, []string{`{"type":"user","timestamp":"2001-02-03T10:00:00Z","origin":{"kind":"human"},"message":{"content":"f<system-reminder>x</system-reminder>oo<system-reminder>y</system-reminder>bar"}}`}},
		{"экранированный дефис", `foobar`, []string{`{"type":"user","timestamp":"2001-02-03T10:00:00Z","origin":{"kind":"human"},"message":{"content":"foo<system\u002dreminder>x</system\u002dreminder>bar"}}`}},
		{"свёртка знака", `ι`, []string{`{"type":"user","timestamp":"2001-02-03T10:00:00Z","origin":{"kind":"human"},"message":{"content":"\u0345"}}`}},
		{"пара суррогатов", `𐐀`, []string{`{"type":"user","timestamp":"2001-02-03T10:00:00Z","origin":{"kind":"human"},"message":{"content":"\ud801\udc00"}}`}},
		{"повторный content результата", `^Read target`, []string{`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"content":[{"type":"tool_use","name":"Read","input":{}}]}}`, `{"type":"user","timestamp":"2001-02-03T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"a","content":"x"}],"content":[{"type":"tool_result","content":"target"}]}}`}},
		{"повторный content вызова", `target`, []string{`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"content":[{"type":"tool_use","id":"a","name":"Read","input":{}}],"content":[{"type":"tool_use","name":"Write","input":{}}]}}`, `{"type":"user","timestamp":"2001-02-03T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"a","content":"target"}]}}`}},
		{"экранированный тег", `foobar`, []string{`{"type":"user","timestamp":"2001-02-03T10:00:00Z","origin":{"kind":"human"},"message":{"content":"foo<system-reminder>x<\/system-reminder>bar"}}`}},
		{"напоминание и свёртка Unicode", `kſ`, []string{`{"type":"user","timestamp":"2001-02-03T10:00:00Z","origin":{"kind":"human"},"message":{"content":"K<system-reminder>x</system-reminder>S"}}`}},
		{"JSON ввода инструмента", `ключ`, []string{`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"Other","input":{"x":"ключ"}}]}}`}},
		{"JSON escape", `a.b`, []string{`{"type":"user","timestamp":"2001-02-03T10:00:00Z","origin":{"kind":"human"},"message":{"role":"user","content":"a\bb"}}`}},
		{"JSON form feed", `a.b`, []string{`{"type":"user","timestamp":"2001-02-03T10:00:00Z","origin":{"kind":"human"},"message":{"content":"a\fb"}}`}},
		{"JSON unicode", `foobar`, []string{`{"type":"user","timestamp":"2001-02-03T10:00:00Z","origin":{"kind":"human"},"message":{"content":"foo\u0062ar"}}`}},
		{"одиночный суррогат", `�`, []string{`{"type":"user","timestamp":"2001-02-03T10:00:00Z","origin":{"kind":"human"},"message":{"content":"\ud83d"}}`}},
		{"состояние вызова", `^Read /home`, []string{`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"Read","input":{"file_path":"/home/x"}}]}}`, `{"type":"user","timestamp":"2001-02-03T10:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":"данные"}]}}`}},
		{"совпадение только по Who", `^Read `, []string{`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"Read","input":{"file_path":"/tmp/x"}}]}}`, `{"type":"user","timestamp":"2001-02-03T10:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":"данные"}]}}`}},
		{"большой ввод и состояние", `^Read target`, []string{`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"content":[{"type":"tool_use","id":"c","name":"Read","input":{"file_path":"` + strings.Repeat("data", 1000) + `","nested":{"input":[1,{"x":"quote\\\""}]}}}]}}`, `{"type":"user","timestamp":"2001-02-03T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"c","content":"target"}]}}`}},
		{"повторное поле content", `^Read target`, []string{`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"content":12,"content":[{"type":"tool_use","id":"c","name":"Read","input":{}}]}}`, `{"type":"user","timestamp":"2001-02-03T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"c","content":"target"}]}}`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := newFilter(options{grep: tt.pattern})
			if err != nil {
				t.Fatal(err)
			}
			got, err := readMatched(t, tt.rows, true, f)
			if err != nil {
				t.Fatal(err)
			}
			want, err := readMatched(t, tt.rows, false, f)
			if err != nil {
				t.Fatal(err)
			}
			if len(want) == 0 {
				t.Fatal("эталонный разбор не дал совпадений")
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("filtered=%q full=%q", got, want)
			}
		})
	}
}

func FuzzDeferredCallState(f *testing.F) {
	f.Add("Read", "Write", "a", "b", "target", "target")
	f.Add("Bash", "Read", "same", "same", "hello", "^Read hello")
	f.Add("ſK", "Edit", "", "b", "payload", "ſ")
	f.Fuzz(func(t *testing.T, nameA, nameB, idA, idB, value, pattern string) {
		if len(nameA)+len(nameB)+len(idA)+len(idB)+len(value)+len(pattern) > 2048 {
			t.Skip()
		}
		filter, err := newFilter(options{grep: pattern})
		if err != nil {
			return
		}
		call := func(id, name string) map[string]any {
			return map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{"description": "unrelated"}}
		}
		result := func(id string) map[string]any {
			return map[string]any{"type": "tool_result", "tool_use_id": id, "content": value}
		}
		makeRow := func(typ string, parts []map[string]any) string {
			out, err := json.Marshal(map[string]any{"type": typ, "timestamp": "2001-02-03T10:00:00Z", "message": map[string]any{"content": parts}})
			if err != nil {
				t.Fatal(err)
			}
			return string(out)
		}
		rows := []string{makeRow("assistant", []map[string]any{call(idA, nameA), call(idB, nameB)}),
			makeRow("user", []map[string]any{result(idB)}), makeRow("user", []map[string]any{result(idA)})}
		got, err := readMatched(t, rows, true, filter)
		if err != nil {
			t.Fatal(err)
		}
		want, err := readMatched(t, rows, false, filter)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("filtered=%q full=%q", got, want)
		}
	})
}

func TestDeferredEscapedIDWithRawFilter(t *testing.T) {
	rows := []string{
		`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"content":[{"type":"tool_use","\u0069d":"a","name":" Bash ","input":{"command":"hello"}}]}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"a","content":"hello include"}]}}`,
	}
	f, err := newFilter(options{grep: "hello", raw: "include"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := readMatched(t, rows, true, f)
	if err != nil {
		t.Fatal(err)
	}
	// Эталон читает оба события, но --raw оставляет только результат.
	want, err := readMatched(t, rows, false, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 2 || len(got) != 1 || got[0] != want[1] || !strings.Contains(got[0], `"Who":" Bash "`) {
		t.Fatalf("filtered=%q full=%q", got, want)
	}
}

func TestDeferredEscapedTypeWithRawFilter(t *testing.T) {
	rows := []string{
		`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"content":[{"type":"tool_u\u0073e","id":"a","name":" Bash ","input":{"command":"hello"}}]}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"a","content":"hello include"}]}}`,
	}
	f, err := newFilter(options{grep: "hello", raw: "include"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := readMatched(t, rows, true, f)
	if err != nil {
		t.Fatal(err)
	}
	// Эталон читает оба события, но --raw оставляет только результат.
	want, err := readMatched(t, rows, false, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 2 || len(got) != 1 || got[0] != want[1] || !strings.Contains(got[0], `"Who":" Bash "`) {
		t.Fatalf("filtered=%q full=%q", got, want)
	}
}

func TestCodexSkippedState(t *testing.T) {
	rows := []string{
		`{"type":"session_meta","payload":{"id":"one","cwd":"/first"}}`,
		`{"type":"turn_context","payload":{"cwd":"/context"}}`,
		`{"type":"response_item","timestamp":"2001-02-03T10:00:00Z","payload":{"type":"function_call","name":" Other ","call_id":"a","arguments":"{}"}}`,
		`{"type":"response_item","timestamp":"2001-02-03T10:00:01Z","payload":{"type":"function_call_output","call_id":"a","output":"target"}}`,
		`{"type":"session_meta","payload":{"id":"two","cwd":"/second"}}`,
		`{"type":"response_item","timestamp":"2001-02-03T10:00:02Z","payload":{"type":"custom_tool_call","name":"Custom","call_id":"b","input":"nothing"}}`,
		`{"type":"response_item","timestamp":"2001-02-03T10:00:03Z","payload":{"type":"custom_tool_call_output","call_id":"b","output":"target"}}`,
	}
	f, err := newFilter(options{grep: "target"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := readMatched(t, rows, true, f)
	if err != nil {
		t.Fatal(err)
	}
	want, err := readMatched(t, rows, false, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 2 || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("filtered=%q full=%q", got, want)
	}
	if !strings.Contains(want[0], `"Who":" Other "`) || !strings.Contains(want[0], `"Cwd":"/context"`) ||
		!strings.Contains(want[1], `"Who":"Custom"`) || !strings.Contains(want[1], `"SessionID":"two"`) {
		t.Fatalf("состояние Codex: %q", want)
	}
}

func TestCodexRawPreservesState(t *testing.T) {
	rows := []string{
		`{"type":"session_meta","payload":{"id":"session","cwd":"/dir"}}`,
		`{"type":"response_item","timestamp":"2001-02-03T10:00:00Z","payload":{"type":"function_call","name":"Bash","call_id":"a","arguments":"{}"}}`,
		`{"type":"response_item","timestamp":"2001-02-03T10:00:01Z","payload":{"type":"function_call_output","call_id":"a","output":"include"}}`,
	}
	f, err := newFilter(options{raw: "include"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := readMatched(t, rows, true, f)
	if err != nil {
		t.Fatal(err)
	}
	want, err := readMatched(t, rows, false, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 2 || len(got) != 1 || got[0] != want[1] || !strings.Contains(got[0], `"Who":"Bash"`) {
		t.Fatalf("filtered=%q full=%q", got, want)
	}
}

func TestCodexOrdinaryLineDiscarded(t *testing.T) {
	f, err := newFilter(options{grep: "target"})
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"type":"event_msg","payload":{"type":"agent_message","message":"unrelated"}}`)
	if f.keepWithState(raw, transcript.ToolNames{}) {
		t.Fatal("строка без совпадения не отсеялась")
	}
}

func TestPayloadCaseAndState(t *testing.T) {
	rows := []string{
		`{"type":"session_meta","PAYLOAD":{"id":"synthetic"}}`,
		`{"timestamp":"2001-02-03T10:00:00Z","type":"response_item","PAYLOAD":{"type":"function_call","call_id":"a","name":"Read","arguments":"{}"}}`,
		`{"timestamp":"2001-02-03T10:00:01Z","type":"response_item","PAYLOAD":{"type":"function_call_output","call_id":"a","output":"target"}}`,
	}
	f, err := newFilter(options{grep: "target"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := readMatched(t, rows, true, f)
	if err != nil {
		t.Fatal(err)
	}
	want, err := readMatched(t, rows, false, f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("filtered=%q full=%q", got, want)
	}
}

func TestDeferredInvalidUTF8(t *testing.T) {
	id := string([]byte{0xff})
	rows := []string{
		`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"Read","input":{}}]}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"target"}]}}`,
	}
	f, err := newFilter(options{grep: "target"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := readMatched(t, rows, true, f)
	if err != nil {
		t.Fatal(err)
	}
	want, err := readMatched(t, rows, false, f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("filtered=%q full=%q", got, want)
	}
}

func TestDeferredCallOrdering(t *testing.T) {
	rows := []string{
		`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"content":[{"type":"tool_use","id":"a","name":"Read","input":{}},{"type":"tool_use","id":"b","name":"Bash","input":{}}]}}`,
		`{"type":"assistant","timestamp":"2001-02-03T10:00:01Z","message":{"content":[{"type":"tool_use","id":"b","name":"Write","input":{}}]}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"b","content":"target"}]}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"a","content":"target"}]}}`,
		`{"type":"assistant","timestamp":"2001-02-03T10:00:04Z","message":{"content":[{"type":"tool_use","id":"a","name":"Edit","input":{"file_path":"target"}}]}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:05Z","message":{"content":[{"type":"tool_result","tool_use_id":"a","content":"target"}]}}`,
		`{"type":"assistant","timestamp":"2001-02-03T10:00:06Z","message":{"content":[{"type":"tool_use","name":"Empty","input":{}}]}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:07Z","message":{"content":[{"type":"tool_result","content":"target"},{"type":"text","tool_use_id":"b","text":"irrelevant"}]}}`,
		`{"type":"assistant","timestamp":"bad","message":{"content":[{"type":"tool_use","id":"b","name":"Read","input":{"file_path":"target"}}]}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:08Z","message":{"content":[{"type":"tool_result","tool_use_id":"b","content":"target"}]}}`,
		`{"type":"assistant","timestamp":"2001-02-03T10:00:09Z","message":{"content":[{"type":"tool_use","ID":"c","name":"Read","input":{}}]}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:10Z","message":{"content":[{"type":"tool_result","tool_uſe_id":"c","content":"target"}]}}`,
		`{"type":"assistant","timestamp":"2001-02-03T10:00:11Z","message":{"content":[{"type":"tool_use","id":"d\u002de","name":"Read","input":{}}]}}`,
		`{"type":"user","timestamp":"2001-02-03T10:00:12Z","message":{"content":[{"type":"tool_result","tool_use_id":"d-e","content":"target"}]}}`,
	}
	for _, pattern := range []string{"target", "^Read target", "^Write target", "^Edit target", "^Empty target"} {
		f, err := newFilter(options{grep: pattern})
		if err != nil {
			t.Fatal(err)
		}
		got, err := readMatched(t, rows, true, f)
		if err != nil {
			t.Fatal(err)
		}
		want, err := readMatched(t, rows, false, f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("%q: filtered=%q full=%q", pattern, got, want)
		}
	}
}

func TestDeferredCallsAcrossLimit(t *testing.T) {
	const count = 33000
	rows := make([]string, 0, count+1)
	for i := range count {
		rows = append(rows, fmt.Sprintf(`{"type":"assistant","timestamp":"2001-02-03T10:00:00Z","message":{"content":[{"type":"tool_use","id":"call%d","name":"Read","input":{}}]}}`, i))
	}
	rows = append(rows, fmt.Sprintf(`{"type":"user","timestamp":"2001-02-03T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call0","content":"target"},{"type":"tool_result","tool_use_id":"call%d","content":"target"}]}}`, count-1))
	f, err := newFilter(options{grep: `^Read target`})
	if err != nil {
		t.Fatal(err)
	}
	got, err := readMatched(t, rows, true, f)
	if err != nil {
		t.Fatal(err)
	}
	want, err := readMatched(t, rows, false, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("filtered=%q full=%q", got, want)
	}
}

func BenchmarkKeepRaw(b *testing.B) {
	fixtures := []struct {
		name    string
		pattern string
		text    string
	}{
		{
			name:    "russian",
			pattern: `ожидани`,
			text:    "ответы и общие сведения: русский текст без совпадения и пояснения ",
		},
		{
			name:    "ascii-sub-n",
			pattern: `sub -n`,
			text:    "status session settings source stream summary sample ",
		},
		{
			name:    "ascii-wfor",
			pattern: `wfor`,
			text:    "word workflow window work wrapper ",
		},
	}
	for _, fixture := range fixtures {
		b.Run(fixture.name, func(b *testing.B) {
			f, err := newFilter(options{grep: fixture.pattern})
			if err != nil {
				b.Fatal(err)
			}
			raw := []byte(`{"type":"assistant","message":"` + strings.Repeat(fixture.text, 1<<20/len(fixture.text)) + `"}`)
			needle := []byte(strings.ToLower(fixture.pattern))
			b.SetBytes(int64(len(raw)))
			b.Run("fold-search", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					f.keepWithState(raw, transcript.ToolNames{})
				}
			})
			b.Run("lower-copy-baseline", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					bytes.Contains(bytes.ToLower(raw), needle)
				}
			})
		})
	}
}
