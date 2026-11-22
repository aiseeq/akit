package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aiseeq/akit/proc"
	"golang.org/x/sys/unix"
)

// cond — одно условие ожидания. check отвечает, выполнено ли оно сейчас; ошибка — ждать дальше нельзя (нет прав,
// кривой файл). state — что видно сейчас, для ответа по таймауту и об исполнении.
type cond interface {
	check(ctx context.Context) (bool, error)
	String() string
	state() string
}

// fileCond — файл есть и не пуст.
type fileCond struct {
	path string
	size int64
	seen bool
}

func (c *fileCond) check(context.Context) (bool, error) {
	st, err := os.Stat(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		c.seen = false
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s: %w", c, err)
	}
	c.seen, c.size = true, st.Size()
	return st.Size() > 0, nil
}

func (c *fileCond) String() string { return "file " + c.path }

func (c *fileCond) state() string {
	if !c.seen {
		return "absent"
	}
	return fmt.Sprintf("%d bytes", c.size)
}

// goneCond — пути нет.
type goneCond struct {
	path string
	size int64
	gone bool
}

func (c *goneCond) check(context.Context) (bool, error) {
	st, err := os.Lstat(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		c.gone = true
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s: %w", c, err)
	}
	c.size = st.Size()
	return false, nil
}

func (c *goneCond) String() string { return "gone " + c.path }

func (c *goneCond) state() string {
	if c.gone {
		return "removed"
	}
	return fmt.Sprintf("still there, %d bytes", c.size)
}

// lineCond — в файле есть строка по регулярному выражению. Файл читается с места прошлой проверки; заменён (mv,
// перезапись на месте) — читается сначала. Недописанная последняя строка тоже проверяется.
type lineCond struct {
	re      *regexp.Regexp
	path    string
	off     int64       // сколько байт файла уже прочитано целыми строками
	file    os.FileInfo // прочитанный файл: другой inode — файл заменён
	head    []byte      // начало прочитанного и
	edge    []byte      // байты перед off: не совпали — файл перезаписан на месте
	lines   int         // номер последней целой строки
	tail    []byte      // недописанная последняя строка
	last    string      // последняя непустая строка — для ответа
	hit     string
	hitLine int
	absent  bool
	skipped bool // начато с --new или --after: повтор вызова должен продолжить с той же строки
	// partial — с --new файл кончался неполной строкой: она начата до старта и не в счёт, пока не кончится
	partial bool
}

// watchFiles shares one inotify descriptor across every file and line condition.
func watchFiles(ctx context.Context, conds []cond) (<-chan struct{}, func()) {
	dirs := fileWatchDirs(conds)
	if len(dirs) == 0 {
		return nil, func() {}
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil { //nolint:silent-error-handling // Event support is optional; ticker checks remain the fallback.
		return nil, func() {}
	}
	byWD := addInotifyWatches(fd, dirs)
	if len(byWD) == 0 {
		_ = unix.Close(fd)
		return nil, func() {}
	}
	cancelFD, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil { //nolint:silent-error-handling // Cancellation support is optional; ticker checks remain the fallback.
		_ = unix.Close(fd)
		return nil, func() {}
	}
	out := make(chan struct{}, 1)
	done := make(chan struct{})
	stop := cancelPollOnContext(ctx, cancelFD)
	go watchInotify(fd, cancelFD, byWD, out, done)
	cleanup := func() {
		stop()
		signalEventFD(cancelFD)
		closeAfterWatcher(done, func() {
			_ = unix.Close(fd)
			_ = unix.Close(cancelFD)
		})
	}
	return out, cleanup
}

func fileWatchDirs(conds []cond) map[string]map[string]struct{} {
	dirs := map[string]map[string]struct{}{}
	for _, c := range conds {
		var path string
		switch fc := c.(type) {
		case *fileCond:
			path = fc.path
		case *lineCond:
			path = fc.path
		default:
			continue
		}
		dir, name := filepath.Dir(path), filepath.Base(path)
		if dirs[dir] == nil {
			dirs[dir] = map[string]struct{}{}
		}
		dirs[dir][name] = struct{}{}
	}
	return dirs
}

func addInotifyWatches(fd int, dirs map[string]map[string]struct{}) map[int]map[string]struct{} {
	byWD := map[int]map[string]struct{}{}
	for dir, names := range dirs {
		wd, addErr := unix.InotifyAddWatch(fd, dir, unix.IN_CREATE|unix.IN_MODIFY|unix.IN_CLOSE_WRITE|unix.IN_MOVED_TO|unix.IN_ATTRIB)
		if addErr != nil { //nolint:silent-error-handling // Unavailable paths retain ticker checks.
			continue
		}
		if byWD[wd] == nil {
			byWD[wd] = map[string]struct{}{}
		}
		for name := range names {
			byWD[wd][name] = struct{}{}
		}
	}
	return byWD
}

func watchInotify(fd, cancelFD int, byWD map[int]map[string]struct{}, out chan<- struct{}, done chan<- struct{}) {
	defer close(done)
	buf := make([]byte, 4096)
	for waitInotify(fd, cancelFD) {
		for {
			n, err := unix.Read(fd, buf)
			if err == unix.EAGAIN {
				break
			}
			if err == unix.EINTR {
				continue
			}
			if err != nil { //nolint:silent-error-handling // A dead watcher falls back to the periodic condition check.
				return
			}
			emitInotifyEvents(buf[:n], byWD, out)
		}
	}
}

func waitInotify(fd, cancelFD int) bool {
	pollfds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}, {Fd: int32(cancelFD), Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(pollfds, -1)
		if err == unix.EINTR {
			continue
		}
		return err == nil && pollfds[1].Revents == 0
	}
}

func emitInotifyEvents(data []byte, byWD map[int]map[string]struct{}, out chan<- struct{}) {
	for off := 0; off+unix.SizeofInotifyEvent <= len(data); {
		wd := int(int32(binary.LittleEndian.Uint32(data[off : off+4])))
		length := int(binary.LittleEndian.Uint32(data[off+12 : off+16]))
		if off+unix.SizeofInotifyEvent+length > len(data) {
			return
		}
		name := string(bytes.TrimRight(data[off+unix.SizeofInotifyEvent:off+unix.SizeofInotifyEvent+length], "\x00"))
		_, knownWD := byWD[wd]
		_, knownName := byWD[wd][name]
		if wd == -1 || knownWD && knownName {
			select {
			case out <- struct{}{}:
			default:
			}
		}
		off += unix.SizeofInotifyEvent + length
	}
}

func signalEventFD(fd int) {
	var value [8]byte
	value[0] = 1
	if _, err := unix.Write(fd, value[:]); err != nil && err != unix.EAGAIN {
		return
	}
}

func cancelPollOnContext(ctx context.Context, fd int) func() {
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		signalEventFD(fd)
		close(finished)
	})
	return func() {
		if !stop() {
			<-finished
		}
	}
}

func closeAfterWatcher(done <-chan struct{}, closeFDs func()) {
	<-done
	closeFDs()
}

// markLen — сколько байт начала и края прочитанного сверяется, чтобы узнать перезапись на месте.
const markLen = 64

// newLineCond — условие line. onlyNew (--new): прежнее содержимое файла не в счёт, только дописанное после старта;
// after > 0 (--after N): не в счёт первые N строк — так повтор вызова продолжает с места прошлого.
func newLineCond(expr, path string, onlyNew bool, after int) (*lineCond, error) {
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("line %q: %w", expr, err)
	}
	c := &lineCond{re: re, path: path}
	if onlyNew || after > 0 {
		c.skipped = true
		if err := c.skipLines(onlyNew, after); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// skipLines пропускает все целые строки файла (all) или первые n; неполная последняя строка, которую они
// покрывают, пропускается целиком, когда допишется.
func (c *lineCond) skipLines(all bool, n int) error {
	f, err := os.Open(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", c, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("%s: %w", c, err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("%s: %w", c, err)
	}
	end := bytes.LastIndexByte(data, '\n') + 1
	lines := bytes.Count(data[:end], []byte("\n"))
	if !all && n < lines {
		end = 0
		for range n {
			end += bytes.IndexByte(data[end:], '\n') + 1
		}
		lines = n
	}
	c.file, c.lines = st, lines
	c.partial = end < len(data) && (all || n > lines)
	c.consumed(data[:end])
	// последняя строка пропущенного — в ответ по таймауту, пока не дописано новое
	for line := range bytes.Lines(data[:end]) {
		if s := strings.TrimSpace(string(line)); s != "" {
			c.last = s
		}
	}
	return nil
}

// consumed — прочитаны целые строки chunk: off вперёд, метки начала и края — по ним.
func (c *lineCond) consumed(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	if c.off == 0 {
		c.head = bytes.Clone(chunk[:min(markLen, len(chunk))])
	}
	c.off += int64(len(chunk))
	c.edge = bytes.Clone(chunk[max(0, len(chunk)-markLen):])
}

// replaced — файл уже не тот, что читали: другой inode, короче прочитанного или другие байты начала и края.
func (c *lineCond) replaced(f *os.File, st os.FileInfo) (bool, error) {
	if c.file != nil && !os.SameFile(c.file, st) || st.Size() < c.off {
		return true, nil
	}
	for _, m := range []struct {
		at   int64
		want []byte
	}{{0, c.head}, {c.off - int64(len(c.edge)), c.edge}} {
		got := make([]byte, len(m.want))
		if _, err := f.ReadAt(got, m.at); err != nil {
			return false, fmt.Errorf("%s: %w", c, err)
		}
		if !bytes.Equal(got, m.want) {
			return true, nil
		}
	}
	return false, nil
}

func (c *lineCond) check(ctx context.Context) (bool, error) {
	return c.checkContent(true)
}

func (c *lineCond) checkEvent() (bool, error) {
	return c.checkContent(false)
}

func (c *lineCond) checkContent(allowIncomplete bool) (bool, error) {
	f, err := os.Open(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		c.absent = true
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s: %w", c, err)
	}
	defer f.Close()
	c.absent = false
	st, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("%s: %w", c, err)
	}
	replaced, err := c.replaced(f, st)
	if err != nil {
		return false, err
	}
	if replaced {
		c.off, c.lines, c.tail, c.head, c.edge, c.partial = 0, 0, nil, nil, nil, false
	}
	c.file = st
	if _, err := f.Seek(c.off, io.SeekStart); err != nil {
		return false, fmt.Errorf("%s: %w", c, err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return false, fmt.Errorf("%s: %w", c, err)
	}
	end := bytes.LastIndexByte(data, '\n') + 1
	first := c.partial
	for line := range bytes.Lines(data[:end]) {
		c.lines++
		if first {
			first, c.partial = false, false
			continue
		}
		if c.match(bytes.TrimRight(line, "\r\n"), c.lines) {
			return true, nil
		}
	}
	c.consumed(data[:end])
	c.tail = data[end:]
	return allowIncomplete && len(c.tail) > 0 && !c.partial && c.match(c.tail, c.lines+1), nil
}

func (c *lineCond) match(line []byte, n int) bool {
	if s := strings.TrimSpace(string(line)); s != "" {
		c.last = s
	}
	if !c.re.Match(line) {
		return false
	}
	c.hit, c.hitLine = string(line), n
	return true
}

func (c *lineCond) String() string { return fmt.Sprintf("line '%s' in %s", c.re, c.path) }

// resume — сколько строк пропустить повтору (--after): прочитанные и начатая до старта неполная.
func (c *lineCond) resume() int {
	if c.partial {
		return c.lines + 1
	}
	return c.lines
}

func (c *lineCond) state() string {
	if c.hit != "" {
		return fmt.Sprintf("line %d: %s", c.hitLine, short(c.hit))
	}
	if c.absent {
		return "file absent"
	}
	if c.partial {
		return fmt.Sprintf("%d lines and one begun before the start, last: %s", c.lines, short(c.last))
	}
	return fmt.Sprintf("%d lines, last: %s", c.lines, short(c.last))
}

// exitCond — процессы завершились. Процесс сверяется по времени старта: pid, занятый новым процессом, — уже конец.
type exitCond struct {
	pids   []int
	start  map[int]string
	comm   map[int]string
	left   []int
	absent []int // не было уже на старте: опечатка в pid выглядит так же, как конец процесса
}

func watchExits(ctx context.Context, conds []cond, reserved int) (<-chan struct{}, func()) {
	fds := openExitPidfds(trackedExitPIDsFromConds(conds), reserved+1) // include this watcher's cancellation eventfd in the shared budget
	if len(fds) == 0 {
		return nil, func() {}
	}
	cancelFD, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil { //nolint:silent-error-handling // Pidfd watching is optional; the ticker remains the fallback.
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
		return nil, func() {}
	}
	out := make(chan struct{}, 1)
	done := make(chan struct{})
	stop := cancelPollOnContext(ctx, cancelFD)
	go watchPidfds(fds, cancelFD, out, done)
	cleanup := func() {
		stop()
		signalEventFD(cancelFD)
		closeAfterWatcher(done, func() { _ = unix.Close(cancelFD) })
	}
	return out, cleanup
}

func trackedExitPIDsFromConds(conds []cond) []int {
	var pids []int
	seen := map[int]struct{}{}
	for _, condition := range conds {
		ec, ok := condition.(*exitCond)
		if !ok {
			continue
		}
		for _, pid := range trackedExitPIDs(ec) {
			if _, ok := seen[pid]; !ok {
				seen[pid] = struct{}{}
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

func watchPidfds(fds []int, cancelFD int, out chan<- struct{}, done chan<- struct{}) {
	defer close(done)
	defer closeFDs(fds)
	pollfds := make([]unix.PollFd, len(fds))
	for i, fd := range fds {
		pollfds[i] = unix.PollFd{Fd: int32(fd), Events: unix.POLLIN}
	}
	pollfds = append(pollfds, unix.PollFd{Fd: int32(cancelFD), Events: unix.POLLIN})
	for activePollFDs(pollfds) > 0 {
		_, err := unix.Poll(pollfds, -1)
		if err == unix.EINTR {
			continue
		}
		if err != nil { //nolint:silent-error-handling // Polling is an optional wakeup; ticker checks remain the fallback.
			return
		}
		if pollfds[len(pollfds)-1].Revents != 0 {
			return
		}
		for i := 0; i < len(pollfds)-1; i++ {
			if pollfds[i].Fd >= 0 && pollfds[i].Revents != 0 {
				select {
				case out <- struct{}{}:
				default:
				}
				pollfds[i].Fd = -1
			}
		}
	}
}

func activePollFDs(pollfds []unix.PollFd) int {
	active := 0
	for _, pfd := range pollfds[:len(pollfds)-1] {
		if pfd.Fd >= 0 {
			active++
		}
	}
	return active
}

func closeFDs(fds []int) {
	for _, fd := range fds {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}
}

func trackedExitPIDs(c *exitCond) []int {
	var pids []int
	seen := make(map[int]struct{}, len(c.start))
	for _, pid := range c.pids {
		if _, tracked := c.start[pid]; !tracked {
			continue
		}
		if _, duplicate := seen[pid]; duplicate {
			continue
		}
		seen[pid] = struct{}{}
		pids = append(pids, pid)
	}
	return pids
}

func openExitPidfds(pids []int, reserved int) []int {
	var limit unix.Rlimit
	if len(pids) == 0 || unix.Getrlimit(unix.RLIMIT_NOFILE, &limit) != nil || !pidfdBudgetFits(len(pids), reserved, limit.Cur) {
		return nil
	}
	var fds []int
	for _, pid := range pids {
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil { //nolint:silent-error-handling // Pidfds are optional; any open failure falls back to ticker checks.
			for _, opened := range fds {
				_ = unix.Close(opened)
			}
			return nil
		}
		fds = append(fds, fd)
	}
	return fds
}

func pidfdBudgetFits(pidfds, reserved int, nofileLimit uint64) bool {
	return pidfds >= 0 && reserved >= 0 && uint64(pidfds+reserved) <= nofileLimit/2
}

func newExitCond(pids []int) (*exitCond, error) {
	c := &exitCond{pids: pids, start: map[int]string{}, comm: map[int]string{}}
	for _, pid := range pids {
		st, err := proc.ReadStat("/proc", pid)
		if proc.Gone(err) {
			c.absent = append(c.absent, pid) // уже завершился: ждать нечего
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("exit %d: %w", pid, err)
		}
		if st.State == "Z" || st.State == "X" {
			c.absent = append(c.absent, pid)
			continue
		}
		c.start[pid], c.comm[pid] = st.Start, st.Comm
	}
	return c, nil
}

func (c *exitCond) check(context.Context) (bool, error) {
	c.left = c.left[:0]
	for _, pid := range c.pids {
		start, ok := c.start[pid]
		if !ok {
			continue
		}
		st, err := proc.ReadStat("/proc", pid)
		if proc.Gone(err) {
			delete(c.start, pid)
			continue
		}
		if err != nil {
			return false, fmt.Errorf("exit %d: %w", pid, err)
		}
		if st.Start != start || st.State == "Z" || st.State == "X" {
			// pid занят новым процессом, или процесс завершился, а родитель его ещё не прибрал
			delete(c.start, pid)
			continue
		}
		c.left = append(c.left, pid)
	}
	return len(c.left) == 0, nil
}

func (c *exitCond) String() string {
	s := make([]string, len(c.pids))
	for i, pid := range c.pids {
		s[i] = fmt.Sprint(pid)
	}
	return "exit " + strings.Join(s, " ")
}

func (c *exitCond) state() string {
	var parts []string
	if len(c.left) == 0 {
		parts = append(parts, "exited")
	} else {
		s := make([]string, len(c.left))
		for i, pid := range c.left {
			s[i] = fmt.Sprintf("%d (%s)", pid, c.comm[pid])
		}
		parts = append(parts, "running: "+strings.Join(s, ", "))
	}
	if len(c.absent) > 0 {
		s := make([]string, len(c.absent))
		for i, pid := range c.absent {
			s[i] = fmt.Sprint(pid)
		}
		parts = append(parts, "not running at start: "+strings.Join(s, " "))
	}
	return strings.Join(parts, ", ")
}

// timeCond — наступило время.
type timeCond struct {
	at  time.Time
	now func() time.Time
}

func (c *timeCond) check(context.Context) (bool, error) { return !c.now().Before(c.at), nil }

func (c *timeCond) String() string { return "time " + c.at.Format("02.01 15:04:05") }

func (c *timeCond) state() string {
	if left := c.at.Sub(c.now()); left > 0 {
		return "in " + left.Round(time.Second).String()
	}
	return "reached"
}

var cancelByStopWithObserver = proc.CancelByStopWithObserver

// okCond — команда bash вышла с кодом 0. Запускается не чаще раза в every.
type okCond struct {
	cmd         string
	every       time.Duration
	next        time.Time
	now         func() time.Time
	runs        int
	code        int
	out         string
	lastCommand *exec.Cmd
}

func (c *okCond) check(ctx context.Context) (bool, error) {
	if c.now().Before(c.next) {
		return false, nil
	}
	c.next = c.now().Add(c.every)
	cmd := exec.CommandContext(ctx, "bash", "-c", c.cmd)
	c.lastCommand = cmd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var cancelDegraded bool
	if err := cancelByStopWithObserver(cmd, 0, syscall.SIGKILL, func(degraded bool, _ error) { cancelDegraded = degraded }); err != nil {
		return false, err
	}
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.CombinedOutput()
	if cancelDegraded {
		fmt.Fprintln(os.Stderr, "остановлен только прямой потомок: pidfd недоступен")
	}
	if cmd.Process != nil {
		// Bash may have exited while commands it started are still running.
		if _, stopErr := proc.StopChildGroup(cmd, 0, syscall.SIGKILL); stopErr != nil {
			return false, fmt.Errorf("%s: stop leftover processes: %w", c, stopErr)
		}
	}
	if err == nil || errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		c.runs, c.code, c.out = c.runs+1, 0, lastLine(out)
		return true, nil
	}
	if ctx.Err() != nil {
		// запуск оборван таймаутом самого ожидания: в ответе остаётся прошлый целый запуск
		return false, nil
	}
	c.runs++
	c.out = lastLine(out)
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		c.code = exitErr.ExitCode()
		return false, nil
	}
	return false, fmt.Errorf("%s: %w", c, err)
}

func (c *okCond) String() string { return fmt.Sprintf("ok '%s'", short(c.cmd)) }

func (c *okCond) state() string {
	if c.runs == 0 {
		return "no run finished yet"
	}
	return fmt.Sprintf("%d run(s), last exit %d, output: %s", c.runs, c.code, short(c.out))
}

// lastLine — последняя непустая строка вывода.
func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// short — строка не длиннее 200 рун.
func short(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= 200 {
		return string(r)
	}
	return string(r[:200]) + fmt.Sprintf("…(+%d)", len(r)-200)
}

// idleCond — машина разгрузилась по правилу graft: одна копия правила, в graft.
// --local: иначе при remote_heavy проекта graft ждал бы разгрузки другой машины. graft ждёт сам, поэтому он
// запускается один раз в фоне, а check только смотрит, вышел ли он.
type idleCond struct {
	graft string
	done  chan error
	out   syncBuf
	met   bool
}

// syncBuf — вывод graft: пишут горутины exec, читает state.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

func (c *idleCond) check(ctx context.Context) (bool, error) {
	if c.done == nil {
		c.done = make(chan error, 1)
		cmd := exec.CommandContext(ctx, c.graft, "exec", "--heavy", "--local", "--", "true")
		if err := proc.CancelByStop(cmd, 2*time.Second, syscall.SIGTERM); err != nil {
			return false, fmt.Errorf("idle: %w", err)
		}
		cmd.Stdout, cmd.Stderr = &c.out, &c.out
		if err := cmd.Start(); err != nil {
			return false, fmt.Errorf("idle: %w", err)
		}
		go func() { c.done <- cmd.Wait() }()
	}
	select {
	case err := <-c.done:
		if ctx.Err() != nil {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("idle: graft exec --heavy: %w: %s", err, lastLine(c.out.Bytes()))
		}
		c.met = true
		return true, nil
	default:
		return false, nil
	}
}

func (c *idleCond) String() string { return "idle" }

func (c *idleCond) state() string {
	if c.met {
		return "machine calm"
	}
	return "graft queue: " + short(lastLine(c.out.Bytes()))
}
