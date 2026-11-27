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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Shadow repeats selected database answers through files and selected file answers through the database. Repeats
// run detached and write one JSON line to the shadow journal. Database-to-files repeats are capped at the database
// line counts captured when the primary answer completed; database fallbacks are journaled separately.

const (
	shadowChildEnv     = "TL_SHADOW_CHILD"
	defaultShadowRate  = 0.1
	shadowPerForm      = 2 // repeats of one form at once
	shadowAll          = 4 // repeats at once in all
	shadowSampleKeys   = 10
	shadowJournalName  = "tl-shadow.jsonl"
	shadowSlotsDirName = "tl-shadow"
	shadowLogName      = "errors.log"
)

var shadowRepeatTimeout = 30 * time.Minute
var shadowRandomFloat = rand.Float64

// answer is what one call found, for the shadow: the result of a listing or --usage, the events of tl REF.
type answer struct {
	res    *result
	refs   []hit
	filled bool
	limits map[string]int
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
	Args        []string       `json:"args"`
	Now         time.Time      `json:"now"`
	Cutoff      time.Time      `json:"cutoff"`
	Keys        []string       `json:"keys"`
	PrimaryS    float64        `json:"primary_s"`
	PrimaryCode int            `json:"primary_code"`
	Primary     string         `json:"primary"`
	Limits      map[string]int `json:"limits,omitempty"`
	Fallback    string         `json:"fallback,omitempty"`
	Started     time.Time      `json:"started"`
	Journal     string         `json:"journal"`
	Slots       string         `json:"slots"`
}

// shadowRecord is one line of the journal.
type shadowRecord struct {
	Time      string   `json:"time"`
	Primary   string   `json:"primary"`
	Fallback  string   `json:"fallback,omitempty"`
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

func boundShadowOptions(opts options, cutoff time.Time) options {
	if !cutoff.IsZero() && (opts.until.IsZero() || cutoff.Before(opts.until)) {
		opts.until = cutoff
	}
	return opts
}

// answerKeys are the events an answer found: path:line of each, a row per session for --usage.
func answerKeys(a *answer, opts options) []string {
	keys := []string{}
	if a.res == nil {
		for i := range a.refs {
			keys = append(keys, a.refs[i].src.Path+":"+strconv.Itoa(a.refs[i].ev.Line))
		}
		slices.Sort(keys)
		return keys
	}
	var hits []hit
	hits = append(hits, a.res.hits...)
	if opts.usage {
		return usageKeys(hits)
	}
	for i := range hits {
		keys = append(keys, hits[i].src.Path+":"+strconv.Itoa(hits[i].ev.Line))
	}
	slices.Sort(keys)
	return keys
}

func answerKeysThrough(a *answer, opts options, cutoff time.Time) []string {
	if a == nil {
		return nil
	}
	if cutoff.IsZero() {
		return answerKeys(a, opts)
	}
	filtered := &answer{filled: a.filled}
	if a.res == nil {
		for _, ref := range a.refs {
			if !ref.ev.Time.After(cutoff) {
				filtered.refs = append(filtered.refs, ref)
			}
		}
	} else {
		res := &result{}
		for _, h := range a.res.hits {
			if !h.ev.Time.After(cutoff) {
				res.hits = append(res.hits, h)
			}
		}
		filtered.res = res
	}
	return answerKeys(filtered, opts)
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

func shadowForm(args []string) string {
	flags := make([]string, 0, len(args))
	positionals := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positionals++
			continue
		}
		name := a
		if j := strings.IndexByte(name, '='); j >= 0 {
			name = name[:j]
		}
		flags = append(flags, name)
		if !strings.Contains(a, "=") && slices.Contains(shadowValueFlags, name) && i+1 < len(args) {
			i++
		}
	}
	return strings.Join(flags, "\x00") + fmt.Sprintf("\x00%d", positionals)
}

// shadowStarts counts the repeats this process started.
var shadowStarts atomic.Int64
var shadowSpawner = spawnShadow

// startShadow starts the repeat of the call through the database, detached from it; the call does not wait for
// it. Nothing starts when shadowPerForm repeats of this form or shadowAll in all already run. The repeat has no
// terminal: what it cannot write to the journal goes to shadowLogName beside the slots.
func startShadow(spec shadowSpec, form string) error {
	if err := os.MkdirAll(spec.Slots, 0o700); err != nil {
		return fmt.Errorf("shadow slots: %w", err)
	}
	if spec.Started.IsZero() {
		spec.Started = time.Now()
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
	if err := shadowSpawner(specPath, filepath.Join(spec.Slots, shadowLogName), spec.Primary); err != nil {
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

func spawnShadow(specPath, logPath, primary string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find tl executable: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("shadow log: %w", err)
	}
	program, prefix := exe, []string{}
	if primary == backendDB {
		if graft, lookErr := exec.LookPath("graft"); lookErr == nil {
			program = graft
			prefix = []string{"exec", "--heavy", "--local", "--", exe}
		}
	}
	cmd := exec.Command(program, prefix...)
	cmd.Env = shadowEnv(map[string]string{shadowChildEnv: specPath})
	if primary == backendDB {
		cmd.Env = shadowEnv(map[string]string{shadowChildEnv: specPath, "TL_BACKEND": backendFiles})
	}
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

func shadowEnv(values map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		keep := true
		for key := range values {
			if strings.HasPrefix(entry, key+"=") {
				keep = false
				break
			}
		}
		if keep {
			env = append(env, entry)
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
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
	if journalFallback(opts, e, code, elapsed, stderr) {
		return
	}
	if a == nil {
		return
	}
	if e.backend == backendDB {
		limits := e.shadowLimits
		if limits == nil {
			limits = a.limits
		}
		if !a.filled || code != 0 && code != exitNone || !e.shadowPicked || limits == nil {
			return
		}
		cutoff := shadowCutoff(opts, e.now)
		spec := shadowSpec{Args: opts.originalArgs, Now: e.now, Cutoff: cutoff, Keys: answerKeysThrough(a, opts, cutoff),
			PrimaryS: elapsed.Seconds(), PrimaryCode: code, Primary: backendDB, Limits: limits,
			Journal: shadowJournal(e.home), Slots: filepath.Join(stateDir(e.home), shadowSlotsDirName)}
		if err := startShadow(spec, formSlot(opts.originalArgs)); err != nil {
			printMasked(stderr, fmt.Sprintf("tl: shadow: %v\n", err))
		}
		return
	}
	_, err := readDBState(e.home)
	if err != nil {
		if e.dbTrace != nil {
			fmt.Fprintf(e.dbTrace, "tl: shadow freshness state: %v; no shadow\n", err)
		}
		return
	}
	cutoff := shadowCutoff(opts, e.now)
	spec := shadowSpec{Args: opts.originalArgs, Now: e.now, Cutoff: cutoff, Keys: answerKeysThrough(a, opts, cutoff), PrimaryS: elapsed.Seconds(),
		PrimaryCode: code, Primary: backendFiles, Journal: shadowJournal(e.home), Slots: filepath.Join(stateDir(e.home), shadowSlotsDirName)}
	if err := startShadow(spec, formSlot(opts.originalArgs)); err != nil {
		printMasked(stderr, fmt.Sprintf("tl: shadow: %v\n", err))
	}
}

func journalFallback(opts options, e env, code int, elapsed time.Duration, stderr io.Writer) bool {
	if e.fallback == nil || e.fallback.reason == "" {
		return false
	}
	filesS := elapsed.Seconds()
	if e.fallback.filesS > 0 {
		filesS = e.fallback.filesS
	}
	rec := shadowRecord{Time: time.Now().Format(time.RFC3339), Args: hideArgs(opts.originalArgs), Primary: backendDB,
		Fallback: e.fallback.reason, FilesS: round3(filesS), FilesCode: code, Diff: -1}
	if err := appendJournal(shadowJournal(e.home), rec); err != nil {
		printMasked(stderr, fmt.Sprintf("tl: shadow journal: %v\n", err))
	}
	return true
}

func hideArgs(args []string) []string {
	masked := make([]string, len(args))
	for i, arg := range args {
		masked[i] = hideSafe(arg)
	}
	return masked
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
	primary := spec.Primary
	if primary == "" {
		primary = backendFiles
	}
	rec := shadowRecord{Time: time.Now().Format(time.RFC3339), Args: args, Primary: primary, Fallback: spec.Fallback, Diff: -1}
	if primary == backendFiles {
		rec.FilesKeys, rec.FilesS, rec.FilesCode = len(spec.Keys), round3(spec.PrimaryS), spec.PrimaryCode
	} else {
		rec.DBKeys, rec.DBS, rec.DBCode = len(spec.Keys), round3(spec.PrimaryS), spec.PrimaryCode
	}
	if !spec.Cutoff.IsZero() {
		rec.Until = spec.Cutoff.Format(time.RFC3339Nano)
	}
	if !spec.Started.IsZero() && time.Since(spec.Started) >= shadowRepeatTimeout {
		rec.Error = "files repeat timed out after 30m"
		return rec
	}
	opts, err := parseArgs(spec.Args, spec.Now)
	if err != nil {
		rec.Error = err.Error()
		return rec
	}
	opts = boundShadowOptions(opts, spec.Cutoff)
	refs, err := splitRefs(opts.args)
	if err != nil {
		rec.Error = err.Error()
		return rec
	}
	a := &answer{}
	e := env{home: home, configHome: os.Getenv("XDG_CONFIG_HOME"), sessionID: os.Getenv("CLAUDE_CODE_SESSION_ID"), now: spec.Now, collect: a}
	remaining := shadowRepeatTimeout
	if !spec.Started.IsZero() {
		remaining -= time.Since(spec.Started)
	}
	ctx, cancel := context.WithTimeout(context.Background(), max(remaining, time.Millisecond))
	defer cancel()
	start := time.Now()
	var code int
	var c *chClient
	if primary == backendFiles {
		var clientErr error
		c, clientErr = newCHClientFromEnv()
		if clientErr != nil {
			rec.Error = hideSafe(clientErr.Error())
			return rec
		}
		code, err = runDB(ctx, c, opts, e, refs, io.Discard, io.Discard)
		rec.DBS, rec.DBCode = round3(time.Since(start).Seconds()), code
	} else {
		e.backend = backendFiles
		fileAnswer := &answer{}
		e.collect = fileAnswer
		done := make(chan int, 1)
		go func() { done <- runOutput(opts, e, io.Discard, io.Discard) }()
		select {
		case code = <-done:
		case <-ctx.Done():
			rec.Error = "files repeat timed out after 30m"
			return rec
		}
		keys := filterShadowLines(answerKeysThrough(fileAnswer, opts, spec.Cutoff), spec.Limits)
		rec.FilesS, rec.FilesCode, rec.FilesKeys = round3(time.Since(start).Seconds()), code, len(keys)
		rec.Diff, rec.OnlyFiles, rec.OnlyDB = keyDiff(keys, spec.Keys)
		if code != spec.PrimaryCode {
			rec.Error = fmt.Sprintf("exit %d through files, %d through database", code, spec.PrimaryCode)
		}
		return rec
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			rec.Error = "files repeat timed out after 30m"
		} else {
			rec.Error = hideSafe(err.Error())
		}
		return rec
	}
	limits, err := shadowLineLimits(c)
	if err != nil {
		rec.Error = err.Error()
		return rec
	}
	spec.Keys = filterShadowLines(spec.Keys, limits)
	keys := filterShadowLines(answerKeys(a, opts), limits)
	rec.DBKeys = len(keys)
	rec.Diff, rec.OnlyFiles, rec.OnlyDB = keyDiff(spec.Keys, keys)
	if code != spec.PrimaryCode {
		rec.Error = fmt.Sprintf("exit %d through the database, %d through files", code, spec.PrimaryCode)
	}
	return rec
}

func shadowLineLimits(c *chClient) (map[string]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), shadowRepeatTimeout)
	defer cancel()
	rows, err := c.SelectTSV(ctx, chQuery{Name: "shadow_files", SQL: "SELECT path, lines FROM files FINAL"})
	if err != nil {
		return nil, err
	}
	limits := make(map[string]int, len(rows))
	for _, row := range rows {
		if len(row) != 2 {
			return nil, fmt.Errorf("shadow files: want 2 fields, got %d", len(row))
		}
		n, err := strconv.Atoi(row[1])
		if err != nil {
			return nil, fmt.Errorf("shadow files lines %q: %w", row[1], err)
		}
		limits[row[0]] = n
	}
	return limits, nil
}

func filterShadowLines(keys []string, limits map[string]int) []string {
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		path, raw, ok := strings.Cut(key, ":")
		if !ok {
			if strings.Contains(key, "\t") {
				out = append(out, key)
			}
			continue
		}
		line, err := strconv.Atoi(raw)
		if err == nil && line <= limits[path] {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
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
	if !dbForm(opts) || backend != backendFiles && backend != backendDB {
		return false, nil
	}
	rate, err := shadowRate()
	if err != nil {
		return false, err
	}
	if rate == 0 {
		return false, nil
	}
	if backend == backendDB {
		form := shadowForm(opts.originalArgs)
		if unseen, err := rememberShadowForm(form); err != nil {
			return false, err
		} else if unseen {
			return true, nil
		}
	}
	return shadowRandomFloat() < rate, nil
}

func rememberShadowForm(form string) (_ bool, retErr error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, fmt.Errorf("find home for shadow forms: %w", err)
	}
	dir := filepath.Join(stateDir(home), shadowSlotsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("create shadow slots directory: %w", err)
	}
	path := filepath.Join(dir, "seen-forms")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return false, fmt.Errorf("open seen shadow forms: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close seen shadow forms: %w", err))
		}
	}()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false, fmt.Errorf("seek seen shadow forms: %w", err)
	}
	b, err := io.ReadAll(file)
	if err != nil {
		return false, fmt.Errorf("read seen shadow forms: %w", err)
	}
	if slices.Contains(strings.Split(string(b), "\n"), form) {
		return false, nil
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return false, fmt.Errorf("seek end of seen shadow forms: %w", err)
	}
	if _, err := fmt.Fprintln(file, form); err != nil {
		return false, fmt.Errorf("append seen shadow form: %w", err)
	}
	return true, nil
}
