package proc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// clockTicks — USER_HZ: поле starttime в /proc/<pid>/stat идёт в этих тиках.
const clockTicks = 100

// Proc — процесс из /proc. Окружение, командная строка, cgroup и cwd читаются
// только у процессов своего uid: чужое окружение не прочитать.
type Proc struct {
	Pid       int
	Ppid      int
	UID       int
	Comm      string
	Args      string    // командная строка через пробел
	StartRaw  string    // поле 22 /proc/<pid>/stat как есть: сверка, что pid не переиспользован
	Start     uint64    // оно же числом
	StartAt   time.Time // момент старта
	Cgroup    string    // путь cgroup v2 ("0::" отрезано)
	Cwd       string
	Session   int    // CLAUDE_PID: pid сессии Claude Code, под которой запущен процесс
	SessionID string // CLAUDE_CODE_SESSION_ID
	PidNS     uint64 // inode pid namespace (/proc/<pid>/ns/pid); 0 — не прочитался
	NSpid     []int  // pid в каждом pid namespace от корневого к своему (NSpid в status)
	Partial   string // свой процесс, но окружение или cgroup не прочитались (недампируемый): почему
}

// Snapshot — все процессы на момент Now.
type Snapshot struct {
	Now   time.Time
	Procs map[int]*Proc
	byNS  map[nsPid]*Proc // процессы по pid в своём namespace; строит Holder
}

// nsPid — pid процесса в его собственном pid namespace.
type nsPid struct {
	ns  uint64
	pid int
}

// Holder — процесс сессии Claude Code, под которой запущен p (CLAUDE_PID), или nil.
// CLAUDE_PID — pid сессии в её pid namespace, а p живёт в нём же или во вложенном
// (песочница bwrap --unshare-pid, контейнер): сначала ищется процесс с этим pid в
// namespace p, затем на хосте — среди процессов того же uid (на хосте pid сессии
// песочницы занят чужим, часто потоком ядра). Сессия в промежуточном namespace (песочница в
// песочнице) не находится. Что pid не переиспользован, сверяет вызывающий по Start.
// Не для одновременных вызовов: индекс строится при первом.
func (s *Snapshot) Holder(p *Proc) *Proc {
	if p.Session == 0 {
		return nil
	}
	if s.byNS == nil {
		s.byNS = map[nsPid]*Proc{}
		for _, q := range s.Procs {
			if q.PidNS != 0 && len(q.NSpid) > 0 {
				s.byNS[nsPid{q.PidNS, q.NSpid[len(q.NSpid)-1]}] = q
			}
		}
	}
	if q, ok := s.byNS[nsPid{p.PidNS, p.Session}]; ok && p.PidNS != 0 {
		return q
	}
	if q, ok := s.Procs[p.Session]; ok && q.UID == p.UID {
		return q
	}
	return nil
}

// ReadSnapshot читает procRoot; подробности (окружение, cgroup) — у процессов uid.
// Процессы, ушедшие во время чтения, пропускаются; прочие ошибки чтения отдельных
// процессов возвращаются вместе со снимком.
func ReadSnapshot(procRoot string, uid int, now time.Time) (*Snapshot, error) {
	boot, err := BootTime(procRoot)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("список процессов: %w", err)
	}
	s := &Snapshot{Now: now, Procs: map[int]*Proc{}}
	var errs []error
	for _, e := range ents {
		if !isPid(e.Name()) {
			continue // не процесс: /proc/self, /proc/meminfo и т. п.
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			return nil, fmt.Errorf("pid %q: %w", e.Name(), err)
		}
		p, err := Read(procRoot, pid, uid, boot)
		if Gone(err) {
			continue // процесс завершился между списком и чтением
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.Procs[pid] = p
	}
	return s, errors.Join(errs...)
}

// Gone — ошибка значит, что процесс уже завершился.
func Gone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// isPid — имя папки /proc из одних цифр.
func isPid(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// BootTime — время загрузки системы (btime из /proc/stat).
func BootTime(procRoot string) (time.Time, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		return time.Time{}, fmt.Errorf("время загрузки: %w", err)
	}
	for l := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "btime "); ok {
			sec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, fmt.Errorf("btime: %w", err)
			}
			return time.Unix(sec, 0), nil
		}
	}
	return time.Time{}, errors.New("в /proc/stat нет btime")
}

// Read читает один процесс; boot — из BootTime. Процесс завершился — ошибка,
// для которой Gone верно. Свой процесс, у которого не прочиталось окружение
// (EACCES у недампируемого), отдаётся с причиной в Partial.
func Read(procRoot string, pid, uid int, boot time.Time) (*Proc, error) {
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("pid %d: %w", pid, err)
	}
	st, err := ReadStat(procRoot, pid)
	if err != nil {
		return nil, fmt.Errorf("pid %d: %w", pid, err)
	}
	start, err := strconv.ParseUint(st.Start, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("pid %d: starttime: %w", pid, err)
	}
	p := &Proc{
		Pid: pid, Ppid: st.Ppid, UID: -1, Comm: st.Comm, StartRaw: st.Start, Start: start,
		StartAt: boot.Add(time.Duration(start) * time.Second / clockTicks),
	}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		p.UID = int(sys.Uid)
	}
	if p.UID != uid {
		return p, nil // чужое окружение не прочитать
	}
	if err := p.readOwn(dir); err != nil {
		if Gone(err) {
			return nil, err
		}
		p.Partial = err.Error()
	}
	return p, nil
}

// readOwn дочитывает окружение, командную строку, cgroup и cwd своего процесса.
// Поток ядра или зомби даёт пустые файлы — это не ошибка.
func (p *Proc) readOwn(dir string) error {
	env, err := os.ReadFile(filepath.Join(dir, "environ"))
	if err != nil {
		return fmt.Errorf("environ: %w", err)
	}
	for kv := range strings.SplitSeq(string(env), "\x00") {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_PID="); ok {
			if p.Session, err = strconv.Atoi(v); err != nil {
				return fmt.Errorf("CLAUDE_PID=%q: %w", v, err)
			}
		} else if v, ok := strings.CutPrefix(kv, "CLAUDE_CODE_SESSION_ID="); ok {
			p.SessionID = v
		}
	}
	cmd, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return fmt.Errorf("cmdline: %w", err)
	}
	p.Args = strings.TrimSpace(strings.ReplaceAll(string(cmd), "\x00", " "))
	cg, err := os.ReadFile(filepath.Join(dir, "cgroup"))
	if err != nil {
		return fmt.Errorf("cgroup: %w", err)
	}
	p.Cgroup = ParseCgroup(string(cg))
	if p.Cwd, err = os.Readlink(filepath.Join(dir, "cwd")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cwd: %w", err)
	}
	if p.PidNS, err = readPidNS(dir); err != nil {
		return err
	}
	p.NSpid, err = readNSpid(dir)
	return err
}

// readPidNS — inode из ссылки ns/pid вида «pid:[4026531836]»; ссылки нет — 0.
func readPidNS(dir string) (uint64, error) {
	link, err := os.Readlink(filepath.Join(dir, "ns", "pid"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("ns/pid: %w", err)
	}
	v, pre := strings.CutPrefix(link, "pid:[")
	v, suf := strings.CutSuffix(v, "]")
	if !pre || !suf {
		return 0, fmt.Errorf("ns/pid: %q", link)
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("ns/pid %q: %w", link, err)
	}
	return n, nil
}

// readNSpid — строка NSpid из status: pid процесса в каждом pid namespace, от корневого к своему.
func readNSpid(dir string) ([]int, error) {
	data, err := os.ReadFile(filepath.Join(dir, "status"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	for line := range strings.Lines(string(data)) {
		v, ok := strings.CutPrefix(line, "NSpid:")
		if !ok {
			continue
		}
		var out []int
		for f := range strings.FieldsSeq(v) {
			n, err := strconv.Atoi(f)
			if err != nil {
				return nil, fmt.Errorf("status NSpid %q: %w", strings.TrimSpace(line), err)
			}
			out = append(out, n)
		}
		return out, nil
	}
	return nil, nil
}
