// tl — транскрипты сессий агентов коротким ответом для агента: окно по времени и роли (человек, агент, команда,
// вывод, письма сессий, ответы в окне вопроса), поиск по одной сессии или по всем проектам. Строка события несёт
// ссылку id:строка, полный текст — `tl id:строка`. Вбирает user-word и поиск реплик из prior.
//
// Коды выхода: 0 — события есть, 1 — ничего не нашлось, 2 — ошибка вызова или чтения.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aiseeq/akit/buildversion"
	"github.com/aiseeq/akit/clihelp"
	"github.com/aiseeq/akit/timearg"
	"github.com/aiseeq/akit/transcript"
	"github.com/aiseeq/akit/transcript/replycache"
	"github.com/aiseeq/akit/transcriptdb"
)

const usage = `tl — session transcripts, briefly: a time/role window or a search.

Usage:
  tl [SESSION...] [flags]     events of sessions (default: this session)
  tl --all [flags]            all Claude Code transcripts (default --since 7d)
  tl REF [REF...]              events in full; REF is id:line from a listing
  tl --version                 print the build version
  tl --help-json               print machine-readable help

  ID in a link may be longer than 8 characters when needed to distinguish sessions.

SESSION is a live session name (main), a session id or its prefix, or a .jsonl
path (Claude Code or codex rollout).

  --role R,R    user ask peer agent cmd out note, or all (assistant = agent,
                tool = cmd: the names in jsonl)
                (default user,ask,peer,agent: the conversation)
  --since T     from time T: 12:05, 25.10, "25.10 12:05", 2026-10-25T10:05,
                2026-10-25T10:05:00, 2026-10-25T10:05:00Z, 2026-10-25T10:05:00+01:00,
                or ago: 30m 2h 3d (default --all window: 7d)
  --until T     up to time T, same forms
  --grep RE     events whose text matches RE (case-insensitive)
                Unparsable line counts include only lines retained for parsing.
  --raw RE      raw jsonl lines matching RE (case-insensitive), e.g. --raw '"hook_cancelled"';
                masked like all output (see Masking)
  -C N          with --grep: N events around each hit
  --all         all transcripts in ~/.claude/projects, including subagents
  --no-subagents with --all: omit subagent transcripts
  --subagents   with SESSION: include that session's subagents
  --codex       with --all: also codex rollouts of worker fighters
  --project S   with --all: project folders containing S; codex checks the
                transcript path and working directory (worker task directory if
                the session has no cwd)
  --max N       last N events (default 100, 0 = all)
  --width N     characters of text per event (default 300, 0 = whole)
  --usage       tokens, API cost, and subscription-week percentage instead of
                events: a TSV row per session and the total (--role defaults to all); the
                summary names the transcripts read: runs that leave none
                (claude -p --no-session-persistence) are not in it; --codex
                includes token_count from codex rollouts
  --open-questions  list question sentences and whether the user replied (SESSION only)
                One row per sentence with '?', linked to its event. Replies are
                matched by file order. AskUserQuestion calls whose window was
                declined or not shown are skipped. '?' inside URLs, code and quotes
                does not count. Fenced code is skipped; the last paragraph is
                the prose before a trailing code fence. Tool output, peer and
                subagent messages and reminder-only messages do not count.
                No questions is an empty successful result (exit 0).
  --worktime    Jira worktime for --grep; a key chunk starts at a key event
                and ends at the last activity within --key-gap after the last
                key match (inclusive; default 30m). Activity events: messages
                (user, ask, peer, agent, notes), tool calls and results; gaps > --gap
                split activity (default 15m). Key roles: user, agent, cmd,
                note/prompt, peer; out is not searched.
                A bare --grep key matches as a whole word (any script) only in
                --worktime; a key with regex characters stays as written.
                Key mentions in command arguments and probes count as events.
                Activity is clipped to --since/--until. Subagents launched
                inside a parent key chunk (through one minute after its end)
                count all activity; a brief with a key uses its own key chunks.
                A codex rollout without a key in its own prompt, text or
                commands is not counted; codex rollouts need --codex. A lone event
                is 1m. Midnight crossings split at local midnight; day rows
                round up to 15m and total sums the rounded rows.
  --gap D       activity split threshold (default 15m)
  --key-gap D   maximum time after a key match to keep a key chunk open (default 30m)
  --wait PROG [SUB]  nearest-rank percentile of program wait time in milliseconds
                (default --q 50; example: --all --since 24h --wait 'tk done' --q 95)
                Completed calls are cached in $XDG_CACHE_HOME/akit/tl-wait (or the user cache directory)
  --wait-all    TSV per start day, program and subcommand; wrapper-only commands use their first program;
                subcommand * is the row over all calls of the program that day
  --q N         percentile for --wait, from 1 to 99 (default 50); needs at least
                3 events for --q 50 and below, 20 above
  Large reads wait for machine capacity through graft's heavy queue.

Roles: user — typed by the human; ask — the human's answer in a question
dialog; peer — a message from another session; agent — the agent's text;
think — the agent's thinking (often empty: not saved); cmd — a tool call (Bash
command as is); out — tool output; attach — a Claude Code attachment (hook,
diagnostics, …); note — background task notices, compaction, briefs and
program prompts, Claude Code system records (Who system), codex token counts.
Masking: all output, --raw too, has secret values replaced with
[скрыто:KIND len=N #TAG]: N is the hidden value's length in characters, TAG a
hash of it under a local key ($XDG_STATE_HOME/akit/mask-key, by default
~/.local/state/akit/mask-key, created on first run). Equal values get equal
TAGs across tl calls; a TAG holds 24 bits, so different values rarely share
one. The value itself is not recoverable. The mask also hides some names that look like keys (a long
pk_test_… identifier): N tells whether it may be the name you expect, and the
same TAG elsewhere shows where the same text recurs. Without the key the
markers are [скрыто:KIND] and stderr says why.
Database: TL_BACKEND=db answers --all, --grep, --raw, --usage, SESSION and REF from
the ClickHouse tables of the transcript loader (TL_CLICKHOUSE_URL, default
http://127.0.0.1:18123; TL_CLICKHOUSE_DB, default tr; read-only). Files missing
from the database are not visible. REF may read its requested line from disk.
Every database answer reports its freshness time in local time. If its state is missing, stale,
unhealthy, for another home, or the database fails, stderr says so and tl reads
the files. The default TL_BACKEND=files repeats a
share of calls (TL_SHADOW_RATE, default 0.1, 0 off) through the database in a
background process after the answer and appends one JSON line to
TL_SHADOW_JOURNAL (default $XDG_STATE_HOME/akit/tl-shadow.jsonl): event
counts of both paths up to the call's start or --until, "diff" (-1 when the
repeat failed), both times. TL_DB_TRACE=1 prints the time of each query.
Exit: 0 found, 1 none, 2 usage/IO error.
`

const (
	exitNone  = 1
	exitUsage = 2
)

type options struct {
	roles         map[transcript.Role]bool
	since         time.Time
	until         time.Time
	grep          string
	raw           string
	context       int
	all           bool
	codex         bool
	noSubagents   bool
	subagents     bool
	project       string
	max           int
	width         int
	usage         bool
	openQuestions bool
	worktime      bool
	gap           time.Duration
	gapSet        bool
	keyGap        time.Duration
	keyGapSet     bool
	waitProg      string
	waitSub       string
	waitAll       bool
	q             int
	qSet          bool
	maxSet        bool
	widthSet      bool
	rolesSet      bool
	version       bool
	args          []string
	originalArgs  []string
	waitCache     *waitCache
	replyCache    *replycache.Cache
}

var errHelp = errors.New("help")
var errHelpJSON = errors.New("help json")
var machineHelp = func() clihelp.Help {
	help := clihelp.NewRoot("tl", strings.Fields("-h --help --help-json --all --codex --no-subagents --subagents --usage --open-questions --worktime --version --role --since --until --gap --key-gap --grep --raw -C --project --wait --wait-all --q --max --width"), strings.Fields("--role --since --until --gap --key-gap --grep --raw -C --project --wait --q --max --width"), "args", "read session events or references with optional filters")
	help.Commands[0].NoDashdash = true
	return help
}()

func main() {
	if spec := os.Getenv(shadowChildEnv); spec != "" {
		if err := runShadowChild(spec); err != nil {
			fmt.Fprintf(os.Stderr, "%s tl: shadow: %v\n", time.Now().Format(time.RFC3339), err)
			os.Exit(exitUsage)
		}
		os.Exit(0)
	}
	os.Exit(start(os.Args[1:], time.Now(), os.Stdout, os.Stderr))
}

// start parses the arguments and runs tl. What it prints itself is masked like the output of run: an
// error quotes the argument, and a pasted key may be one.
func start(args []string, now time.Time, stdout, stderr io.Writer) int {
	begin := time.Now()
	opts, err := parseArgs(args, now)
	if errors.Is(err, errHelp) {
		printMasked(stdout, usage)
		return 0
	}
	if errors.Is(err, errHelpJSON) {
		if err := clihelp.Print(stdout, machineHelp); err != nil {
			fmt.Fprintf(stderr, "tl: write help: %v\n", err)
			return exitUsage
		}
		return 0
	}
	if err != nil {
		printMasked(stderr, fmt.Sprintf("tl: %v\n(tl -h for usage)\n", err))
		return exitUsage
	}
	home, err := os.UserHomeDir()
	if err != nil {
		printMasked(stderr, fmt.Sprintf("tl: %v\n", err))
		return exitUsage
	}
	if err := useMaskKey(maskKeyPath(home)); err != nil {
		printMasked(stderr, fmt.Sprintf("tl: %v\n", err))
	}
	e := env{home: home, configHome: os.Getenv("XDG_CONFIG_HOME"), sessionID: os.Getenv("CLAUDE_CODE_SESSION_ID"), now: now,
		backend: backendFromEnv(stderr)}
	if os.Getenv("TL_DB_TRACE") != "" {
		e.dbTrace = stderr
	}
	if pick, err := pickShadow(opts, e.backend); err != nil {
		printMasked(stderr, fmt.Sprintf("tl: %v; no shadow\n", err))
	} else if pick {
		e.collect = &answer{}
	}
	code := run(opts, e, stdout, stderr)
	shadowAfter(e.collect, opts, e, code, time.Since(begin), stderr)
	return code
}

// backendFromEnv is TL_BACKEND: files (the default) or db.
func backendFromEnv(stderr io.Writer) string {
	switch b := os.Getenv("TL_BACKEND"); b {
	case "", backendFiles:
		return backendFiles
	case backendDB:
		return backendDB
	default:
		printMasked(stderr, fmt.Sprintf("tl: TL_BACKEND=%q is not files or db; reading files\n", b))
		return backendFiles
	}
}

// printMasked prints like fmt.Fprint, with secret values masked.
func printMasked(w io.Writer, text string) {
	masked, _ := hide(text)
	fmt.Fprint(w, masked)
}

// env — то, что tl берёт из окружения; в тестах подменяется.
type env struct {
	home       string
	configHome string
	sessionID  string
	now        time.Time
	backend    string    // TL_BACKEND: files or db
	dbThrough  time.Time // freshness boundary from tlfeed state.json
	collect    *answer   // set when the shadow repeats this call: what the answer found
	dbTrace    io.Writer // TL_DB_TRACE: the time of each database query
}

func parseArgs(args []string, now time.Time) (options, error) {
	opts := options{max: 100, width: 300, q: 50, gap: defaultWorkGap, keyGap: defaultKeyGap}
	opts.originalArgs = append([]string(nil), args...)
	if err := setRoles(&opts, "user,ask,peer,agent"); err != nil {
		return opts, err
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		if !strings.HasPrefix(a, "-") || a == "-" {
			opts.args = append(opts.args, a)
			continue
		}
		if name == "--help-json" {
			return opts, errHelpJSON
		}
		if name == "-h" || name == "--help" {
			return opts, errHelp
		}
		if setSwitch(&opts, name) {
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return opts, fmt.Errorf("%s needs a value", name)
			}
			i++
			val = args[i]
		}
		if err := setFlag(&opts, name, val, now); err != nil {
			return opts, err
		}
	}
	if opts.waitProg != "" || opts.waitAll {
		if !opts.rolesSet {
			if err := setRoles(&opts, "all"); err != nil {
				return opts, err
			}
		}
	}
	if opts.usage && !opts.rolesSet {
		if err := setRoles(&opts, "all"); err != nil {
			return opts, err
		}
	}
	return opts, checkCombos(opts)
}

// setSwitch ставит флаг без значения; false — это не переключатель.
func setSwitch(opts *options, name string) bool {
	switch name {
	case "--all":
		opts.all = true
	case "--codex":
		opts.codex = true
	case "--no-subagents":
		opts.noSubagents = true
	case "--subagents":
		opts.subagents = true
	case "--usage":
		opts.usage = true
	case "--open-questions":
		opts.openQuestions = true
	case "--worktime":
		opts.worktime = true
	case "--wait-all":
		opts.waitAll = true
	case "--version":
		opts.version = true
	default:
		return false
	}
	return true
}

// checkCombos отвергает флаги, которые не работают друг без друга или вместе.
func checkCombos(opts options) error {
	if err := checkWorktimeCombos(opts); err != nil {
		return err
	}
	if err := checkEventOutputCombos(opts); err != nil {
		return err
	}
	if err := checkScopeCombos(opts); err != nil {
		return err
	}
	if err := checkWaitCombos(opts); err != nil {
		return err
	}
	return checkOpenQuestionsCombos(opts)
}

func checkScopeCombos(opts options) error {
	if (opts.codex || opts.project != "" || opts.noSubagents) && !opts.all {
		return errors.New("--codex, --project and --no-subagents work with --all")
	}
	if opts.subagents && opts.all {
		return errors.New("--subagents works with SESSION, not --all")
	}
	return nil
}

func checkWaitCombos(opts options) error {
	if opts.waitProg != "" || opts.waitAll {
		if opts.usage {
			return errors.New("--wait and --usage cannot be used together")
		}
		if opts.rolesSet || opts.grep != "" || opts.raw != "" || opts.context > 0 || opts.maxSet || opts.widthSet {
			return errors.New("--wait cannot be used with event output options")
		}
		if opts.waitAll && opts.waitProg != "" {
			return errors.New("--wait-all and --wait cannot be used together")
		}
		if opts.waitAll && opts.qSet {
			return errors.New("--q works with --wait")
		}
	} else if opts.qSet {
		return errors.New("--q works with --wait")
	}
	return nil
}

func checkOpenQuestionsCombos(opts options) error {
	if opts.openQuestions && openQuestionsOutputOptionsSet(opts) {
		return errors.New("--open-questions works with SESSION, not event output options")
	}
	return nil
}

func openQuestionsOutputOptionsSet(opts options) bool {
	return opts.all || opts.subagents || opts.usage || opts.worktime || opts.waitProg != "" || opts.waitAll || opts.qSet ||
		opts.rolesSet || !opts.since.IsZero() || !opts.until.IsZero() || opts.grep != "" || opts.raw != "" ||
		opts.maxSet || opts.widthSet
}

func checkWorktimeCombos(opts options) error {
	if opts.worktime && !opts.all {
		return errors.New("--worktime works with --all")
	}
	if opts.worktime && opts.grep == "" {
		return errors.New("--worktime needs --grep")
	}
	if opts.worktime && opts.usage {
		return errors.New("--worktime and --usage cannot be used together")
	}
	if opts.worktime && (opts.waitProg != "" || opts.waitAll) {
		return errors.New("--worktime and --wait cannot be used together")
	}
	if !opts.worktime && opts.gapSet {
		return errors.New("--gap works with --worktime")
	}
	if !opts.worktime && opts.keyGapSet {
		return errors.New("--key-gap works with --worktime")
	}
	if opts.worktime && worktimeOutputFlagSet(opts) {
		return errors.New("--worktime cannot be used with --raw, --max, --width, --role or -C")
	}
	return nil
}

func worktimeOutputFlagSet(opts options) bool {
	return opts.raw != "" || opts.maxSet || opts.widthSet || opts.rolesSet || opts.context > 0
}

func checkEventOutputCombos(opts options) error {
	if opts.context > 0 && opts.grep == "" {
		return errors.New("-C works with --grep")
	}
	return nil
}

func setFlag(opts *options, name, val string, now time.Time) error {
	if name == "--role" {
		opts.rolesSet = true
		return setRoles(opts, val)
	}
	if name == "--since" || name == "--until" {
		parsed, err := timearg.Parse(val, now)
		if name == "--since" {
			opts.since = parsed
		} else {
			opts.until = parsed
		}
		return err
	}
	if name == "--gap" || name == "--key-gap" {
		if name == "--gap" {
			opts.gapSet = true
		} else {
			opts.keyGapSet = true
		}
		gap, err := time.ParseDuration(val)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if gap <= 0 {
			return fmt.Errorf("%s needs a duration > 0, got %q", name, val)
		}
		if name == "--gap" {
			opts.gap = gap
		} else {
			opts.keyGap = gap
		}
		return nil
	}
	return setOtherFlag(opts, name, val)
}

func setOtherFlag(opts *options, name, val string) error {
	var err error
	switch name {
	case "--grep":
		opts.grep = val
	case "--raw":
		opts.raw = val
	case "-C":
		opts.context, err = nonNegative(name, val)
	case "--project":
		opts.project = val
	case "--wait":
		fields := strings.Fields(val)
		if len(fields) < 1 || len(fields) > 2 {
			return errors.New("--wait needs PROG [SUB]")
		}
		opts.waitProg = fields[0]
		if len(fields) == 2 {
			opts.waitSub = fields[1]
		}
	case "--q":
		opts.q, err = strconv.Atoi(val)
		opts.qSet = true
		if err == nil && (opts.q < 1 || opts.q > 99) {
			err = fmt.Errorf("--q needs a number from 1 to 99, got %q", val)
		}
	case "--max":
		opts.max, err = nonNegative(name, val)
		opts.maxSet = true
	case "--width":
		opts.width, err = nonNegative(name, val)
		opts.widthSet = true
	default:
		if countFlags[name] {
			return fmt.Errorf("unknown flag %s: the number of events is --max N", name)
		}
		return fmt.Errorf("unknown flag %s", name)
	}
	return err
}

// countFlags — names other tools give to an output limit; tl answers with its own flag instead of a second name for it.
var countFlags = map[string]bool{"--limit": true, "--head": true, "-n": true}

func nonNegative(name, val string) (int, error) {
	n, err := strconv.Atoi(val)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s needs a number >= 0, got %q", name, val)
	}
	return n, nil
}

// roleAliases — имена ролей, как они записаны в jsonl: агенты пишут их в --role естественно.
var roleAliases = map[string]transcript.Role{"assistant": transcript.Agent, "tool": transcript.Cmd, "tool_use": transcript.Cmd, "bash": transcript.Cmd, "result": transcript.Out}

func setRoles(opts *options, list string) error {
	opts.roles = map[transcript.Role]bool{}
	for r := range strings.SplitSeq(list, ",") {
		r = strings.TrimSpace(r)
		if role, ok := roleAliases[r]; ok {
			opts.roles[role] = true
			continue
		}
		if r == "all" {
			for _, role := range transcript.Roles {
				opts.roles[role] = true
			}
			continue
		}
		known := false
		for _, role := range transcript.Roles {
			if string(role) == r {
				opts.roles[role], known = true, true
			}
		}
		if !known {
			names := make([]string, 0, len(transcript.Roles))
			for _, role := range transcript.Roles {
				names = append(names, string(role))
			}
			return fmt.Errorf("unknown role %q (roles: %s all)", r, strings.Join(names, " "))
		}
	}
	return nil
}

// run prints what runOutput prints with secret values masked: an event quoted verbatim may hold a key a
// user pasted, and tl output goes on to other agents and models. Event text that is printed across
// lines is masked as one event; the one-line listing is masked one line at a time.
func run(opts options, e env, stdout, stderr io.Writer) int {
	var out, errs bytes.Buffer
	code := runOutput(opts, e, &out, &errs)
	if err := errors.Join(writeMasked(stdout, out.String()), writeMasked(stderr, errs.String())); err != nil {
		fmt.Fprintf(stderr, "tl: write output: %v\n", err)
		return exitUsage
	}
	return code
}

// parallelMask is the shortest output masked in pieces of whole lines on all processors: each line is masked on
// its own, so the pieces do not depend on each other.
const parallelMask = 256 << 10

// writeMasked writes text masked line by line.
func writeMasked(w io.Writer, text string) error {
	return writeMaskedWith(w, text, writeMaskedLines)
}

func writeMaskedWith(w io.Writer, text string, mask func(io.Writer, string) error) error {
	if len(text) < parallelMask {
		return mask(w, text)
	}
	var pieces []string
	size := len(text)/(4*runtime.GOMAXPROCS(0)) + 1
	for len(text) > 0 {
		end := len(text)
		if size < len(text) {
			if nl := strings.IndexByte(text[size:], '\n'); nl >= 0 {
				end = size + nl + 1
			}
		}
		pieces, text = append(pieces, text[:end]), text[end:]
	}
	masked := make([]strings.Builder, len(pieces))
	errs := make([]error, len(pieces))
	var wg sync.WaitGroup
	for i := range pieces {
		wg.Go(func() { errs[i] = mask(&masked[i], pieces[i]) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	for i := range masked {
		if _, err := io.WriteString(w, masked[i].String()); err != nil {
			return fmt.Errorf("write masked text: %w", err)
		}
	}
	return nil
}

func writeMaskedLines(w io.Writer, text string) error {
	bw := bufio.NewWriterSize(w, 64<<10)
	for len(text) > 0 {
		end := strings.IndexByte(text, '\n')
		if end < 0 {
			masked, _ := hide(text)
			_, err := io.WriteString(bw, masked)
			if err != nil {
				return fmt.Errorf("write masked text: %w", err)
			}
			if err := bw.Flush(); err != nil {
				return fmt.Errorf("write masked text: %w", err)
			}
			return nil
		}
		line, newline := text[:end], "\n"
		if before, ok := strings.CutSuffix(line, "\r"); ok {
			line, newline = before, "\r\n"
		}
		masked, _ := hide(line)
		if _, err := io.WriteString(bw, masked); err != nil {
			return fmt.Errorf("write masked line: %w", err)
		}
		if _, err := io.WriteString(bw, newline); err != nil {
			return fmt.Errorf("write line ending: %w", err)
		}
		text = text[end+1:]
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("write masked text: %w", err)
	}
	return nil
}

var newReplyCache = replycache.New

func replyCacheFor(opts options, now time.Time) *replycache.Cache {
	if !opts.all || opts.usage || opts.waitProg != "" || opts.waitAll || opts.raw != "" || len(opts.roles) == 0 {
		return nil
	}
	for role := range opts.roles {
		if role != transcript.User && role != transcript.Ask {
			return nil
		}
	}
	cache := newReplyCache()
	cache.Cleanup(now)
	return cache
}

func runOutput(opts options, e env, stdout, stderr io.Writer) int {
	if opts.version {
		fmt.Fprintln(stdout, buildversion.String("tl"))
		return 0
	}
	refs, err := splitRefs(opts.args)
	if err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage
	}
	if opts.openQuestions && len(refs) > 0 {
		fmt.Fprintln(stderr, "tl: --open-questions needs a SESSION, not an event reference")
		return exitUsage
	}
	if e.backend == backendDB && dbForm(opts) {
		code, ok, err := runOutputDB(opts, e, refs, stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "tl: %v\n", err)
		}
		if ok {
			return code
		}
	}
	if len(refs) > 0 {
		return reportRefs(refs, opts, e, stdout, stderr)
	}
	return runOutputSessions(opts, e, stdout, stderr)
}

// runOutputDB answers through the database; false — it could not, nothing is printed but the warning, and the
// caller reads the files.
func runOutputDB(opts options, e env, refs []eventRef, stdout, stderr io.Writer) (int, bool, error) {
	state, err := readDBState(e.home)
	if err != nil {
		return 0, false, fmt.Errorf("TL_BACKEND=db: %w; reading files instead", err)
	}
	e.dbThrough = state.DBThrough
	c, err := newCHClientFromEnv()
	if err == nil {
		var out, errs bytes.Buffer
		var code int
		if code, err = runDB(context.Background(), c, opts, e, refs, &out, &errs); err == nil {
			errs.WriteString(transcriptdb.FreshnessLine("tl", e.dbThrough, time.Now()))
			_, outErr := io.Copy(stdout, &out)
			_, errErr := io.Copy(stderr, &errs)
			if err := errors.Join(outErr, errErr); err != nil {
				return exitUsage, true, fmt.Errorf("write output: %w", err)
			}
			return code, true, nil
		}
	}
	if e.collect != nil {
		*e.collect = answer{}
	}
	return 0, false, fmt.Errorf("TL_BACKEND=db: %w; reading files instead", err)
}

type dbState = transcriptdb.State

func readDBState(home string) (dbState, error) { return transcriptdb.CheckHealth(home) }

func reportRefs(refs []eventRef, opts options, e env, stdout, stderr io.Writer) int {
	// REF — событие целиком; режет его только явный --width, не ширина списка по умолчанию.
	width := 0
	if opts.widthSet {
		width = opts.width
	}
	code := 0
	if e.collect != nil {
		e.collect.filled = true
	}
	for _, ref := range refs {
		if result := showRef(ref.id, ref.line, width, e, stdout, stderr); result > code {
			code = result
		}
	}
	return code
}

func runOutputSessions(opts options, e env, stdout, stderr io.Writer) int {
	sources, err := resolve(opts, e)
	if err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage
	}
	if opts.waitProg != "" || opts.waitAll {
		opts.waitCache = newWaitCache(stderr)
		opts.waitCache.cleanup(e.now)
	}
	opts.replyCache = replyCacheFor(opts, e.now)
	f, err := newFilter(opts)
	if err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage
	}
	code, done, err := admission(sources, opts.originalArgs, stdout, stderr, opts.waitCache, opts.replyCache, len(f.needles) > 0)
	if done {
		if err != nil {
			fmt.Fprintf(stderr, "tl: %v\n", err)
		}
		return code
	}
	if opts.worktime {
		return reportWorktime(sources, opts, e, stdout, stderr)
	}
	if opts.openQuestions {
		return reportOpenQuestions(sources, stdout, stderr)
	}
	res, err := scan(sources, opts, e.home)
	if err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage
	}
	if opts.waitProg != "" || opts.waitAll {
		return reportWaitOutput(res, sources, opts, stdout, stderr)
	}
	if e.collect != nil {
		e.collect.res, e.collect.filled = &res, true
	}
	if opts.usage {
		return reportUsage(res, sources, opts, e, stdout)
	}
	return report(res, opts, e, stdout)
}

func reportWaitOutput(res result, sources []source, opts options, stdout, stderr io.Writer) int {
	if opts.waitAll {
		if len(res.errs) > 0 {
			reportWaitAll(res, stdout)
			fmt.Fprintln(stderr, res.errs[0])
			if len(res.waitRows) < minWaitEvents(opts.q) {
				return 3
			}
			return 0
		}
	}
	if n := opts.waitCache.misses.Load(); n > 0 {
		// Причина промаха — запись под этот код ещё не писалась или транскрипт после неё дописан.
		fmt.Fprintf(stderr, "tl: wait cache: %d of %d transcript(s) read without cache (no usable entry: first read under this tl code, a changed transcript, or a missing or damaged entry)\n", n, len(sources))
	}
	if opts.waitAll {
		return reportWaitAll(res, stdout)
	}
	return reportWait(res, opts, stdout, stderr)
}
