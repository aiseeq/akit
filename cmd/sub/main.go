// sub — правка файлов точным текстом с проверкой числа совпадений: либо у каждой правки ровно ожидаемое число
// совпадений и все файлы записаны, либо не меняется ни один файл, а в ответе сказано, где расхождение. Закрывает
// молчаливый промах `sed -i` и `.replace()`: ненайденный текст там выглядит как успех.
//
// Коды выхода: 0 — записано, 1 — число совпадений не сошлось, ничего не записано, 2 — ошибка вызова или чтения.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aiseeq/akit/edit"
)

const usage = `sub — exact-text replace with an expected match count; all or nothing.

Usage:
  sub [-n N] [--dry-run] OLD NEW FILE...   replace OLD with NEW; exactly N matches
                                           in all FILEs together (default 1)
  sub [--dry-run] [-F SPEC]                many edits from SPEC file or stdin
  sub [--dry-run] [-F SPEC] FILE           same, every block edits FILE;
                                           no path line needed in the spec

If any edit finds a different number of matches, no file is changed and the
answer says where: expected/found counts and line numbers, or near-misses
that differ only in indentation. Exit: 0 written, 1 mismatch, 2 usage/IO error.

  -n N        expected matches (args mode)
  -F SPEC     read the spec from file SPEC ("-" = stdin)
  --dry-run   check counts, write nothing
  --          end of flags (OLD may start with "-")

`

// exitMismatch — число совпадений не сошлось, exitUsage — ошибка вызова или чтения.
const (
	exitMismatch = 1
	exitUsage    = 2
)

type options struct {
	want   int
	spec   string
	dryRun bool
	args   []string
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if errors.Is(err, errHelp) {
		fmt.Print(usage + edit.SpecHelp + "\n")
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "sub: %v\n(sub -h for usage)\n", err)
		os.Exit(exitUsage)
	}
	os.Exit(run(opts, os.Stdin, os.Stdout, os.Stderr))
}

var errHelp = errors.New("help")

// parseArgs разбирает флаги в любом месте до «--»: агенты ставят -n и после файлов.
func parseArgs(args []string) (options, error) {
	opts := options{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			opts.args = append(opts.args, args[i+1:]...)
			break
		}
		name, val, hasVal := strings.Cut(a, "=")
		switch name {
		case "-h", "--help":
			return opts, errHelp
		case "--dry-run":
			opts.dryRun = true
			continue
		case "-n", "-F":
			if !hasVal {
				if i+1 >= len(args) {
					return opts, fmt.Errorf("%s needs a value", name)
				}
				i++
				val = args[i]
			}
			if name == "-F" {
				opts.spec = val
				continue
			}
			n, err := edit.ParseWant(val)
			if err != nil {
				return opts, err
			}
			opts.want = n
			continue
		}
		if looksLikeFlag(a) {
			return opts, fmt.Errorf("unknown flag %s (use -- before OLD that starts with -)", a)
		}
		opts.args = append(opts.args, a)
	}
	return opts, nil
}

// looksLikeFlag — аргумент похож на флаг: -x, --word, --word=… Текст вроде «- пункт списка» или «-1» — OLD или
// NEW: правка markdown-списка частая, и требовать для неё «--» — лишний заход.
func looksLikeFlag(a string) bool {
	rest := strings.TrimLeft(a, "-")
	if dashes := len(a) - len(rest); dashes < 1 || dashes > 2 {
		return false
	}
	name, _, _ := strings.Cut(rest, "=")
	if name == "" {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-') {
			return false
		}
	}
	return true
}

func run(opts options, stdin io.Reader, stdout, stderr io.Writer) int {
	edits, err := collect(opts, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "sub: %v\n(sub -h for usage)\n", err)
		return exitUsage
	}
	plan, err := edit.Build(edits)
	if err != nil {
		fmt.Fprintf(stderr, "sub: %v; nothing changed\n", err)
		return exitUsage
	}
	if !plan.OK() {
		bad := plan.Mismatches()
		fmt.Fprintf(stderr, "sub: nothing changed: %d of %d edits mismatched\n", len(bad), len(plan.Results))
		for _, m := range bad {
			fmt.Fprintf(stderr, "  %s\n", m)
		}
		return exitMismatch
	}
	if opts.dryRun {
		fmt.Fprintf(stdout, "sub: dry run, would make %s\n", plan.Summary())
		return 0
	}
	if _, err := plan.Write(); err != nil {
		fmt.Fprintf(stderr, "sub: %v\n", err)
		return exitUsage
	}
	fmt.Fprintf(stdout, "sub: %s\n", plan.Summary())
	return 0
}

// collect собирает правки из аргументов (OLD NEW FILE...) или из задания: файла -F или stdin. Один аргумент — файл,
// который правят все блоки задания.
func collect(opts options, stdin io.Reader) ([]edit.Edit, error) {
	var file string
	if len(opts.args) == 1 {
		file = opts.args[0]
		if file == "" {
			return nil, errors.New("FILE is empty")
		}
	} else if len(opts.args) > 0 {
		if opts.spec != "" {
			return nil, errors.New("-F and OLD NEW FILE... together; pick one")
		}
		if len(opts.args) == 2 {
			return nil, fmt.Errorf("need OLD NEW FILE..., got %d arguments", len(opts.args))
		}
		if opts.args[0] == "" {
			return nil, errors.New("OLD is empty")
		}
		want := opts.want
		if want == 0 {
			want = 1
		}
		return []edit.Edit{{Files: opts.args[2:], Old: opts.args[0], New: opts.args[1], Want: want, Origin: "OLD"}}, nil
	}
	if opts.want != 0 {
		return nil, errors.New("-n is for args mode; in a spec write \"<<<<<<< SEARCH N\"")
	}
	r := stdin
	if opts.spec != "" && opts.spec != "-" {
		f, err := os.Open(opts.spec)
		if err != nil {
			return nil, fmt.Errorf("open spec: %w", err)
		}
		defer f.Close()
		r = f
	} else if f, ok := stdin.(*os.File); ok && opts.spec == "" {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			if file != "" {
				return nil, errors.New("FILE given but no spec on stdin (for one edit: sub OLD NEW FILE)")
			}
			return nil, errors.New("no arguments and no spec on stdin")
		}
	}
	if file != "" {
		return edit.ParseSpecFor(r, file)
	}
	return edit.ParseSpec(r)
}
