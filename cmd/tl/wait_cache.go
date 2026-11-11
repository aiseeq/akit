package main

import (
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aiseeq/akit/transcript"
	transcriptwaits "github.com/aiseeq/akit/transcript/waits"
)

const waitCacheMaxAge = 30 * 24 * time.Hour

// afterWaitRead — точка между чтением транскрипта и сверкой его ключа; тест дописывает в ней файл.
var afterWaitRead = func(string) {}

// waitCacheVersion — формат записи кэша ожиданий.
const waitCacheVersion = 9

var errWaitCacheDecode = errors.New("invalid wait cache entry")

type cachedWaitEvent struct {
	Role      transcript.Role
	CallID    string
	Text      string
	Time      time.Time
	Line      int
	MessageID string
	Who       string
	Sidechain bool
	Error     bool
	Raw       []byte
	Command   string
	BG        bool
	Calls     []cachedShellCall
	Invalid   bool
}

type cachedShellCall struct {
	Name       string
	Arg0       string
	HasArg     bool
	Background bool
}

type waitCacheRecord struct {
	Version int
	Path    string
	Size    int64
	MTime   int64
	Code    string
}

type waitCacheState uint8

const (
	waitCacheMiss waitCacheState = iota
	waitCacheHit
)

type waitCache struct {
	dir      string
	code     string
	stderr   io.Writer
	warnOnce sync.Once
	misses   atomic.Int64 // транскрипты, прочитанные мимо кэша
}

// newWaitCache — кэш под код этой сборки: исходники, от которых зависит запись (waitCacheCode), версия Go и платформа.
// Отпечаток всего бинаря менялся бы с каждой установкой: в бинарь вшиты ревизия и пути сборки.
func newWaitCache(stderr io.Writer) *waitCache {
	dir, err := waitCacheDir()
	if err != nil {
		cache := &waitCache{stderr: stderr}
		cache.warn(err)
		return cache
	}
	return &waitCache{dir: dir, code: waitCacheCode + " " + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH, stderr: stderr}
}

func waitCacheDir() (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("find user cache directory: %w", err)
		}
	}
	return filepath.Join(base, "akit", "tl-wait"), nil
}

func (c *waitCache) warn(err error) {
	if c == nil || c.stderr == nil {
		return
	}
	c.warnOnce.Do(func() { fmt.Fprintf(c.stderr, "tl: wait cache unavailable: %v\n", err) })
}

func (c *waitCache) recordPath(path string) string {
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])+".gob")
}

func (c *waitCache) key(path string) (waitCacheRecord, error) {
	info, err := os.Stat(path)
	if err != nil {
		return waitCacheRecord{}, fmt.Errorf("stat transcript: %w", err)
	}
	if c == nil || c.dir == "" || c.code == "" {
		return waitCacheRecord{}, fmt.Errorf("cache key unavailable")
	}
	return waitCacheRecord{Version: waitCacheVersion, Path: path, Size: info.Size(), MTime: info.ModTime().UnixNano(), Code: c.code}, nil
}

func (c *waitCache) valid(path string) bool {
	_, state, err := c.load(path, false)
	if err != nil && !errors.Is(err, errWaitCacheDecode) {
		c.warn(err)
	}
	return state == waitCacheHit
}

// load сверяет ключ записи с файлом; события декодирует, только если withEvents: заголовок лежит в записи
// отдельным значением gob, и проверке допуска не нужно разбирать события.
func (c *waitCache) load(path string, withEvents bool) ([]cachedWaitEvent, waitCacheState, error) {
	key, err := c.key(path)
	if err != nil {
		return nil, waitCacheMiss, err
	}
	f, err := os.Open(c.recordPath(path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, waitCacheMiss, nil
		}
		return nil, waitCacheMiss, fmt.Errorf("read cache entry: %w", err)
	}
	defer f.Close()
	dec := gob.NewDecoder(f)
	var record waitCacheRecord
	if err := dec.Decode(&record); err != nil {
		return nil, waitCacheMiss, fmt.Errorf("%w: %w", errWaitCacheDecode, err)
	}
	if !sameWaitCacheKey(record, key) {
		return nil, waitCacheMiss, nil
	}
	if !withEvents {
		return nil, waitCacheHit, nil
	}
	var events []cachedWaitEvent
	if err := dec.Decode(&events); err != nil {
		return nil, waitCacheMiss, fmt.Errorf("%w: %w", errWaitCacheDecode, err)
	}
	return events, waitCacheHit, nil
}

func sameWaitCacheKey(a, b waitCacheRecord) bool {
	return a.Version == b.Version && a.Path == b.Path && a.Size == b.Size && a.MTime == b.MTime && a.Code == b.Code
}

func (c *waitCache) cleanup(now time.Time) {
	if c == nil || c.dir == "" {
		return
	}
	entries, err := os.ReadDir(c.dir)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		c.warn(err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".gob") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			c.warn(err)
			continue
		}
		if now.Sub(info.ModTime()) > waitCacheMaxAge {
			if err := os.Remove(filepath.Join(c.dir, entry.Name())); err != nil && !os.IsNotExist(err) {
				c.warn(err)
			}
		}
	}
}

func (c *waitCache) events(path string, codex bool) ([]cachedWaitEvent, error) {
	if events, state, err := c.load(path, true); err == nil && state == waitCacheHit {
		return events, nil
	} else if err != nil && !errors.Is(err, errWaitCacheDecode) {
		c.warn(err)
	}
	key, err := c.key(path)
	if err != nil {
		return nil, err
	}
	c.misses.Add(1)
	events, err := readWaitEvents(path, codex)
	if err != nil {
		return nil, err
	}
	afterWaitRead(path)
	// Транскрипт живой: дописанный за время чтения файл дал бы записи неполный список под новым ключом.
	if current, err := c.key(path); err == nil && current.Size == key.Size && current.MTime == key.MTime {
		c.saveRecord(key, events)
	}
	return events, nil
}

// readWaitEvents reads what a wait needs; a codex rollout gets its command
// completions in the same pass, matched to the calls.
func readWaitEvents(path string, codex bool) ([]cachedWaitEvent, error) {
	events := make([]cachedWaitEvent, 0)
	var commandResults []transcript.CodexCommandResult
	var err error
	addEvent := func(ev transcript.Event) {
		events = append(events, cacheWaitEvents(ev)...)
	}
	if codex {
		_, err = transcript.ReadWithCodexCommandsDiscardOutput(path, addEvent, func(result transcript.CodexCommandResult) {
			commandResults = append(commandResults, result)
		})
	} else {
		_, err = transcript.Read(path, nil, addEvent)
	}
	if err != nil {
		return nil, err
	}
	if len(commandResults) > 0 {
		matcher := matchCodexWaitResults(codexCallsFromCache(events), commandResults)
		for i := range events {
			matcher.applyCached(&events[i])
		}
	}
	return events, nil
}

func cacheWaitEvents(ev transcript.Event) []cachedWaitEvent {
	if ev.Role == transcript.Cmd {
		command, bg := waitCommand(ev)
		if _, poll := waitPollSession(cacheWaitCall(ev, command, bg, nil, false)); poll {
			return []cachedWaitEvent{cacheWaitCall(ev, command, bg, nil, false)}
		}
		if command == "" && ev.MessageID != "" && !ev.Sidechain && isSerialTool(ev.Who) {
			// Последовательный инструмент без команды (Edit, Write) — предшественник для начала соседа.
			return []cachedWaitEvent{cacheWaitCall(ev, "", false, nil, false)}
		}
		if command != "" {
			cachedCalls, invalid := parseWaitCalls(command)
			return []cachedWaitEvent{cacheWaitCall(ev, command, bg, cachedCalls, invalid)}
		}
	} else if ev.Role == transcript.Out {
		// Текст вывода нужен только ошибке: по нему узнаётся отказ разрешения; весь вывод раздул бы кэш.
		var raw []byte
		if ev.Error {
			raw = append([]byte(nil), ev.Raw...)
		}
		text := ""
		if _, running := transcriptwaits.Running(ev.Text); running {
			text = ev.Text
		}
		return []cachedWaitEvent{{Role: ev.Role, CallID: ev.CallID, Text: text, Time: ev.Time, Line: ev.Line, MessageID: ev.MessageID, Who: ev.Who, Sidechain: ev.Sidechain, Error: ev.Error, Raw: raw}}
	}
	return nil
}

func cacheWaitCall(ev transcript.Event, command string, bg bool, calls []cachedShellCall, invalid bool) cachedWaitEvent {
	var cached cachedWaitEvent
	cached.Role = ev.Role
	cached.CallID = ev.CallID
	cached.Time = ev.Time
	cached.Line = ev.Line
	cached.MessageID = ev.MessageID
	cached.Who = ev.Who
	cached.Sidechain = ev.Sidechain
	cached.Error = ev.Error
	cached.Raw = append([]byte(nil), ev.Raw...)
	cached.Command = command
	cached.BG = bg
	cached.Calls = calls
	cached.Invalid = invalid
	return cached
}

// saveRecord пишет запись атомарно: временный файл, Sync, переименование — усечённой записи читатель не
// застанет. Своей суммы у записи нет: порчу носителя ловит файловая система, а сбой чтения или разбора —
// промах и перечтение транскрипта.
func (c *waitCache) saveRecord(key waitCacheRecord, events []cachedWaitEvent) {
	if c == nil || c.dir == "" {
		return
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		c.warn(err)
		return
	}
	f, err := os.CreateTemp(c.dir, ".tl-wait-*.tmp")
	if err != nil {
		c.warn(err)
		return
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	enc := gob.NewEncoder(f)
	err = enc.Encode(key)
	if err == nil {
		err = enc.Encode(events)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, c.recordPath(key.Path))
	}
	if err != nil {
		c.warn(err)
	}
}
