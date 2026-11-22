package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
)

const watchMask = syscall.IN_CREATE | syscall.IN_MOVED_TO | syscall.IN_MOVED_FROM | syscall.IN_MODIFY |
	syscall.IN_CLOSE_WRITE | syscall.IN_DELETE | syscall.IN_ONLYDIR

// inotify watches folders through the kernel's inotify. Events are collected by a reader goroutine and taken by
// the passes; repeated events of one entry count once.
type inotify struct {
	fd     int
	f      *os.File // the same descriptor, read through the runtime poller so that close ends the reader
	mu     sync.Mutex
	dirs   map[int32]string
	wds    map[string]int32
	events map[watchEvent]bool
	order  []watchEvent
	lost   bool
	full   bool  // the kernel's watch limit is reached: the rest is found by polling
	dead   error // the reader stopped on this error: the passes poll as without inotify
	told   bool  // dead is logged
	logf   func(string, ...any)
}

// newWatcher starts an inotify watcher; logf is called from the goroutine of the passes only.
func newWatcher(logf func(string, ...any)) (watcher, error) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("inotify: %w", err)
	}
	w := &inotify{fd: fd, f: os.NewFile(uintptr(fd), "inotify"), dirs: map[int32]string{}, wds: map[string]int32{},
		events: map[watchEvent]bool{}, logf: logf}
	go w.read()
	return w, nil
}

func (w *inotify) add(dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.wds[dir]; ok || w.full {
		return
	}
	wd, err := syscall.InotifyAddWatch(w.fd, dir, watchMask)
	switch {
	case errors.Is(err, syscall.ENOSPC):
		w.full = true
		w.logf("inotify: watch limit reached at %d folders; the rest is found by polling", len(w.wds))
		return
	case errors.Is(err, syscall.ENOENT):
		return // gone before the watch: nothing to watch
	case err != nil:
		w.logf("inotify watch %s: %v; found by polling", dir, err)
		return
	}
	w.dirs[int32(wd)], w.wds[dir] = dir, int32(wd)
}

func (w *inotify) read() {
	buf := make([]byte, 64<<10)
	for {
		n, err := w.f.Read(buf)
		if err != nil {
			if !errors.Is(err, os.ErrClosed) {
				w.mu.Lock()
				w.dead, w.lost = err, true
				w.mu.Unlock()
			}
			return
		}
		w.mu.Lock()
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			// struct inotify_event: wd, mask, cookie, len, then len bytes of a NUL-padded name
			wd := int32(binary.NativeEndian.Uint32(buf[off:]))
			mask := binary.NativeEndian.Uint32(buf[off+4:])
			size := int(binary.NativeEndian.Uint32(buf[off+12:]))
			start := off + syscall.SizeofInotifyEvent
			if start+size > n {
				break
			}
			name := buf[start : start+size]
			off = start + size
			if mask&syscall.IN_Q_OVERFLOW != 0 {
				w.lost = true
				continue
			}
			dir, ok := w.dirs[wd]
			if mask&syscall.IN_IGNORED != 0 {
				delete(w.dirs, wd)
				delete(w.wds, dir)
				continue
			}
			if !ok {
				continue
			}
			e := watchEvent{dir: dir, name: string(bytes.TrimRight(name, "\x00")), isDir: mask&syscall.IN_ISDIR != 0}
			if !w.events[e] {
				w.events[e] = true
				w.order = append(w.order, e)
			}
		}
		w.mu.Unlock()
	}
}

func (w *inotify) drain() ([]watchEvent, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out, lost := w.order, w.lost
	w.order, w.events, w.lost = nil, map[watchEvent]bool{}, false
	return out, lost
}

func (w *inotify) active() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dead != nil && !w.told {
		w.logf("inotify stopped: %v; polling only", w.dead)
		w.told = true
	}
	return w.dead == nil
}

func (w *inotify) close() { w.f.Close() }
