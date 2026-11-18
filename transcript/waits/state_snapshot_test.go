package waits

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/aiseeq/akit/price"
	"github.com/aiseeq/akit/transcript"
)

func snapshotResumeAt(t *testing.T, s *events, cut int) []Wait {
	t.Helper()
	r := New()
	var got []Wait
	for _, ev := range s.evs[:cut] {
		got = append(got, r.Add(ev)...)
	}
	open := r.OpenStates()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	r = New()
	if err := json.Unmarshal(b, r); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.OpenStates(), open) {
		t.Fatalf("OpenStates после восстановления = %+v, до = %+v", r.OpenStates(), open)
	}
	for _, ev := range s.evs[cut:] {
		got = append(got, r.Add(ev)...)
	}
	return append(got, r.Close()...)
}

func TestSnapshotPreservesRecognitionDetails(t *testing.T) {
	t.Run("repeat call time", func(t *testing.T) {
		s := newEvents()
		cmd := `timeout 60 bash -c 'until [ -f /t/x ]; do sleep 5; done'`
		a := s.bash(cmd, 0, "r1", false)
		s.out(a, "", 60)
		b := s.bash(cmd, 61, "r1", false)
		s.out(b, "", 90) // второй вызов длится 29 секунд: Done, не Timeout
		s.bash("ls", 91, "r2", false)
		got := snapshotResumeAt(t, s, 2)
		if len(got) != 1 || got[0].Outcome != Done || got[0].Calls != 2 || got[0].Tokens != 100 {
			t.Fatalf("повторный расход после восстановления: %+v", got)
		}
		got = snapshotResumeAt(t, s, 3)
		if len(got) != 1 || got[0].Outcome != Done || got[0].Calls != 2 || got[0].Tokens != 100 || got[0].End.Sub(got[0].Start) != 90*time.Second {
			t.Fatalf("повтор после восстановления: %+v", got)
		}
	})

	t.Run("codex session", func(t *testing.T) {
		s := newEvents()
		a := s.tool("exec", "wfor exit 42 -t 10m", "x", 0)
		s.out(a, `{"session_id":7,"output":""}`, 30)
		b := s.tool("exec", `const r = await tools.write_stdin({session_id:7,chars:"",yield_time_ms:30000})`, "x", 31)
		s.out(b, `{"exit_code":0,"output":"wfor: done in 1m"}`, 45)
		r := New()
		for _, ev := range s.evs[:2] {
			r.Add(ev)
		}
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		r = New()
		if err := json.Unmarshal(data, r); err != nil {
			t.Fatal(err)
		}
		p := r.polls["7"]
		if p == nil || p.session != "7" || !p.codex {
			t.Fatalf("не восстановлены поля codex ожидания: %+v", p)
		}
		var got []Wait
		for _, ev := range s.evs[2:] {
			got = append(got, r.Add(ev)...)
		}
		got = append(got, r.Close()...)
		if len(got) != 1 || got[0].How != Wfor || got[0].Calls != 2 || got[0].Outcome != Done {
			t.Fatalf("codex poll после восстановления: %+v", got)
		}
	})

	t.Run("sends order", func(t *testing.T) {
		s := newEvents()
		s.tool("SendMessage", "first", map[string]string{"to": "main"}, 0)
		s.tool("SendMessage", "second", map[string]string{"to": "main"}, 1)
		s.add(transcript.Event{Role: transcript.Peer, Who: "main", From: "main"}, 2)
		got := snapshotResumeAt(t, s, 2)
		if len(got) != 2 || got[0].Line != 1 || got[1].Line != 2 || got[0].Outcome != Done || got[1].Outcome != Done {
			t.Fatalf("порядок Send после восстановления: %+v", got)
		}
	})
}

func TestSnapshotPreservesLimit(t *testing.T) {
	s := newEvents()
	first := s.bash(`timeout 60 bash -c 'while true; do sleep 5; done'`, 0, "r1", false)
	s.out(first, "", 60)
	second := s.bash(`timeout 90 bash -c 'while true; do sleep 5; done'`, 61, "r2", false)
	s.out(second, "", 150) // 89 секунд из 90-секундного лимита => Timeout
	s.bash("ls", 151, "r3", false)
	got := snapshotResumeAt(t, s, 3)
	if len(got) != 1 || got[0].Outcome != Timeout || got[0].Calls != 2 {
		t.Fatalf("лимит после восстановления: %+v", got)
	}
}

func TestSnapshotPreservesAuxiliaryRecognizerState(t *testing.T) {
	r := New()
	r.tasks["task"] = "go test ./..."
	r.shellText["call"] = "go build ./..."
	r.sessionCmd["17"] = "go vet ./..."
	r.prevCmd = "go test ./cmd/..."
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	got := New()
	if err := json.Unmarshal(b, got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.tasks, r.tasks) || !reflect.DeepEqual(got.shellText, r.shellText) ||
		!reflect.DeepEqual(got.sessionCmd, r.sessionCmd) || got.prevCmd != r.prevCmd {
		t.Fatalf("вспомогательное состояние потеряно: %+v", got)
	}
}

func TestSnapshotIndexRejectsUnknownPending(t *testing.T) {
	p := &pending{}
	if id, err := pendingIndex(map[*pending]int{}, p); err == nil || id != 0 {
		t.Fatalf("неизвестный pending: id=%d err=%v", id, err)
	}
	if id, err := pendingIndex(map[*pending]int{p: 4}, p); err != nil || id != 4 {
		t.Fatalf("известный pending: id=%d err=%v", id, err)
	}
	if id, err := pendingIndex(nil, nil); err != nil || id != -1 {
		t.Fatalf("nil pending: id=%d err=%v", id, err)
	}
}

func TestSnapshotOpenStatesSortsMapBackedWaits(t *testing.T) {
	for trial := range 20 {
		r := New()
		for i := 1; i <= 8; i++ {
			p := &pending{w: Wait{Line: i, CallIDs: []string{fmt.Sprintf("c%d", i)}}}
			r.byCall[p.w.CallIDs[0]] = p
			r.bg[p.w.CallIDs[0]] = p
		}
		states := r.OpenStates()
		for i, st := range states {
			if st.Line != i+1 {
				t.Fatalf("проба %d: OpenStates не отсортированы: %+v", trial, states)
			}
		}
	}
}

func TestSnapshotNullMapsAndRepliesCannotPanic(t *testing.T) {
	const emptyMaps = `{"version":1,"by_call":null,"bg":null,"polls":null,"tasks":null,"shell_text":null,"session_cmd":null,"sends":null,"cur":-1,"wakeup":-1,"tokens_to":-1}`
	r := New()
	if err := json.Unmarshal([]byte(emptyMaps), r); err != nil {
		t.Fatal(err)
	}
	s := newEvents()
	s.tool("exec_command", "go list ./...", map[string]any{}, 0)
	s.out("c1", "Process running with session ID 17", 1)
	s.bash("go test ./...", 2, "r1", true)
	s.out("c2", "Command running in background with ID: b7.", 3)
	for _, ev := range s.evs {
		r.Add(ev)
	}
	if r.sessionCmd["17"] != "go list ./..." || r.tasks["b7"] == "" {
		t.Fatalf("null-карты не восстановились: session=%v tasks=%v", r.sessionCmd, r.tasks)
	}
	r.Close()

	const nullReplies = `{"version":1,"pending":[{"wait":{"How":"sleep","Calls":1,"Line":1,"CallIDs":["a"]},"replies":null}],"cur":0,"wakeup":-1,"tokens_to":-1}`
	r = New()
	if err := json.Unmarshal([]byte(nullReplies), r); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"command": "sleep 30"})
	r.Add(transcript.Event{Role: transcript.Cmd, Who: "Bash", Text: "sleep 30", CallID: "b", Raw: raw,
		Reply: &transcript.Reply{ID: "reply", Usage: &price.Usage{Output: 1}}})
	r.Close()
}
