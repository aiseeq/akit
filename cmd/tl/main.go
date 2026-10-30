// tl — транскрипты сессий агентов коротким ответом для агента: окно по времени и роли (человек, агент, команда,
// вывод, письма сессий, ответы в окне вопроса), поиск по одной сессии или по всем проектам. Строка события несёт
// ссылку id:строка, полный текст — `tl id:строка`. Вбирает user-word и поиск реплик из prior.
//
// Коды выхода: 0 — события есть, 1 — ничего не нашлось, 2 — ошибка вызова или чтения.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/timearg"
	"github.com/aiseeq/akit/transcript"
)

const usage = `tl — session transcripts, briefly: a time/role window or a search.

Usage:
  tl [SESSION...] [flags]     events of sessions (default: this session)
  tl --all [flags]            all Claude Code transcripts (default --since 7d)
  tl REF                      one event in full; REF is id:line from a listing

SESSION is a live session name (main), a session id or its prefix, or a .jsonl
path (Claude Code or codex rollout).

  --role R,R    user ask peer agent cmd out note, or all (assistant = agent,
                tool = cmd: the names in jsonl)
                (default user,ask,peer,agent: the conversation)
  --since T     from time T: 12:05, 25.10, "25.10 12:05", 2026-10-25T10:05:00Z,
                or ago: 30m 2h 3d
  --until T     up to time T, same forms
  --grep RE     events whose text matches RE (case-insensitive)
  --raw RE      raw jsonl lines matching RE (case-insensitive), e.g. --raw '"hook_cancelled"'
  -C N          with --grep: N events around each hit
  --all         all transcripts in ~/.claude/projects, including subagents
  --no-subagents with --all: omit subagent transcripts
  --subagents   with SESSION: include that session's subagents
  --codex       with --all: also codex rollouts of worker fighters
  --project S   with --all: project folders containing S
  --max N       last N events (default 100, 0 = all)
  --width N     characters of text per event (default 300, 0 = whole)
  --usage       tokens and API cost of model replies instead of events: a TSV
                row per session and the total (--role defaults to all)

Roles: user — typed by the human; ask — the human's answer in a question
dialog; peer — a message from another session; agent — the agent's text;
think — the agent's thinking (often empty: not saved); cmd — a tool call (Bash
command as is); out — tool output; attach — a Claude Code attachment (hook,
diagnostics, …); note — background task notices, compaction, briefs and
program prompts, Claude Code system records (Who system), codex token counts.
Exit: 0 events found, 1 none, 2 usage/IO error.
`

const (
	exitNone  = 1
	exitUsage = 2
)

type options struct {
	roles       map[transcript.Role]bool
	since       time.Time
	until       time.Time
	grep        string
	raw         string
	context     int
	all         bool
	codex       bool
	noSubagents bool
	subagents   bool
	project     string
	max         int
	width       int
	usage       bool
	rolesSet    bool
	args        []string
}

var errHelp = errors.New("help")

func main() {
	opts, err := parseArgs(os.Args[1:], time.Now())
	if errors.Is(err, errHelp) {
		fmt.Print(usage)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "tl: %v\n(tl -h for usage)\n", err)
		os.Exit(exitUsage)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tl: %v\n", err)
		os.Exit(exitUsage)
	}
	os.Exit(run(opts, env{home: home, sessionID: os.Getenv("CLAUDE_CODE_SESSION_ID"), now: time.Now()}, os.Stdout, os.Stderr))
}

// env — то, что tl берёт из окружения; в тестах подменяется.
type env struct {
	home      string
	sessionID string
	now       time.Time
}

func parseArgs(args []string, now time.Time) (options, error) {
	opts := options{max: 100, width: 300}
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
		switch name {
		case "-h", "--help":
			return opts, errHelp
		case "--all":
			opts.all = true
			continue
		case "--codex":
			opts.codex = true
			continue
		case "--no-subagents":
			opts.noSubagents = true
			continue
		case "--subagents":
			opts.subagents = true
			continue
		case "--usage":
			opts.usage = true
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
	if opts.usage && !opts.rolesSet {
		if err := setRoles(&opts, "all"); err != nil {
			return opts, err
		}
	}
	if opts.context > 0 && opts.grep == "" {
		return opts, errors.New("-C works with --grep")
	}
	if (opts.codex || opts.project != "" || opts.noSubagents) && !opts.all {
		return opts, errors.New("--codex, --project and --no-subagents work with --all")
	}
	if opts.subagents && opts.all {
		return opts, errors.New("--subagents works with SESSION, not --all")
	}
	return opts, nil
}

func setFlag(opts *options, name, val string, now time.Time) error {
	var err error
	switch name {
	case "--role":
		opts.rolesSet = true
		return setRoles(opts, val)
	case "--since":
		opts.since, err = timearg.Parse(val, now)
	case "--until":
		opts.until, err = timearg.Parse(val, now)
	case "--grep":
		opts.grep = val
	case "--raw":
		opts.raw = val
	case "-C":
		opts.context, err = nonNegative(name, val)
	case "--project":
		opts.project = val
	case "--max":
		opts.max, err = nonNegative(name, val)
	case "--width":
		opts.width, err = nonNegative(name, val)
	default:
		return fmt.Errorf("unknown flag %s", name)
	}
	return err
}

func nonNegative(name, val string) (int, error) {
	n, err := strconv.Atoi(val)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s needs a number >= 0, got %q", name, val)
	}
	return n, nil
}

// roleAliases — имена ролей, как они записаны в jsonl: агенты пишут их в --role естественно.
var roleAliases = map[string]transcript.Role{"assistant": transcript.Agent, "tool": transcript.Cmd}

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

func run(opts options, e env, stdout, stderr io.Writer) int {
	ref, line, err := splitRef(opts.args)
	if err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage
	}
	if ref != "" {
		return showRef(ref, line, e, stdout, stderr)
	}
	sources, err := resolve(opts, e)
	if err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage
	}
	res, err := scan(sources, opts)
	if err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage
	}
	if opts.usage {
		return reportUsage(res, opts, e, stdout)
	}
	return report(res, opts, e, stdout)
}
