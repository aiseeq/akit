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

The first form prints the end of the agent's reply before the user's line with
FRAGMENT (what the user answered), the line itself and the user's next lines
within MINUTES (default 30). Several lines with FRAGMENT: the last one, since
the sender refers to the fresh decision; for an earlier one, a longer fragment.

--search looks through all session transcripts in ~/.claude/projects: up to 5
hits in full, more — a list by time. --project keeps project folders whose path
contains S (a project path like ~/git/akit works too).

FRAGMENT matches ignoring case, ё/е and line breaks or runs of spaces.
User lines: typed or queued by the human, and answers in a question dialog.
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
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return opts, errHelp
		}
	}
	if len(args) > 0 && args[0] == "--search" {
		opts.search = true
		if len(args) < 2 {
			return opts, errors.New("--search needs a fragment")
		}
		opts.frag = args[1]
		rest := args[2:]
		for i := 0; i < len(rest); i += 2 {
			if i+1 >= len(rest) {
				return opts, fmt.Errorf("%s needs a value", rest[i])
			}
			switch rest[i] {
			case "--since":
				if !dateArg.MatchString(rest[i+1]) {
					return opts, fmt.Errorf("--since needs a date YYYY-MM-DD, got %q", rest[i+1])
				}
				opts.since = rest[i+1]
			case "--project":
				opts.project = rest[i+1]
			default:
				return opts, fmt.Errorf("unknown flag %s", rest[i])
			}
		}
	} else {
		if len(args) < 2 || len(args) > 3 {
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
	}
	if opts.frag == "" {
		return opts, errors.New("empty fragment")
	}
	return opts, nil
}

func run(opts options, e env, stdout, stderr io.Writer) int {
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
		fmt.Fprintf(stderr, "user-word: no user line with %q in %s\nin other sessions: user-word --search %q\n", opts.frag, opts.path, opts.frag)
		return exitNone
	}
	show(stdout, lines, at, opts.mins, e.loc)
	return 0
}
