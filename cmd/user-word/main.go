// user-word — решение пользователя из транскрипта другой сессии: получатель читает оригинал вместо пересказа.
// Печатает конец ответа агента перед репликой (на что пользователь отвечал), саму реплику и следующие реплики
// пользователя в той же сессии за N минут: уточнения вроде «но один сегодняшний деплой» часто идут отдельным
// сообщением. Реплика — набранная человеком, поставленная в очередь или ответ в окне вопроса.
//
// Коды выхода: 0 — реплика нашлась, 1 — нет, 2 — ошибка вызова или чтения.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"time"
)

const usage = `user-word — the user's own words from another session's transcript, around a fragment.

Usage:
  user-word TRANSCRIPT.jsonl 'FRAGMENT' [MINUTES]
  user-word --search 'FRAGMENT' [--since YYYY-MM-DD] [--project S]
  user-word --brief FILE [--until 'YYYY-MM-DD HH:MM']

The first form prints the end of the agent's reply before the user's line with
FRAGMENT (what the user answered), the line itself and the user's next lines
within MINUTES (default 30). Several lines with FRAGMENT: the last one, since
the sender refers to the fresh decision; for an earlier one, a longer fragment.

--search looks through all session transcripts in ~/.claude/projects: up to 5
hits in full, more — a list by time. --project keeps project folders whose path
contains S (a project path like ~/git/akit works too).

--brief finds user words cited in a brief and prints their transcript context.
Without --until, the brief file's modification time is used.

FRAGMENT matches ignoring case, ё/е and line breaks or runs of spaces.
User lines: typed or queued by the human, and answers in a question dialog.
Question dialog searches match only the human's answer; the question is shown
as context. The word right after a brief marker clock is its zone: CET, CEST,
UTC, GMT, Z, UTC+H (any case, parentheses allowed); another word there is
reported as not found.
Exit: 0 found, 1 not found, 2 usage/IO error.
`

const (
	exitNone  = 1
	exitUsage = 2
)

type options struct {
	search  bool
	path    string
	frag    string
	mins    int
	since   string // YYYY-MM-DD, местная дата
	project string
	brief   bool
	until   string
}

var errHelp = errors.New("help")

func main() {
	opts, err := parseArgs(os.Args[1:])
	if errors.Is(err, errHelp) {
		fmt.Print(usage)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "user-word: %v\n(user-word -h for usage)\n", err)
		os.Exit(exitUsage)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "user-word: %v\n", err)
		os.Exit(exitUsage)
	}
	os.Exit(run(opts, env{home: home, loc: time.Local}, os.Stdout, os.Stderr))
}

// env — то, что user-word берёт из окружения; в тестах подменяется.
type env struct {
	home string
	loc  *time.Location
}

var dateArg = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

func parseArgs(args []string) (options, error) {
	opts := options{mins: 30}
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return opts, errHelp
		}
	}
	if len(args) > 0 && args[0] == "--brief" {
		return parseBriefArgs(args)
	}
	if len(args) > 0 && args[0] == "--search" {
		return parseSearchArgs(args)
	}
	return parseTranscriptArgs(args)
}

func parseBriefArgs(args []string) (options, error) {
	opts := options{mins: 30, brief: true}
	if len(args) < 2 || len(args) > 4 {
		return opts, errors.New("--brief needs FILE and optional --until time")
	}
	opts.path = args[1]
	if len(args) == 2 {
		return opts, nil
	}
	if len(args) != 4 || args[2] != "--until" {
		return opts, errors.New("--brief accepts only --until 'YYYY-MM-DD HH:MM'")
	}
	if _, err := time.ParseInLocation("2006-01-02 15:04", args[3], time.Local); err != nil {
		return opts, fmt.Errorf("--until needs YYYY-MM-DD HH:MM, got %q", args[3])
	}
	opts.until = args[3]
	return opts, nil
}

func parseSearchArgs(args []string) (options, error) {
	opts := options{mins: 30, search: true}
	if len(args) < 2 || args[1] == "" {
		return opts, errors.New("--search needs a fragment")
	}
	opts.frag = args[1]
	for i := 2; i < len(args); i += 2 {
		if i+1 >= len(args) {
			return opts, fmt.Errorf("%s needs a value", args[i])
		}
		value := args[i+1]
		switch args[i] {
		case "--since":
			if !dateArg.MatchString(value) {
				return opts, fmt.Errorf("--since needs a date YYYY-MM-DD, got %q", value)
			}
			opts.since = value
		case "--project":
			opts.project = value
		default:
			return opts, fmt.Errorf("unknown flag %s", args[i])
		}
	}
	return opts, nil
}

func parseTranscriptArgs(args []string) (options, error) {
	opts := options{mins: 30}
	if len(args) < 2 || len(args) > 3 || args[1] == "" {
		return opts, errors.New("need TRANSCRIPT.jsonl and FRAGMENT")
	}
	opts.path, opts.frag = args[0], args[1]
	if len(args) == 3 {
		n, err := strconv.Atoi(args[2])
		if err != nil || n < 0 {
			return opts, fmt.Errorf("MINUTES needs a number >= 0, got %q", args[2])
		}
		opts.mins = n
	}
	return opts, nil
}

func run(opts options, e env, stdout, stderr io.Writer) int {
	if opts.brief {
		return runBrief(opts, e, stdout, stderr)
	}
	if opts.search {
		return search(opts, e, stdout, stderr)
	}
	if _, err := os.Stat(opts.path); err != nil {
		fmt.Fprintf(stderr, "user-word: transcript %s: %v\n", opts.path, err)
		return exitUsage
	}
	lines, err := readLines(opts.path, nil)
	if err != nil {
		fmt.Fprintf(stderr, "user-word: %v\n", err)
		return exitUsage
	}
	at := lastWith(lines, opts.frag)
	if at < 0 {
		writeWarnings(stderr, lines)
		fmt.Fprintf(stderr, "user-word: no user line with %q in %s\nin other sessions: user-word --search %q\n", opts.frag, opts.path, opts.frag)
		return exitNone
	}
	show(stdout, lines, at, opts.mins, e.loc)
	return 0
}
