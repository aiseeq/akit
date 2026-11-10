package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// tk ls --json gives, per ticket, the time of the last journal record of each
// author: a watcher sees that a master keeps working an open ticket.
func TestListJSONActorsLastRecordByAuthor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	dir := filepath.Join(project, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# subject\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n" +
		"- 2026-11-07 15:00 открыт — lead\n" +
		"- 2026-11-07 16:30 note — dream-master: first look\n" +
		"- 2026-11-07 16:40 ждёт — dream-master — 2026-11-07 16:50\n" +
		"- 2026-11-07 16:45 ждёт — tester — 2026-11-07 18:00\n" +
		"- 2026-11-07 16:46 в работе — doer\n" +
		"- 2026-11-07 16:55 note — dream-master: a reply quoting «note — lead» and — lead: nothing\n" +
		"- 2026-11-07 17:34 note — aguard: alert\n"
	if err := os.WriteFile(filepath.Join(dir, "20261107-1500-HIGH-subject.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := tkIn(t, project, "", "ls", "--json")
	if code != 0 {
		t.Fatalf("ls exit %d: %s", code, errOut)
	}
	var rows []struct {
		Slug   string            `json:"slug"`
		Actors map[string]string `json:"actors"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: %s", out)
	}
	want := map[string]string{"lead": "2026-11-07 15:00", "dream-master": "2026-11-07 16:55", "aguard": "2026-11-07 17:34",
		"tester": "2026-11-07 16:45", "doer": "2026-11-07 16:46"}
	got := rows[0].Actors
	if len(got) != len(want) {
		t.Errorf("actors %v, want %v", got, want)
	}
	for who, at := range want {
		if got[who] != at {
			t.Errorf("actors[%q] = %q, want %q (all: %v)", who, got[who], at, got)
		}
	}
}
