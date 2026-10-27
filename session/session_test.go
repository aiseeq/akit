package session

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeProc(t *testing.T, root string, pid, start string) {
	t.Helper()
	d := filepath.Join(root, pid)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := pid + " (claude) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 " + start + " 0 0\n"
	if err := os.WriteFile(filepath.Join(d, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryAlive(t *testing.T) {
	root := t.TempDir()
	reg := Registry{Dir: filepath.Join(root, "sessions"), ProcRoot: filepath.Join(root, "proc")}
	if err := os.MkdirAll(reg.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"10.json": `{"pid":10,"procStart":"500","name":"main","startedAt":1,"messagingSocketPath":"/s/10.sock",` +
			`"kind":"interactive","status":"idle","statusUpdatedAt":1792978244330}`,
		"11.json":     `{"pid":11,"procStart":"600","name":"main","startedAt":2,"messagingSocketPath":"/s/11.sock"}`, // pid переиспользован
		"12.json":     `{"pid":12,"procStart":"700","name":"builder","startedAt":3}`,
		"13.json":     `{"pid":13,"procStart":"800","name":"dead"}`,
		"13.abcd.key": `{"peerToken":"tok13"}`,
		"x.json":      `{}`,
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(reg.Dir, n), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeProc(t, reg.ProcRoot, "10", "500")
	writeProc(t, reg.ProcRoot, "11", "999")
	writeProc(t, reg.ProcRoot, "12", "700")
	alive, err := reg.Alive()
	if err != nil {
		t.Fatal(err)
	}
	if alive["main"].Pid != 10 || alive["builder"].Pid != 12 || len(alive) != 2 {
		t.Errorf("живые: %+v", alive)
	}
	if reg.IsAlive(13, "800") {
		t.Error("мёртвая сессия жива")
	}
	if tok, err := reg.Token(13); err != nil || tok != "tok13" {
		t.Errorf("токен 13: %q %v", tok, err)
	}
	if tok, err := reg.Token(10); err != nil || tok != "" {
		t.Errorf("нет файла — пусто без ошибки: %q %v", tok, err)
	}
	if in, err := reg.Get(10); err != nil || in.Kind != "interactive" || in.StatusUpdatedAt != 1792978244330 {
		t.Errorf("поля реестра 10: %+v %v", in, err)
	}
	if in, err := reg.Get(99); in != nil || err != nil {
		t.Errorf("нет записи — nil без ошибки: %v %v", in, err)
	}
}

func TestDescribe(t *testing.T) {
	projects := t.TempDir()
	write := func(dir, id, data string, mtime time.Time) string {
		t.Helper()
		p := filepath.Join(projects, dir, id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	now := time.Now()
	write("-home-a-old", "s1", `{"type":"x","cwd":"/home/a/old"}`+"\n", now.Add(-time.Hour))
	p := write("-home-a-git-x", "s1", `{"type":"custom-title","cwd":null}`+"\n"+`{"type":"user","cwd":"/home/a/git/x"}`+"\n", now)
	tr, err := Describe(projects, "s1")
	if err != nil || tr.Path != p || tr.Cwd != "/home/a/git/x" || tr.ID != "s1" {
		t.Errorf("Describe: %+v %v", tr, err)
	}
	write("-home-a", "s2", `{"type":"mode"}`+"\n", now)
	if tr, err := Describe(projects, "s2"); err != nil || tr.Cwd != "" {
		t.Errorf("без cwd — пусто без ошибки: %+v %v", tr, err)
	}
	if _, err := Describe(projects, "нет"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("нет транскрипта — ErrNotExist: %v", err)
	}
	if _, err := Describe(projects, "../s1"); err == nil {
		t.Error("id с папкой принят")
	}
}
