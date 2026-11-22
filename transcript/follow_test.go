package transcript

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// readPrefix — события и Meta, которые Read даёт для файла из первых n байт data.
func readPrefix(t *testing.T, data []byte, n int) ([]Event, Meta) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.jsonl")
	if err := os.WriteFile(path, data[:n], 0o600); err != nil {
		t.Fatal(err)
	}
	var evs []Event
	meta, err := Read(path, nil, func(e Event) { evs = append(evs, e) })
	if err != nil {
		t.Fatal(err)
	}
	return evs, meta
}

// cuts — концы строк data: после каждой '\n'.
func cuts(data []byte) []int {
	var out []int
	for i, c := range data {
		if c == '\n' {
			out = append(out, i+1)
		}
	}
	return out
}

// followTo кормит Follow полными строками data порциями по шагу step строк; на ErrFollowRestart начинает заново
// с начала файла, как должен вызывающий. Отдаёт события по порядку и Follow.
func followTo(t *testing.T, data []byte, step int) ([]Event, *Follow, int) {
	t.Helper()
	ends := cuts(data)
	f := NewFollow()
	var evs []Event
	restarts, done := 0, 0
	for i := step - 1; ; i += step {
		i = min(i, len(ends)-1)
		chunk := data[done:ends[i]]
		var got []Event
		err := f.Feed(chunk, func(e Event) { got = append(got, e) })
		if errors.Is(err, ErrFollowRestart) {
			restarts++
			f, evs, got = NewFollow(), nil, nil
			if err := f.Feed(data[:ends[i]], func(e Event) { got = append(got, e) }); err != nil {
				t.Fatal(err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, got...)
		done = ends[i]
		want, meta := readPrefix(t, data, done)
		if !reflect.DeepEqual(evs, want) {
			t.Fatalf("после %d байт события Follow не равны Read:\n%v\nждали:\n%v", done, evs, want)
		}
		if got := f.Meta(); got.Name != meta.Name || got.Bad != meta.Bad || got.Originator != meta.Originator || got.Account != meta.Account {
			t.Fatalf("после %d байт Meta %+v, ждали %+v", done, got, meta)
		}
		if f.Lines() != bytes.Count(data[:done], []byte{'\n'}) {
			t.Fatalf("Lines %d после %d байт", f.Lines(), done)
		}
		if i == len(ends)-1 {
			return evs, f, restarts
		}
	}
}

// Follow по порциям полных строк даёт то же, что Read для файла той же длины: на образцах Claude Code и codex,
// с пустой строкой, \r\n и испорченной строкой посреди.
func TestFollowEqualsRead(t *testing.T) {
	for _, name := range []string{"claude.jsonl", "codex.jsonl"} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		ends := cuts(data)
		mid := ends[len(ends)/2]
		// пустая строка, строка с \r\n и недописанная запись внутри файла
		mixed := append(append(bytes.Clone(data[:mid]), "\n{\"type\":\"user\",\"timest\n"...), data[mid:]...)
		mixed = bytes.Replace(mixed, []byte("}\n"), []byte("}\r\n"), 1)
		for _, d := range [][]byte{data, mixed} {
			for _, step := range []int{1, 2, 3, 100} {
				evs, _, restarts := followTo(t, d, step)
				if len(evs) == 0 || restarts != 0 {
					t.Fatalf("%s шаг %d: событий %d, перезапусков %d", name, step, len(evs), restarts)
				}
			}
		}
	}
}

// Rollout начал писать события user_message: по Read реплики берутся уже из них, и прежние события из
// response_item неверны — Follow просит начать заново, а с начала файла даёт то же, что Read.
func TestFollowRestartOnMessageEvents(t *testing.T) {
	lines := []string{
		`{"timestamp":"2026-10-25T10:00:00.000Z","type":"session_meta","payload":{"id":"s1","cwd":"/w","originator":"codex-tui"}}`,
		`{"timestamp":"2026-10-25T10:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"привет"}]}}`,
		`{"timestamp":"2026-10-25T10:00:01.001Z","type":"event_msg","payload":{"type":"user_message","message":"привет"}}`,
		`{"timestamp":"2026-10-25T10:00:02.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"да"}]}}`,
		`{"timestamp":"2026-10-25T10:00:02.001Z","type":"event_msg","payload":{"type":"agent_message","message":"да"}}`,
	}
	data := []byte(strings.Join(lines, "\n") + "\n")
	evs, _, restarts := followTo(t, data, 2)
	if restarts != 2 || len(evs) != 2 {
		t.Fatalf("перезапусков %d (ждали 2), событий %d (ждали 2): %v", restarts, len(evs), evs)
	}
	f := NewFollow()
	if err := f.Feed(data[:cuts(data)[1]], func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if err := f.Feed(data[cuts(data)[1]:], func(Event) {}); !errors.Is(err, ErrFollowRestart) {
		t.Fatalf("ошибка %v, ждали ErrFollowRestart", err)
	}
	if f.Lines() != 2 {
		t.Fatalf("после отказа Lines %d, ждали 2: порция не должна разбираться", f.Lines())
	}
}

// Порция должна кончаться переводом строки: недописанный хвост остаётся у вызывающего.
func TestFollowRejectsPartialLine(t *testing.T) {
	f := NewFollow()
	if err := f.Feed([]byte(`{"type":"user"`), func(Event) {}); err == nil {
		t.Fatal("порция без перевода строки принята")
	}
	if f.Lines() != 0 {
		t.Fatalf("Lines %d после отказа", f.Lines())
	}
}

// SourcePatterns — те же файлы, что перечисляет AllSources.
func TestSourcePatternsMatchAllSources(t *testing.T) {
	home := t.TempDir()
	paths := []string{
		".claude/projects/p/11111111-2222-3333-4444-555555555555.jsonl",
		".claude/projects/p/11111111-2222-3333-4444-555555555555/subagents/agent-abc.jsonl",
		".local/state/worker/job/codex/sessions/2026/10/25/rollout-2026-10-25T10-00-00-x.jsonl",
		".claude/projects/p/notes.txt",
		".local/state/worker/job/codex/other.jsonl",
	}
	for _, p := range paths {
		full := filepath.Join(home, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	srcs, err := AllSources(home, time.Time{}, "", true, true)
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, s := range srcs {
		listed = append(listed, s.Path)
	}
	var matched []string
	for _, p := range paths {
		full := filepath.Join(home, p)
		for _, pat := range SourcePatterns(home, true, true) {
			if ok, _ := filepath.Match(pat, full); ok {
				matched = append(matched, full)
			}
		}
	}
	if strings.Join(listed, "\n") != strings.Join(matched, "\n") || len(listed) != 3 {
		t.Fatalf("AllSources:\n%s\nпо шаблонам:\n%s", strings.Join(listed, "\n"), strings.Join(matched, "\n"))
	}
}

// Prescan всего файла до первого Feed: разбор частями идёт без перезапуска и даёт то же, что Read.
func TestFollowPrescan(t *testing.T) {
	lines := []string{
		`{"timestamp":"2026-10-25T10:00:00.000Z","type":"session_meta","payload":{"id":"s1","cwd":"/w"}}`,
		`{"timestamp":"2026-10-25T10:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"привет"}]}}`,
		`{"timestamp":"2026-10-25T10:00:01.001Z","type":"event_msg","payload":{"type":"user_message","message":"привет"}}`,
	}
	data := []byte(strings.Join(lines, "\n") + "\n")
	ends := cuts(data)
	f := NewFollow()
	for i, at := 0, 0; i < len(ends); at, i = ends[i], i+1 {
		if err := f.Prescan(data[at:ends[i]]); err != nil {
			t.Fatal(err)
		}
	}
	var evs []Event
	for i, at := 0, 0; i < len(ends); at, i = ends[i], i+1 {
		if err := f.Feed(data[at:ends[i]], func(e Event) { evs = append(evs, e) }); err != nil {
			t.Fatalf("строка %d: %v", i+1, err)
		}
	}
	want, _ := readPrefix(t, data, len(data))
	if !reflect.DeepEqual(evs, want) || len(evs) != 1 {
		t.Fatalf("события %v, ждали %v", evs, want)
	}
	if err := f.Prescan(data[:ends[0]]); err == nil {
		t.Fatal("Prescan после Feed принят")
	}
}

// Follow, пропустивший первые n строк, отдаёт для следующих те же события, что Read, на любом n и любом шаге.
func TestFollowSkipEqualsRead(t *testing.T) {
	for _, name := range []string{"claude.jsonl", "codex.jsonl"} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		all, meta := readPrefix(t, data, len(data))
		ends := cuts(data)
		for skip := 0; skip <= len(ends); skip++ {
			for _, step := range []int{1, 3, len(ends)} {
				f := NewFollow()
				if err := f.Prescan(data); err != nil {
					t.Fatal(err)
				}
				if err := f.SkipThrough(skip); err != nil {
					t.Fatal(err)
				}
				var got []Event
				done := 0
				for i := step - 1; done < len(data); i += step {
					end := ends[min(i, len(ends)-1)]
					if err := f.Feed(data[done:end], func(e Event) { got = append(got, e) }); err != nil {
						t.Fatal(err)
					}
					done = end
				}
				var want []Event
				for _, e := range all {
					if e.Line > skip {
						want = append(want, e)
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s, пропуск %d, шаг %d: события\n%v\nждали\n%v", name, skip, step, got, want)
				}
				if m := f.Meta(); m.Name != meta.Name || m.Originator != meta.Originator || m.Account != meta.Account {
					t.Fatalf("%s, пропуск %d: Meta %+v, ждали %+v", name, skip, m, meta)
				}
			}
		}
	}
	if err := NewFollow().SkipThrough(1); err != nil {
		t.Fatal(err)
	}
	f := NewFollow()
	if err := f.Feed([]byte("{}\n"), func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if f.SkipThrough(1) == nil {
		t.Fatal("пропуск после Feed принят")
	}
}
