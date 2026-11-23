package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The shadow: with TL_BACKEND=files a share of the calls (TL_SHADOW_RATE, 0.1 by default, 0 — off) repeats the
// query through the database after the answer, in a process of its own that the call does not wait for. It
// compares the events both paths found and writes one JSON line per call to the journal (TL_SHADOW_JOURNAL,
// $XDG_STATE_HOME/akit/tl-shadow.jsonl by default). Both sides count only events up to the moment the call began
// (or its --until, if earlier): lines written while the files were read are in one path only by timing.

const (
	shadowChildEnv     = "TL_SHADOW_CHILD"
	defaultShadowRate  = 0.1
	shadowTimeout      = 60 * time.Second
	shadowPerForm      = 2 // repeats of one form at once
	shadowAll          = 4 // repeats at once in all
	shadowSampleKeys   = 10
	shadowJournalName  = "tl-shadow.jsonl"
	shadowSlotsDirName = "tl-shadow"
	shadowLogName      = "errors.log"
)

// answer is what one call found, for the shadow: the result of a listing or --usage, the events of tl REF.
type answer struct {
	res    *result
	refs   []hit
	filled bool
}

// shadowRate is TL_SHADOW_RATE: a share from 0 to 1.
func shadowRate() (float64, error) {
	raw, ok := os.LookupEnv("TL_SHADOW_RATE")
	if !ok || raw == "" {
		return defaultShadowRate, nil
	}
	rate, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("TL_SHADOW_RATE=%q: %w", raw, err)
	}
	if rate < 0 || rate > 1 {
		return 0, fmt.Errorf("TL_SHADOW_RATE=%q: want a share from 0 to 1", raw)
	}
	return rate, nil
}

func stateDir(home string) string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "akit")
	}
	return filepath.Join(home, ".local", "state", "akit")
}

func shadowJournal(home string) string {
	if path := os.Getenv("TL_SHADOW_JOURNAL"); path != "" {
		return path
	}
	return filepath.Join(stateDir(home), shadowJournalName)
}

// shadowSpec is what the repeat gets from the call: its arguments and moment, and what the file path found.
type shadowSpec struct {
	Args      []string  `json:"args"`
	Now       time.Time `json:"now"`
	Cutoff    time.Time `json:"cutoff"`
	Keys      []string  `json:"keys"`
	FilesS    float64   `json:"files_s"`
	FilesCode int       `json:"files_code"`
	Journal   string    `json:"journal"`
	Slots     string    `json:"slots"`
}

// shadowRecord is one line of the journal.
type shadowRecord struct {
	Time      string   `json:"time"`
	Args      []string `json:"args"`
	Until     string   `json:"until"`
	FilesKeys int      `json:"files_keys"`
	DBKeys    int      `json:"db_keys"`
	Diff      int      `json:"diff"`
	OnlyFiles []string `json:"only_files,omitempty"`
	OnlyDB    []string `json:"only_db,omitempty"`
	FilesS    float64  `json:"files_s"`
	DBS       float64  `json:"db_s"`
	FilesCode int      `json:"files_code"`
	DBCode    int      `json:"db_code"`
	Error     string   `json:"error,omitempty"`
}

// shadowCutoff is the moment both paths are compared up to.
func shadowCutoff(opts options, now time.Time) time.Time {
	if !opts.until.IsZero() && opts.until.Before(now) {
		return opts.until
	}
	return now
}

func shadowDBCutoff(opts options, now, through time.Time) time.Time {
	cutoff := shadowCutoff(opts, now)
	if !through.IsZero() && (cutoff.IsZero() || through.Before(cutoff)) {
		return through
	}
	return cutoff
}

// answerKeys are the events an answer holds up to cutoff: path:line of each, a row per session for --usage.
func answerKeys(a *answer, opts options, cutoff time.Time) []string {
	keys := []string{}
	if a.res == nil {
		for i := range a.refs {
			if !cutoff.IsZero() && a.refs[i].ev.Time.After(cutoff) {
				continue
			}
			keys = append(keys, a.refs[i].src.Path+":"+strconv.Itoa(a.refs[i].ev.Line))
		}
		slices.Sort(keys)
		return keys
	}
	var hits []hit
	for i := range a.res.hits {
		if cutoff.IsZero() || !a.res.hits[i].ev.Time.After(cutoff) {
			hits = append(hits, a.res.hits[i])
		}
	}
	if opts.usage {
		return usageKeys(hits)
	}
	for i := range hits {
		keys = append(keys, hits[i].src.Path+":"+strconv.Itoa(hits[i].ev.Line))
	}
	slices.Sort(keys)
	return keys
}

// usageKeys — a row per session: replies, the four token counts and the API price, as --usage counts them.
func usageKeys(hits []hit) []string {
	per := map[string]*tally{}
	for _, rp := range usageReplies(hits) {
		t := per[rp.session]
		if t == nil {
			t = &tally{}
			per[rp.session] = t
		}
		t.add(rp.r.Model, rp.r.Usage)
	}
	keys := make([]string, 0, len(per))
	for path, t := range per {
		keys = append(keys, fmt.Sprintf("%s\t%d\t%d\t%d\t%d\t%d\t%.2f", path, t.replies, t.input, t.cacheWrite, t.cacheRead, t.outp, t.usd))
	}
	slices.Sort(keys)
	return keys
}

// keyDiff counts the keys in one list only, each key as often as it is there more than in the other; samples
// are the first of them.
func keyDiff(a, b []string) (n int, onlyA, onlyB []string) {
	i, j := 0, 0
	add := func(list *[]string, key string) {
		n++
		if len(*list) < shadowSampleKeys {
			*list = append(*list, shortKey(key))
		}
	}
	for i < len(a) || j < len(b) {
		switch {
		case j == len(b) || i < len(a) && a[i] < b[j]:
			add(&onlyA, a[i])
			i++
		case i == len(a) || b[j] < a[i]:
			add(&onlyB, b[j])
			j++
		default:
			i++
			j++
		}
	}
	return n, onlyA, onlyB
}

// shortKey names the transcript of a key by its id, not its path.
func shortKey(key string) string {
	path, rest, ok := strings.Cut(key, "\t")
	sep := "\t"
	if !ok {
		i := strings.LastIndexByte(key, ':')
		if i < 0 {
			return key
		}
		path, rest, sep = key[:i], key[i+1:], ":"
	}
	id := sourceFullID(source{Path: path, ID: path})
	return id[:min(8, len(id))] + sep + rest
}

// formSlot names the form of a call: the repeats of one form at once are limited apart from all of them.
func formSlot(args []string) string {
	sum := sha256.Sum256([]byte(strings.Join(args, "\x00")))
	return hex.EncodeToString(sum[:6])
}

// shadowStarts counts the repeats this process started.
var shadowStarts atomic.Int64

// startShadow starts the repeat of the call through the database, detached from it; the call does not wait for
// it. Nothing starts when shadowPerForm repeats of this form or shadowAll in all already run. The repeat has no
// terminal: what it cannot write to the journal goes to shadowLogName beside the slots.
func startShadow(spec shadowSpec, form string) error {
	if err := os.MkdirAll(spec.Slots, 0o700); err != nil {
		return fmt.Errorf("shadow slots: %w", err)
	}
	for _, slot := range []struct {
		prefix string
		n      int
	}{{"form-" + form, shadowPerForm}, {"all", shadowAll}} {
		free, err := slotFree(spec.Slots, slot.prefix, slot.n)
		if err != nil || !free {
			return err
		}
	}
	specPath, err := writeSpec(spec)
	if err != nil {
		return err
	}
	if err := spawnShadow(specPath, filepath.Join(spec.Slots, shadowLogName)); err != nil {
		return errors.Join(err, removeSpec(specPath))
	}
	shadowStarts.Add(1)
	return nil
}

func writeSpec(spec shadowSpec) (string, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("shadow spec: %w", err)
	}
	f, err := os.CreateTemp(spec.Slots, "spec-*.json")
	if err != nil {
		return "", fmt.Errorf("shadow spec: %w", err)
	}
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Close()); err != nil {
		return "", errors.Join(fmt.Errorf("shadow spec: %w", err), removeSpec(f.Name()))
	}
	return f.Name(), nil
}

func removeSpec(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove shadow spec: %w", err)
	}
	return nil
}

func spawnShadow(specPath, logPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find tl executable: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("shadow log: %w", err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), shadowChildEnv+"="+specPath)
	cmd.Stderr = logFile
	cmd.SysProcAttr = detached()
	err = cmd.Start()
	// the repeat has its own copy of the log; this one is not written here
	closeErr := logFile.Close()
	if err != nil {
		return errors.Join(fmt.Errorf("start shadow: %w", err), closeErr)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("start shadow: %w", err)
	}
	return closeErr
}

// slotFree tells whether one of the n slots named prefix is not held now.
func slotFree(dir, prefix string, n int) (bool, error) {
	for i := range n {
		release, err := takeSlot(filepath.Join(dir, fmt.Sprintf("%s-%d.lock", prefix, i)))
		if err != nil {
			return false, err
		}
		if release != nil {
			release()
			return true, nil
		}
	}
	return false, nil
}

// heldSlots keep the slots holdSlot took reachable: a dropped release lets the collector close the file, and
// the lock goes with it.
var (
	heldMu    sync.Mutex
	heldSlots []func()
)

// holdSlot takes one of the n slots named prefix for the life of the process; false — all are held.
func holdSlot(dir, prefix string, n int) (bool, error) {
	for i := range n {
		release, err := takeSlot(filepath.Join(dir, fmt.Sprintf("%s-%d.lock", prefix, i)))
		if err != nil {
			return false, err
		}
		if release != nil {
			heldMu.Lock()
			heldSlots = append(heldSlots, release)
			heldMu.Unlock()
			return true, nil
		}
	}
	return false, nil
}

// shadowAfter starts the repeat of a call answered from files, when it was picked for the shadow.
func shadowAfter(a *answer, opts options, e env, code int, elapsed time.Duration, stderr io.Writer) {
	if a == nil || !a.filled || code != 0 && code != exitNone {
		return
	}
	state, err := readDBState(e.home)
	if err != nil {
		printMasked(stderr, fmt.Sprintf("tl: shadow: freshness state: %v; no shadow\n", err))
		return
	}
	cutoff := shadowDBCutoff(opts, e.now, state.DBThrough)
	spec := shadowSpec{Args: opts.originalArgs, Now: e.now, Cutoff: cutoff, Keys: answerKeys(a, opts, cutoff), FilesS: elapsed.Seconds(),
		FilesCode: code, Journal: shadowJournal(e.home), Slots: filepath.Join(stateDir(e.home), shadowSlotsDirName)}
	if err := startShadow(spec, formSlot(opts.originalArgs)); err != nil {
		printMasked(stderr, fmt.Sprintf("tl: shadow: %v\n", err))
	}
}

// runShadowChild is the repeat: it reads its spec, asks the database the same, compares and writes the journal.
// All slots of its form or of all held — it was started past the limit, and leaves.
func runShadowChild(specPath string) error {
	data, err := os.ReadFile(specPath)
	if err != nil {
		return fmt.Errorf("shadow spec: %w", err)
	}
	if err := removeSpec(specPath); err != nil {
		return err
	}
	var spec shadowSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("shadow spec %s: %w", specPath, err)
	}
	for _, slot := range []struct {
		prefix string
		n      int
	}{{"form-" + formSlot(spec.Args), shadowPerForm}, {"all", shadowAll}} {
		held, err := holdSlot(spec.Slots, slot.prefix, slot.n)
		if err != nil || !held {
			return err
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("shadow: %w", err)
	}
	if err := useMaskKey(maskKeyPath(home)); err != nil {
		// the plain mask stays: the journal holds arguments, not event texts
		fmt.Fprintf(os.Stderr, "tl: shadow: %v\n", err)
	}
	return appendJournal(spec.Journal, shadowRepeat(spec, home))
}

func shadowRepeat(spec shadowSpec, home string) shadowRecord {
	args := make([]string, len(spec.Args))
	for i, a := range spec.Args {
		args[i] = hideSafe(a)
	}
	rec := shadowRecord{Time: time.Now().Format(time.RFC3339), Args: args, FilesKeys: len(spec.Keys), FilesS: round3(spec.FilesS),
		FilesCode: spec.FilesCode, Diff: -1}
	if !spec.Cutoff.IsZero() {
		rec.Until = spec.Cutoff.Format(time.RFC3339Nano)
	}
	opts, err := parseArgs(spec.Args, spec.Now)
	if err != nil {
		rec.Error = err.Error()
		return rec
	}
	if !spec.Cutoff.IsZero() {
		opts.until = spec.Cutoff
	}
	c, err := newCHClientFromEnv()
	if err != nil {
		rec.Error = err.Error()
		return rec
	}
	refs, err := splitRefs(opts.args)
	if err != nil {
		rec.Error = err.Error()
		return rec
	}
	a := &answer{}
	e := env{home: home, configHome: os.Getenv("XDG_CONFIG_HOME"), sessionID: os.Getenv("CLAUDE_CODE_SESSION_ID"), now: spec.Now, collect: a}
	ctx, cancel := context.WithTimeout(context.Background(), shadowTimeout)
	defer cancel()
	start := time.Now()
	code, err := runDB(ctx, c, opts, e, refs, io.Discard, io.Discard)
	rec.DBS, rec.DBCode = round3(time.Since(start).Seconds()), code
	if err != nil {
		rec.Error = hideSafe(err.Error())
		return rec
	}
	keys := answerKeys(a, opts, spec.Cutoff)
	rec.DBKeys = len(keys)
	rec.Diff, rec.OnlyFiles, rec.OnlyDB = keyDiff(spec.Keys, keys)
	if code != spec.FilesCode {
		rec.Error = fmt.Sprintf("exit %d through the database, %d through files", code, spec.FilesCode)
	}
	return rec
}

// hideSafe masks what goes to the journal from the call: a pasted key may be in an argument.
func hideSafe(s string) string {
	masked, _ := hide(s)
	return masked
}

func round3(x float64) float64 { return float64(int64(x*1000+0.5)) / 1000 }

// appendJournal adds one line to the journal: the writes of concurrent repeats are whole lines.
func appendJournal(path string, rec shadowRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("shadow record: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("shadow journal: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("shadow journal: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("shadow journal: %w", err)
	}
	return errors.Join(f.Close())
}

// pickShadow decides whether this call is repeated through the database.
func pickShadow(opts options, backend string) (bool, error) {
	if backend != backendFiles || !dbForm(opts) {
		return false, nil
	}
	rate, err := shadowRate()
	if err != nil {
		return false, err
	}
	return rate > 0 && rand.Float64() < rate, nil
}
