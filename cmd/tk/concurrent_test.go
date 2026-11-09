package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// Notes written at the same time must all land in the journal and leave a valid file: a plain
// truncate-and-write lets a longer earlier write leave its tail behind and a later one drop the earlier note.
func TestConcurrentNotesKeepEveryNote(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "20200101-1000-LOW-race.md")
	if err := os.WriteFile(path, []byte("# Race\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n- 2020-01-01 10:00 открыт — A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const writers = 40
	var wg sync.WaitGroup
	codes := make([]int, writers)
	errs := make([]string, writers)
	for i := range writers {
		wg.Go(func() {
			// Notes of different length, with multi-byte text, so a leftover tail would split a rune.
			text := fmt.Sprintf("заметка-%02d %s", i, strings.Repeat("ж", i*7))
			o, err := parseArgs([]string{"note", "race", text, "--by", "A"})
			if err != nil {
				errs[i] = err.Error()
				codes[i] = -1
				return
			}
			var out, errOut bytes.Buffer
			codes[i] = runAtBase(base, dir, o, &out, &errOut)
			errs[i] = errOut.String()
		})
	}
	wg.Wait()
	for i, code := range codes {
		if code != 0 {
			t.Errorf("writer %d: code %d: %s", i, code, errs[i])
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(b) {
		t.Fatalf("ticket is not valid UTF-8 after concurrent notes:\n%q", b)
	}
	for i := range writers {
		if !strings.Contains(string(b), fmt.Sprintf("заметка-%02d ", i)) {
			t.Errorf("note %d lost", i)
		}
	}
	if n := strings.Count(string(b), "## Журнал"); n != 1 {
		t.Errorf("journal heading %d times", n)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".*"))
	if err != nil || len(leftovers) != 0 {
		t.Errorf("temporary files left in the task folder: %v %v", leftovers, err)
	}
}

// A symlinked ticket is updated at its target and the link survives; a hard-linked one is refused rather
// than split by the rename.
func TestWriteTaskKeepsLinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "store", "20200101-1000-LOW-linked.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "20200101-1000-LOW-linked.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeTask(link, "new\n"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced: %v %v", info, err)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "new\n" {
		t.Fatalf("target not updated: %q %v", b, err)
	}
	if st, err := os.Stat(target); err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("target mode changed: %v %v", st, err)
	}
	hard := filepath.Join(dir, "20200101-1000-LOW-hard.md")
	if err := os.Link(target, hard); err != nil {
		t.Fatal(err)
	}
	if err := writeTask(hard, "split\n"); err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("hard-linked ticket written: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "new\n" {
		t.Fatalf("refused write changed the ticket: %q", b)
	}
}

// A new ticket gets fileMode less the umask, as os.WriteFile gave it.
func TestWriteTaskNewHonorsUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	path := filepath.Join(t.TempDir(), "20200101-1000-LOW-private.md")
	if err := writeTask(path, "x\n"); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("new ticket mode %v, want 0600: %v", st.Mode().Perm(), err)
	}
}

func TestConcurrentStartsAssignTicketToOneActor(t *testing.T) {
	base := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	gatePath := filepath.Join(home, ".local", "state", "queue", "gate.json")
	if err := os.MkdirAll(filepath.Dir(gatePath), 0o755); err != nil {
		t.Fatal(err)
	}
	gate := fmt.Sprintf(`{"at":%q,"live":["%s"],"open":[],"open_by_root":{%q:["MED-DO","MED-HUNT","MED-ARCH"]}}`, time.Now().Format(time.RFC3339), filepath.Base(base), base)
	if err := os.WriteFile(gatePath, []byte(gate), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := readTaskGate(base)
	if err != nil {
		t.Fatal(err)
	}
	if !gateVisible(task{status: "открыт", prio: "MED", diff: "DO"}, state) {
		t.Fatalf("test gate is closed: %#v", state)
	}
	dir := filepath.Join(base, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "20200101-1000-MED-race.md")
	body := "# Race\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n- 2020-01-01 10:00 открыт — author\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	codes := make([]int, 2)
	errs := make([]string, 2)
	for i := range 2 {
		wg.Go(func() {
			o := options{cmd: "start", id: "race", by: fmt.Sprintf("actor-%d", i)}
			var out, errOut bytes.Buffer
			codes[i] = runAtBase(base, dir, o, &out, &errOut)
			errs[i] = errOut.String()
		})
	}
	wg.Wait()
	if !((codes[0] == 0 && codes[1] == exitNone) || (codes[1] == 0 && codes[0] == exitNone)) {
		t.Fatalf("start codes = %v, errors = %q", codes, errs)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	owner := executor(string(data))
	if owner != "actor-0" && owner != "actor-1" {
		t.Fatalf("ticket has unexpected executor %q: %s", owner, data)
	}
}
