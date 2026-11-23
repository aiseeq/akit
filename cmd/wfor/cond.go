package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

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
	re        *regexp.Regexp
	path      string
	literal   string
	literalRE bool
	off       int64       // сколько байт файла уже прочитано целыми строками
	eventOff  int64       // сколько байт уже проверено на перевод строки между тиками
	tailOff   int64       // сколько байт неполной строки проверено на literal RE между тиками
	file      os.FileInfo // прочитанный файл: другой inode — файл заменён
	head      []byte      // начало прочитанного и
	edge      []byte      // байты перед off: не совпали — файл перезаписан на месте
	lines     int         // номер последней целой строки
	tail      []byte      // недописанная последняя строка
	last      string      // короткая последняя непустая строка — для ответа
	hit       string
	hitLine   int
	absent    bool
	skipped   bool // начато с --new или --after: повтор вызова должен продолжить с той же строки
	// partial — с --new файл кончался неполной строкой: она начата до старта и не в счёт, пока не кончится
	partial bool
}

type fileEvent struct{ paths map[string]struct{} }

const inotifyCheckInterval = 100 * time.Millisecond
const inotifyReadCooldown = 600 * time.Millisecond
const inotifyReadBufferSize = 64 * 1024

// A second write within 10 seconds may wait for the remainder of this cooldown.
const inotifyWriteCooldown = 10 * time.Second
const inotifyNoiseCooldown = 490 * time.Millisecond

// watchFiles shares one inotify descriptor across every file and line condition.
func watchFiles(ctx context.Context, conds []cond) (<-chan fileEvent, func(), error) {
	return watchFilesWithCooldownSignal(ctx, conds, nil)
}

func watchFilesWithCooldownSignal(ctx context.Context, conds []cond, cooldownStarted chan<- struct{}) (<-chan fileEvent, func(), error) {
	dirs := fileWatchDirs(conds)
	if len(dirs) == 0 {
		return nil, func() {}, nil
	}
	available, err := availableFDs()
	if err != nil {
		return nil, func() {}, err
	}
	if available < 8 {
		return nil, func() {}, nil
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil { //nolint:silent-error-handling // Event support is optional; ticker checks remain the fallback.
		return nil, func() {}, nil
	}
	byWD, fileMasks, filePaths, paths, dirWDs := addInotifyWatches(fd, dirs)
	if len(byWD) == 0 {
		_ = unix.Close(fd)
		return nil, func() {}, nil
	}
	pruneInotifyDirectoryWatches(fd, byWD, filePaths, paths, dirWDs)
	cancelFD, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil { //nolint:silent-error-handling // Cancellation support is optional; ticker checks remain the fallback.
		_ = unix.Close(fd)
		return nil, func() {}, nil
	}
	out := make(chan fileEvent, 1)
	done := make(chan struct{})
	stop := cancelPollOnContext(ctx, cancelFD)
	go watchInotify(ctx, fd, cancelFD, byWD, fileMasks, filePaths, paths, dirWDs, out, done, cooldownStarted)
	cleanup := func() {
		stop()
		signalEventFD(cancelFD)
		closeAfterWatcher(done, func() {
			_ = unix.Close(fd)
			_ = unix.Close(cancelFD)
		})
	}
	return out, cleanup, nil
}

func fileWatchDirs(conds []cond) map[string]map[string]uint32 {
	dirs := map[string]map[string]uint32{}
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
			dirs[dir] = map[string]uint32{}
		}
		mask := uint32(unix.IN_CREATE | unix.IN_MOVED_TO | unix.IN_DELETE | unix.IN_MOVED_FROM)
		switch c.(type) {
		case *lineCond:
			mask |= unix.IN_MODIFY | unix.IN_ATTRIB | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF
		case *fileCond:
			mask |= unix.IN_MODIFY | unix.IN_CLOSE_WRITE | unix.IN_ATTRIB | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF
		}
		dirs[dir][name] |= mask
	}
	return dirs
}

const directoryWatchMask = unix.IN_CREATE | unix.IN_MOVED_TO

func addInotifyWatches(fd int, dirs map[string]map[string]uint32) (map[int]map[string]struct{}, map[int]uint32, map[int]map[string]struct{}, map[string]map[string]uint32, map[int]string) {
	byWD := map[int]map[string]struct{}{}
	fileMasks := map[int]uint32{}
	filePaths := map[int]map[string]struct{}{}
	paths := map[string]map[string]uint32{}
	dirWDs := map[int]string{}
	for dir, names := range dirs {
		wd, addErr := unix.InotifyAddWatch(fd, dir, directoryWatchMask)
		if addErr != nil { //nolint:silent-error-handling // Unavailable paths retain ticker checks.
			continue
		}
		if byWD[wd] == nil {
			byWD[wd] = map[string]struct{}{}
		}
		dirWDs[wd] = dir
		for name, fileMask := range names {
			byWD[wd][name] = struct{}{}
			path := filepath.Join(dir, name)
			if paths[dir] == nil {
				paths[dir] = map[string]uint32{}
			}
			paths[dir][name] = fileMask
			addFileInotifyWatch(fd, path, fileMask, byWD, fileMasks, filePaths)
		}
	}
	return byWD, fileMasks, filePaths, paths, dirWDs
}

func addFileInotifyWatch(fd int, path string, mask uint32, byWD map[int]map[string]struct{}, fileMasks map[int]uint32, filePaths map[int]map[string]struct{}) {
	wd, err := unix.InotifyAddWatch(fd, path, mask)
	if err == nil {
		if byWD[wd] == nil {
			byWD[wd] = map[string]struct{}{}
		}
		byWD[wd][""] = struct{}{}
		fileMasks[wd] |= mask
		if filePaths[wd] == nil {
			filePaths[wd] = map[string]struct{}{}
		}
		filePaths[wd][path] = struct{}{}
	}
}

func watchInotify(ctx context.Context, fd, cancelFD int, byWD map[int]map[string]struct{}, fileMasks map[int]uint32, filePaths map[int]map[string]struct{}, paths map[string]map[string]uint32, dirWDs map[int]string, out chan fileEvent, done chan<- struct{}, cooldownStarted chan<- struct{}) {
	defer close(done)
	pollfds := [2]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}, {Fd: int32(cancelFD), Events: unix.POLLIN}}
	cooldownTimer := time.NewTimer(time.Hour)
	cooldownTimer.Stop()
	defer cooldownTimer.Stop()
	state := newInotifyWatchState(byWD, fileMasks, filePaths, paths, dirWDs)
	ready := false
	for {
		if !ready && !waitInotify(&pollfds) {
			return
		}
		ready = false
		anyRelevant, anyWrite, anyOverflow, anyEvent, ok := state.drain(fd, out)
		if !ok {
			return
		}
		if anyRelevant || anyWrite || anyOverflow {
			addMissingInotifyDirectoryWatches(fd, state.byWD, state.filePaths, state.paths, state.dirWDs)
			pruneInotifyDirectoryWatches(fd, state.byWD, state.filePaths, state.paths, state.dirWDs)
			state.singleNames = singleDirectoryWatchNames(state.byWD, state.dirWDs)
			state.fastName = singleInotifyNamePattern(state.singleNames)
		}
		if !anyEvent {
			continue
		}
		// Let the kernel collect a burst instead of waking this reader for every write.
		cooldown := inotifyEventCooldown(anyRelevant, anyWrite, anyOverflow)
		if cooldownStarted != nil {
			select {
			case cooldownStarted <- struct{}{}:
			default:
			}
		}
		if cooldown > 0 {
			if !waitContextTimer(ctx, cooldownTimer, cooldown) {
				return
			}
			ready = true
		}
	}
}

func inotifyEventCooldown(anyRelevant, anyWrite, anyOverflow bool) time.Duration {
	if anyOverflow {
		return 0
	}
	if anyRelevant {
		return inotifyReadCooldown
	}
	if anyWrite {
		return inotifyWriteCooldown
	}
	return inotifyNoiseCooldown
}

type inotifyWatchState struct {
	buf         []byte
	byWD        map[int]map[string]struct{}
	fileMasks   map[int]uint32
	filePaths   map[int]map[string]struct{}
	paths       map[string]map[string]uint32
	dirWDs      map[int]string
	singleNames []string
	fastName    []byte
	fastFileWD  [4]byte
	hasFastWD   bool
}

func newInotifyWatchState(byWD map[int]map[string]struct{}, fileMasks map[int]uint32, filePaths map[int]map[string]struct{}, paths map[string]map[string]uint32, dirWDs map[int]string) inotifyWatchState {
	state := inotifyWatchState{
		buf:       make([]byte, inotifyReadBufferSize),
		byWD:      byWD,
		fileMasks: fileMasks,
		filePaths: filePaths,
		paths:     paths,
		dirWDs:    dirWDs,
	}
	state.singleNames = singleDirectoryWatchNames(byWD, dirWDs)
	state.fastName = singleInotifyNamePattern(state.singleNames)
	state.updateFastFileWD()
	return state
}

var inotifyOverflowSignature = func() []byte {
	var signature [8]byte
	binary.NativeEndian.PutUint32(signature[:4], ^uint32(0))
	binary.NativeEndian.PutUint32(signature[4:], unix.IN_Q_OVERFLOW)
	return signature[:]
}()

func singleInotifyNamePattern(singleNames []string) []byte {
	name := ""
	for _, candidate := range singleNames {
		if candidate == "" {
			return nil
		}
		if name != "" && name != candidate {
			return nil
		}
		name = candidate
	}
	if name == "" {
		return nil
	}
	pattern := make([]byte, len(name)+1)
	copy(pattern, name)
	return pattern
}

func inotifyPacketMayMatch(data, fastName, fastFileWD []byte, checkOverflow bool) bool {
	if len(fastName) == 0 {
		return true
	}
	return bytes.Contains(data, fastName) ||
		(len(fastFileWD) > 0 && bytes.Contains(data, fastFileWD)) ||
		(checkOverflow && bytes.Contains(data, inotifyOverflowSignature)) ||
		inotifyPacketHasNamelessEvent(data)
}

func inotifyPacketHasNamelessEvent(data []byte) bool {
	for off := 0; off+unix.SizeofInotifyEvent <= len(data); {
		mask := binary.NativeEndian.Uint32(data[off+4 : off+8])
		length := int(binary.NativeEndian.Uint32(data[off+12 : off+16]))
		if mask&unix.IN_Q_OVERFLOW != 0 {
			off += unix.SizeofInotifyEvent
			continue
		}
		if length == 0 || mask&(unix.IN_IGNORED|unix.IN_DELETE_SELF|unix.IN_MOVE_SELF) != 0 {
			return true
		}
		if length > len(data)-off-unix.SizeofInotifyEvent {
			return true
		}
		off += unix.SizeofInotifyEvent + length
	}
	return false
}

func shouldCheckInotifyOverflow(readBytes, n, bufferSize int) bool {
	return readBytes >= bufferSize || n == bufferSize
}

func (s *inotifyWatchState) updateFastFileWD() {
	s.hasFastWD = false
	if len(s.filePaths) == 1 {
		for wd := range s.filePaths {
			binary.NativeEndian.PutUint32(s.fastFileWD[:], uint32(wd))
			s.hasFastWD = true
			return
		}
	}
}

func (s *inotifyWatchState) drain(fd int, out chan fileEvent) (anyRelevant, anyWrite, anyOverflow, anyEvent, ok bool) {
	ok = true
	readBytes := 0
	for {
		n, err := unix.Read(fd, s.buf)
		if err == unix.EAGAIN {
			return
		}
		if err == unix.EINTR {
			continue
		}
		if err != nil { //nolint:silent-error-handling // A dead watcher falls back to the periodic condition check.
			ok = false
			return
		}
		anyEvent = true
		checkOverflow := shouldCheckInotifyOverflow(readBytes, n, len(s.buf))
		fastName, fastFileWD := s.fastName, []byte(nil)
		if s.hasFastWD {
			fastFileWD = s.fastFileWD[:]
		} else if len(s.filePaths) != 0 {
			fastName = nil
		}
		relevant, writeOnly, overflow := emitInotifyEventsForFiles(fd, s.buf[:n], checkOverflow, fastName, fastFileWD, s.singleNames, s.byWD, s.fileMasks, s.filePaths, s.paths, s.dirWDs, out)
		readBytes += n
		s.updateFastFileWD()
		if overflow {
			anyOverflow = true
			removeAllFileInotifyWatches(fd, s.byWD, s.fileMasks, s.filePaths)
			s.byWD, s.fileMasks, s.filePaths, s.paths, s.dirWDs = rebuildInotifyWatches(fd, s.paths)
			s.singleNames = singleDirectoryWatchNames(s.byWD, s.dirWDs)
			s.fastName = singleInotifyNamePattern(s.singleNames)
			s.updateFastFileWD()
		}
		if writeOnly {
			anyWrite = true
		} else if relevant {
			anyRelevant = true
		}
	}
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func waitContextTimer(ctx context.Context, timer *time.Timer, delay time.Duration) bool {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func waitInotify(pollfds *[2]unix.PollFd) bool {
	for {
		pollfds[0].Revents = 0
		pollfds[1].Revents = 0
		_, err := unix.Poll(pollfds[:], -1)
		if err == unix.EINTR {
			continue
		}
		return err == nil && pollfds[1].Revents == 0
	}
}

func emitInotifyEventsForFiles(fd int, data []byte, checkOverflow bool, fastName, fastFileWD []byte, singleNames []string, byWD map[int]map[string]struct{}, fileMasks map[int]uint32, filePaths map[int]map[string]struct{}, paths map[string]map[string]uint32, dirWDs map[int]string, out chan fileEvent) (bool, bool, bool) {
	if !inotifyPacketMayMatch(data, fastName, fastFileWD, checkOverflow) {
		return false, false, false
	}
	var changed map[string]struct{}
	writeOnly := true
	overflow := false
	cachedWD := -2
	cachedSingleName := ""
	for off := 0; off+unix.SizeofInotifyEvent <= len(data); {
		header := (*inotifyEventHeader)(unsafe.Pointer(&data[off]))
		wd := int(header.wd)
		length := int(header.len)
		nameStart := off + unix.SizeofInotifyEvent
		if length > 0 {
			if wd != cachedWD {
				cachedWD = wd
				cachedSingleName = ""
				if wd >= 0 && wd < len(singleNames) {
					cachedSingleName = singleNames[wd]
				}
			}
		}
		if off+unix.SizeofInotifyEvent+length > len(data) {
			return false, false, false
		}
		var nameBytes []byte
		if length > 0 {
			if cachedSingleName != "" {
				expectedLen := len(cachedSingleName)
				if !inotifyEventNameMatches(data, nameStart, length, cachedSingleName) {
					off += unix.SizeofInotifyEvent + length
					continue
				}
				nameBytes = data[nameStart : nameStart+expectedLen]
			} else if names, ok := byWD[wd]; !ok {
				off += unix.SizeofInotifyEvent + length
				continue
			} else {
				nameBytes = data[nameStart : nameStart+length]
				if nameEnd := bytes.IndexByte(nameBytes, 0); nameEnd >= 0 {
					nameBytes = nameBytes[:nameEnd]
				}
				if _, relevant := names[string(nameBytes)]; !relevant {
					off += unix.SizeofInotifyEvent + length
					continue
				}
			}
		} else if nameEnd := bytes.IndexByte(nameBytes, 0); nameEnd >= 0 {
			nameBytes = nameBytes[:nameEnd]
		}
		if changed == nil {
			changed = make(map[string]struct{})
		}
		mask := header.mask
		name := string(nameBytes)
		eventOverflow, eventWriteOnly := recordInotifyEvent(fd, wd, mask, name, byWD, fileMasks, filePaths, paths, dirWDs, changed)
		overflow = overflow || eventOverflow
		if !eventWriteOnly {
			writeOnly = false
		}
		off += unix.SizeofInotifyEvent + length
	}
	queueInotifyChanges(changed, out)
	return len(changed) > 0, len(changed) > 0 && writeOnly, overflow
}

func inotifyEventNameMatches(data []byte, start, length int, expected string) bool {
	if len(expected) == 0 || length <= len(expected) || data[start] != expected[0] {
		return false
	}
	return data[start+len(expected)] == 0 && inotifyNameTailEqual(data[start:start+len(expected)], expected)
}

func queueInotifyChanges(changed map[string]struct{}, out chan fileEvent) {
	if len(changed) == 0 {
		return
	}
	select {
	case out <- fileEvent{paths: changed}:
	default:
		select {
		case previous := <-out:
			for path := range previous.paths {
				changed[path] = struct{}{}
			}
		default:
		}
		select {
		case out <- fileEvent{paths: changed}:
		default:
		}
	}
}

type inotifyEventHeader struct {
	wd   int32
	mask uint32
	_    uint32
	len  uint32
}

func inotifyNameTailEqual(name []byte, expected string) bool {
	for i := 1; i < len(expected); i++ {
		if name[i] != expected[i] {
			return false
		}
	}
	return true
}

func singleDirectoryWatchNames(byWD map[int]map[string]struct{}, dirWDs map[int]string) []string {
	maxWD := -1
	for wd := range dirWDs {
		if wd > maxWD {
			maxWD = wd
		}
	}
	singleNames := make([]string, maxWD+1)
	for wd := range dirWDs {
		if names := byWD[wd]; len(names) == 1 {
			for name := range names {
				if name != "" {
					singleNames[wd] = name
				}
			}
		}
	}
	return singleNames
}

func addMissingInotifyDirectoryWatches(fd int, byWD map[int]map[string]struct{}, filePaths map[int]map[string]struct{}, paths map[string]map[string]uint32, dirWDs map[int]string) {
	for dir, names := range paths {
		watched := false
		for _, watchedDir := range dirWDs {
			if watchedDir == dir {
				watched = true
				break
			}
		}
		if watched {
			continue
		}
		if canPruneInotifyDirectory(dir, filePaths, paths) {
			continue
		}
		wd, err := unix.InotifyAddWatch(fd, dir, directoryWatchMask)
		if err != nil { //nolint:silent-error-handling // Unavailable directories retain ticker checks.
			continue
		}
		if byWD[wd] == nil {
			byWD[wd] = make(map[string]struct{}, len(names))
		}
		for name := range names {
			byWD[wd][name] = struct{}{}
		}
		dirWDs[wd] = dir
	}
}

func canPruneInotifyDirectory(dir string, filePaths map[int]map[string]struct{}, paths map[string]map[string]uint32) bool {
	names := paths[dir]
	if len(names) == 0 {
		return false
	}
	existing := false
	for name := range names {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
		existing = true
		watched := false
		for _, watchedPaths := range filePaths {
			if _, ok := watchedPaths[path]; ok {
				watched = true
				break
			}
		}
		if !watched {
			return false
		}
	}
	return existing
}

func pruneInotifyDirectoryWatches(fd int, byWD map[int]map[string]struct{}, filePaths map[int]map[string]struct{}, paths map[string]map[string]uint32, dirWDs map[int]string) {
	for wd, dir := range dirWDs {
		if !canPruneInotifyDirectory(dir, filePaths, paths) {
			continue
		}
		if _, err := unix.InotifyRmWatch(fd, uint32(wd)); err != nil && err != unix.EINVAL {
			continue
		}
		for name := range paths[dir] {
			delete(byWD[wd], name)
		}
		if len(byWD[wd]) == 0 {
			delete(byWD, wd)
		}
		delete(dirWDs, wd)
	}
}

func refreshInotifyFileWatch(fd int, path string, byWD map[int]map[string]struct{}, fileMasks map[int]uint32, filePaths map[int]map[string]struct{}, paths map[string]map[string]uint32, dirWDs map[int]string) {
	dir, name := filepath.Dir(path), filepath.Base(path)
	fileMask, expected := paths[dir][name]
	removeFileInotifyWatch(fd, path, byWD, fileMasks, filePaths)
	if expected {
		addFileInotifyWatch(fd, path, fileMask, byWD, fileMasks, filePaths)
	}
	addMissingInotifyDirectoryWatches(fd, byWD, filePaths, paths, dirWDs)
}

func recordInotifyEvent(fd, wd int, mask uint32, name string, byWD map[int]map[string]struct{}, fileMasks map[int]uint32, filePaths map[int]map[string]struct{}, paths map[string]map[string]uint32, dirWDs map[int]string, changed map[string]struct{}) (bool, bool) {
	_, fileWatch := byWD[wd][""]
	if mask&unix.IN_Q_OVERFLOW != 0 {
		for dir, names := range paths {
			for targetName := range names {
				changed[filepath.Join(dir, targetName)] = struct{}{}
			}
		}
		return true, false
	}
	if mask&unix.IN_IGNORED != 0 {
		if fileWatch {
			oldPaths := filePaths[wd]
			delete(byWD, wd)
			delete(fileMasks, wd)
			delete(filePaths, wd)
			for path := range oldPaths {
				changed[path] = struct{}{}
				dir, baseName := filepath.Dir(path), filepath.Base(path)
				if fileMask, expected := paths[dir][baseName]; expected {
					addFileInotifyWatch(fd, path, fileMask, byWD, fileMasks, filePaths)
				}
			}
			addMissingInotifyDirectoryWatches(fd, byWD, filePaths, paths, dirWDs)
			return false, false
		}
		delete(byWD, wd)
		delete(fileMasks, wd)
		delete(filePaths, wd)
		delete(dirWDs, wd)
		return false, true
	}
	if fileWatch && name == "" {
		if mask&fileMasks[wd] == 0 {
			return false, true
		}
		if mask&^uint32(unix.IN_MODIFY) != 0 {
			for path := range filePaths[wd] {
				changed[path] = struct{}{}
				if mask&(unix.IN_ATTRIB|unix.IN_DELETE_SELF|unix.IN_MOVE_SELF) != 0 {
					refreshInotifyFileWatch(fd, path, byWD, fileMasks, filePaths, paths, dirWDs)
				}
			}
			return false, false
		}
		for path := range filePaths[wd] {
			changed[path] = struct{}{}
		}
		return false, true
	}
	names, ok := byWD[wd]
	if !ok {
		return false, true
	}
	if _, relevant := names[name]; !relevant {
		return false, true
	}
	dir := dirWDs[wd]
	if mask&(unix.IN_DELETE|unix.IN_MOVED_FROM) != 0 {
		removeFileInotifyWatch(fd, filepath.Join(dir, name), byWD, fileMasks, filePaths)
	}
	if fileMask, exists := paths[dir][name]; exists && mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0 {
		removeFileInotifyWatch(fd, filepath.Join(dir, name), byWD, fileMasks, filePaths)
		addFileInotifyWatch(fd, filepath.Join(dir, name), fileMask, byWD, fileMasks, filePaths)
	}
	changed[filepath.Join(dir, name)] = struct{}{}
	return false, false
}

func rebuildInotifyWatches(fd int, paths map[string]map[string]uint32) (map[int]map[string]struct{}, map[int]uint32, map[int]map[string]struct{}, map[string]map[string]uint32, map[int]string) {
	dirs := make(map[string]map[string]uint32, len(paths))
	maps.Copy(dirs, paths)
	byWD, fileMasks, filePaths, _, dirWDs := addInotifyWatches(fd, dirs)
	return byWD, fileMasks, filePaths, paths, dirWDs
}

func removeAllFileInotifyWatches(fd int, byWD map[int]map[string]struct{}, fileMasks map[int]uint32, filePaths map[int]map[string]struct{}) {
	for wd := range filePaths {
		if _, err := unix.InotifyRmWatch(fd, uint32(wd)); err != nil && err != unix.EINVAL {
			continue // Keep the old mapping if the kernel could not remove the watch.
		}
		delete(filePaths, wd)
		delete(fileMasks, wd)
		delete(byWD, wd)
	}
}

func removeFileInotifyWatch(fd int, path string, byWD map[int]map[string]struct{}, fileMasks map[int]uint32, filePaths map[int]map[string]struct{}) {
	for wd, watched := range filePaths {
		if _, ok := watched[path]; !ok {
			continue
		}
		delete(watched, path)
		if len(watched) == 0 {
			if _, err := unix.InotifyRmWatch(fd, uint32(wd)); err != nil && err != unix.EINVAL {
				continue // Keep the old mapping if the kernel could not remove the watch.
			}
			delete(filePaths, wd)
			delete(fileMasks, wd)
			delete(byWD, wd)
		}
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
	literal, complete := re.LiteralPrefix()
	c := &lineCond{re: re, path: path, literal: literal, literalRE: complete && literal != ""}
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
	c.eventOff = max(c.eventOff, c.off)
	c.tailOff = c.off
	// последняя строка пропущенного — в ответ по таймауту, пока не дописано новое
	for line := range bytes.Lines(data[:end]) {
		if s := shortBytes(line); s != "" {
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
	if c.eventOff < c.off {
		c.eventOff = c.off
	}
	c.edge = bytes.Clone(chunk[max(0, len(chunk)-markLen):])
}

// replaced — файл уже не тот, что читали: другой inode, короче прочитанного или другие байты начала и края.
func (c *lineCond) replaced(f *os.File, st os.FileInfo) (bool, error) {
	if c.file != nil && !os.SameFile(c.file, st) || st.Size() < c.off || st.Size() < c.eventOff {
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
	st, err := os.Stat(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		c.absent = true
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s: %w", c, err)
	}
	if c.file != nil && !c.partial && c.eventOff <= c.off && st.Size() == c.off && os.SameFile(c.file, st) && c.file.Size() == st.Size() && c.file.ModTime().Equal(st.ModTime()) {
		c.absent = false
		return false, nil
	}
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
		c.off, c.eventOff, c.tailOff, c.lines, c.tail, c.head, c.edge, c.partial = 0, 0, 0, 0, nil, nil, nil, false
	}
	c.file = st
	if len(c.tail) > 0 {
		previous := make([]byte, len(c.tail))
		n, readErr := f.ReadAt(previous, c.off)
		if readErr != nil && readErr != io.EOF {
			return false, fmt.Errorf("%s: %w", c, readErr)
		}
		if n != len(c.tail) || !bytes.Equal(previous, c.tail) {
			c.tailOff = c.off
			c.tail = nil
		}
	}
	return c.scanContent(f, allowIncomplete)
}

func (c *lineCond) scanContent(f *os.File, allowIncomplete bool) (bool, error) {
	scanAt := max(c.eventOff, c.off)
	if _, err := f.Seek(scanAt, io.SeekStart); err != nil {
		return false, fmt.Errorf("%s: %w", c, err)
	}
	newData, err := io.ReadAll(f)
	if err != nil {
		return false, fmt.Errorf("%s: %w", c, err)
	}
	c.eventOff = scanAt + int64(len(newData))
	if !bytes.Contains(newData, []byte{'\n'}) {
		return c.checkIncomplete(f, allowIncomplete)
	}
	startOff := c.off
	if _, err := f.Seek(startOff, io.SeekStart); err != nil {
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
	c.eventOff = max(c.eventOff, startOff+int64(len(data)))
	c.tailOff = c.off
	if allowIncomplete && len(c.tail) > 0 && !c.partial {
		if c.literalRE {
			return c.matchLiteralTail(f, c.eventOff)
		}
		return c.match(c.tail, c.lines+1), nil
	}
	return false, nil
}

func (c *lineCond) checkIncomplete(f *os.File, allow bool) (bool, error) {
	if !allow || c.partial {
		if c.partial {
			c.tailOff = c.eventOff
		}
		return false, nil
	}
	if c.literalRE {
		return c.matchLiteralTail(f, c.eventOff)
	}
	if _, err := f.Seek(c.off, io.SeekStart); err != nil {
		return false, fmt.Errorf("%s: %w", c, err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return false, fmt.Errorf("%s: %w", c, err)
	}
	c.tail = data
	return len(c.tail) > 0 && c.match(c.tail, c.lines+1), nil
}

func (c *lineCond) matchLiteralTail(f *os.File, end int64) (bool, error) {
	stateLen := min(int64(4096), max(int64(0), end-c.off))
	if stateLen > 0 {
		state := make([]byte, int(stateLen))
		n, err := f.ReadAt(state, c.off)
		if err != nil && err != io.EOF {
			return false, fmt.Errorf("%s: %w", c, err)
		}
		if n > 0 {
			c.last = shortBytes(state[:n])
		}
	}
	start := min(max(c.off, c.tailOff-int64(len(c.literal))+1), end)
	data := make([]byte, end-start)
	if len(data) > 0 {
		n, err := f.ReadAt(data, start)
		if err != nil && err != io.EOF {
			return false, fmt.Errorf("%s: %w", c, err)
		}
		data = data[:n]
	}
	fullTail := make([]byte, max(int64(0), end-c.off))
	if len(fullTail) > 0 {
		n, err := f.ReadAt(fullTail, c.off)
		if err != nil && err != io.EOF {
			return false, fmt.Errorf("%s: %w", c, err)
		}
		fullTail = fullTail[:n]
	}
	c.tail = fullTail
	c.tailOff = end
	if !bytes.Contains(data, []byte(c.literal)) {
		return false, nil
	}
	if _, err := f.Seek(c.off, io.SeekStart); err != nil {
		return false, fmt.Errorf("%s: %w", c, err)
	}
	line, err := io.ReadAll(f)
	if err != nil {
		return false, fmt.Errorf("%s: %w", c, err)
	}
	c.hit, c.hitLine = string(line), c.lines+1
	c.last = shortBytes(line)
	return true, nil
}

func (c *lineCond) match(line []byte, n int) bool {
	if s := shortBytes(line); s != "" {
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
		return fmt.Sprintf("%d lines and one begun before the start, last: %s", c.lines, c.last)
	}
	return fmt.Sprintf("%d lines, last: %s", c.lines, c.last)
}

// exitCond — процессы завершились. Процесс сверяется по времени старта: pid, занятый новым процессом, — уже конец.
type exitCond struct {
	pids   []int
	start  map[int]string
	comm   map[int]string
	left   []int
	absent []int // не было уже на старте: опечатка в pid выглядит так же, как конец процесса
}

func watchExits(ctx context.Context, conds []cond, reserved int) (<-chan struct{}, func(), error) {
	fds, err := openExitPidfds(trackedExitPIDsFromConds(conds), reserved+1) // include this watcher's cancellation eventfd in the shared budget
	if err != nil {
		return nil, func() {}, err
	}
	if len(fds) == 0 {
		return nil, func() {}, nil
	}
	cancelFD, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil { //nolint:silent-error-handling // Pidfd watching is optional; the ticker remains the fallback.
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
		return nil, func() {}, nil
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
	return out, cleanup, nil
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

func openExitPidfds(pids []int, reserved int) ([]int, error) {
	if len(pids) == 0 {
		return nil, nil
	}
	available, err := availableFDs()
	if err != nil {
		return nil, err
	}
	if available <= reserved+5 {
		return nil, nil
	}
	maxPIDFDs := available - reserved - 5 // keep room for the exit cancellation fd and condition checks
	if len(pids) > maxPIDFDs {
		pids = pids[:maxPIDFDs]
	}
	var fds []int
	for _, pid := range pids {
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil { //nolint:silent-error-handling // Keep successfully opened pidfds; this PID remains covered by ticker checks.
			continue
		}
		fds = append(fds, fd)
	}
	return fds, nil
}

// availableFDs counts descriptors that can be opened now, including inherited descriptors.
func availableFDs() (int, error) {
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		return 0, fmt.Errorf("get descriptor limit: %w", err)
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, fmt.Errorf("count open descriptors: %w", err)
	}
	used := uint64(len(entries))
	if used > 0 { // ReadDir itself owns one fd, closed before this function returns.
		used--
	}
	if limit.Cur <= used {
		return 0, nil
	}
	free := limit.Cur - used
	if free > uint64(^uint(0)>>1) {
		return int(^uint(0) >> 1), nil
	}
	return int(free), nil
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

func shortBytes(b []byte) string {
	b = bytes.TrimSpace(b)
	end := 0
	for range 200 {
		if end == len(b) {
			return string(b)
		}
		_, size := utf8.DecodeRune(b[end:])
		end += size
	}
	if end == len(b) {
		return string(b)
	}
	if len(b) <= 4096 {
		return string(b[:end]) + fmt.Sprintf("…(+%d)", utf8.RuneCount(b)-200)
	}
	return string(b[:end]) + "…"
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
			c.done = nil
			return false, fmt.Errorf("idle: %w", err)
		}
		cmd.Stdout, cmd.Stderr = &c.out, &c.out
		if err := cmd.Start(); err != nil {
			c.done = nil
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
