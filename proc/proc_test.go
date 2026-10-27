package proc

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestParseStat(t *testing.T) {
	// comm со скобками и пробелом
	s := "123 (my (odd) proc) S 45 123 123 0 -1 4194560 1 0 0 0 0 0 0 0 20 0 1 0 26733327 1000 10\n"
	st, err := ParseStat(s)
	if err != nil || st.Comm != "my (odd) proc" || st.State != "S" || st.Ppid != 45 || st.Start != "26733327" {
		t.Errorf("ParseStat: %+v %v", st, err)
	}
	if _, err := ParseStat("garbage"); err == nil {
		t.Error("мусор разобран")
	}
}

func TestServiceUnit(t *testing.T) {
	for cg, want := range map[string]string{
		"/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-main-12.scope": "",
		"/user.slice/user-1000.slice/user@1000.service/app.slice/tmpguard.service":       "tmpguard.service",
		"/system.slice/sshd.service":                               "sshd.service",
		"/user.slice/user-1000.slice/user@1000.service/init.scope": "",
	} {
		if got := ServiceUnit(cg); got != want {
			t.Errorf("ServiceUnit(%q) = %q, хотим %q", cg, got, want)
		}
	}
}

func TestScopeName(t *testing.T) {
	cases := map[string]struct {
		name string
		pid  int
		ok   bool
	}{
		"agent-main-3548014.scope":       {"main", 3548014, true},
		"agent-demo-agent-12.scope":      {"demo-agent", 12, true},
		"agent-x.scope":                  {"", 0, false},
		"app-org.kde.konsole-5028.scope": {"", 0, false},
		"agent-main-12.service":          {"", 0, false},
		"agent--12.scope":                {"", 0, false},
		"agent-main-1x.scope":            {"", 0, false},
		"agent-main-666971-r1.scope":     {"main", 666971, true},
		"agent-demo-agent-12-r13.scope":  {"demo-agent", 12, true},
		"agent-main-12-r.scope":          {"", 0, false},
		"agent-main-r1.scope":            {"", 0, false},
	}
	for in, want := range cases {
		name, pid, ok := ScopeName(in)
		if name != want.name || pid != want.pid || ok != want.ok {
			t.Errorf("ScopeName(%q) = %q %d %v, хотим %+v", in, name, pid, ok, want)
		}
	}
	if name, pid, ok := SessionScope("/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-main-12-r2.scope"); !ok || name != "main" || pid != 12 {
		t.Errorf("SessionScope после перезапуска: %q %d %v", name, pid, ok)
	}
	name, pid, ok := SessionScope("/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-main-12.scope")
	if name != "main" || pid != 12 || !ok {
		t.Errorf("SessionScope: %q %d %v", name, pid, ok)
	}
	if _, _, ok := SessionScope("/user.slice/user-1000.slice/user@1000.service/init.scope"); ok {
		t.Error("init.scope — не сессия")
	}
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadSnapshot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "stat"), "cpu 1 2 3\nbtime 1000\n")
	write(t, filepath.Join(root, "meminfo"), "MemTotal: 1 kB\n") // не процесс
	d := filepath.Join(root, "42")
	write(t, filepath.Join(d, "stat"), "42 (go) S 7 42 42 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 500 0 0\n")
	write(t, filepath.Join(d, "environ"), "HOME=/h\x00CLAUDE_PID=7\x00CLAUDE_CODE_SESSION_ID=s-1\x00")
	write(t, filepath.Join(d, "cmdline"), "go\x00test\x00./...\x00")
	write(t, filepath.Join(d, "cgroup"), "0::/user.slice/agents.slice/agent-main-7.scope\n")
	if err := os.Symlink("/work", filepath.Join(d, "cwd")); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2000, 0)
	s, err := ReadSnapshot(root, os.Getuid(), now)
	if err != nil {
		t.Fatal(err)
	}
	p := s.Procs[42]
	if len(s.Procs) != 1 || p == nil {
		t.Fatalf("процессы: %+v", s.Procs)
	}
	if p.Ppid != 7 || p.Comm != "go" || p.Args != "go test ./..." || p.Session != 7 || p.SessionID != "s-1" ||
		p.Cwd != "/work" || p.Cgroup != "/user.slice/agents.slice/agent-main-7.scope" || p.Partial != "" ||
		!p.StartAt.Equal(time.Unix(1005, 0)) || p.StartRaw != "500" {
		t.Errorf("процесс: %+v", p)
	}
	// чужой uid: окружение не читается
	s, err = ReadSnapshot(root, os.Getuid()+1, now)
	if err != nil || s.Procs[42].Session != 0 || s.Procs[42].Args != "" {
		t.Errorf("чужой процесс: %+v %v", s.Procs[42], err)
	}
	if _, err := Read(root, 99, os.Getuid(), time.Unix(1000, 0)); !Gone(err) {
		t.Errorf("нет процесса — Gone: %v", err)
	}
}

// Сессия в песочнице bwrap --unshare-pid: CLAUDE_PID — её pid внутри, на хосте этот pid чужой или пуст.
func TestHolderPidNamespace(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "stat"), "cpu 1 2 3\nbtime 1000\n")
	proc := func(pid, ppid int, ns, nspid, env string) {
		d := filepath.Join(root, strconv.Itoa(pid))
		write(t, filepath.Join(d, "stat"), fmt.Sprintf("%d (x) S %d 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 500 0 0\n", pid, ppid))
		write(t, filepath.Join(d, "environ"), env)
		write(t, filepath.Join(d, "cmdline"), "x\x00")
		write(t, filepath.Join(d, "cgroup"), "0::/user.slice\n")
		write(t, filepath.Join(d, "status"), "Name:\tx\nNSpid:\t"+nspid+"\nVmRSS:\t1 kB\n")
		if err := os.MkdirAll(filepath.Join(d, "ns"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("pid:["+ns+"]", filepath.Join(d, "ns", "pid")); err != nil {
			t.Fatal(err)
		}
	}
	proc(800, 1, "1", "800", "CLAUDE_PID=700\x00")    // сессия хоста 700 мертва
	proc(900, 1, "2", "900\t12", "")                  // claude в песочнице: pid 12 внутри
	proc(901, 1, "2", "901\t30", "CLAUDE_PID=12\x00") // его фоновая команда
	proc(12, 1, "1", "12", "")                        // pid 12 хоста — другой процесс
	proc(950, 1, "3", "950\t5", "CLAUDE_PID=12\x00")  // другая песочница: своего 12 нет — берётся хост
	proc(960, 1, "4", "960\t1", "CLAUDE_PID=900\x00") // помощник в своём namespace, сессия на хосте
	s, err := ReadSnapshot(root, os.Getuid(), time.Unix(2000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if p := s.Procs[901]; p.PidNS != 2 || !slices.Equal(p.NSpid, []int{901, 30}) {
		t.Fatalf("namespace: %+v", p)
	}
	for pid, want := range map[int]int{800: 0, 901: 900, 950: 12, 960: 900} {
		got := 0
		if h := s.Holder(s.Procs[pid]); h != nil {
			got = h.Pid
		}
		if got != want {
			t.Errorf("Holder(%d) = %d, ждали %d", pid, got, want)
		}
	}
	if s.Holder(s.Procs[900]) != nil {
		t.Error("процесс без CLAUDE_PID — без сессии")
	}
}
