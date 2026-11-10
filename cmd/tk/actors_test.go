package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// tk ls --json gives, per ticket, the time of the latest journal record of each
// author: a watcher sees that a master keeps working an open ticket.
func TestListJSONActorsLastRecordByAuthor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	dir := filepath.Join(project, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# subject\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n" +
		"- 2030-01-02 09:00 открыт — opener\n" +
		"- 2030-01-02 10:30 note — alpha-master: first look\n" +
		"- 2030-01-02 10:40 ждёт — alpha-master — 2030-01-02 10:50\n" +
		"- 2030-01-02 10:45 ждёт — tester — 2030-01-02 12:00\n" +
		"- 2030-01-02 10:46 в работе — doer\n" +
		"- 2030-01-02 10:55 note — alpha-master: a reply quoting «note — opener» and — opener: nothing\n" +
		"- 2030-01-02 10:50 note — alpha-master: written by hand, back in time\n" +
		"- 2030-01-02 11:00 готово — checker (session beta-master): ok: verified\n" +
		"- 2030-01-02 11:05 note — odd:: colon name\n" +
		"- 2030-01-02 11:06 в работе — odd:\n" +
		"- 2030-01-02 11:34 note — watcher: alert\n"
	if err := os.WriteFile(filepath.Join(dir, "20300102-0900-HIGH-subject.md"), []byte(body), 0o644); err != nil {
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
	want := map[string]string{"opener": "2030-01-02 09:00", "alpha-master": "2030-01-02 10:55", "watcher": "2030-01-02 11:34",
		"tester": "2030-01-02 10:45", "doer": "2030-01-02 10:46", "checker": "2030-01-02 11:00", "beta-master": "2030-01-02 11:00",
		"odd:": "2030-01-02 11:06"}
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
