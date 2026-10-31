// claude-rules-export — выгрузка правил и скиллов Claude Code коллегам: выбранные скиллы, разделы общего CLAUDE.md
// и правила рабочего пространства копируются в OUT_DIR, в тексте делаются замены и вычёркиваются строки, затем
// выгрузка проверяется на личные данные. Что выгружать и что считать личным — данные выгрузки в папке --rules,
// не код: в akit не попадают ни имена, ни политика.
//
// Коды выхода: 0 — выгрузка чистая, 1 — в выгрузке личные данные или нет скилла из списка, 2 — ошибка вызова
// или чтения.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const usage = `claude-rules-export — Claude Code rules and skills exported for colleagues, checked for private data.

Usage:
  claude-rules-export [--src-home DIR] [--rules DIR] OUT_DIR

OUT_DIR is cleared (all but .git) and filled from the source home (default $HOME):
skills/<name> for each line of manifest/skills.txt, work/CLAUDE.md, global/CLAUDE.md
with the sections of ~/.claude/CLAUDE.md named in manifest/global-sections.txt, and
README.md from README.template.md. Then in every file but README.md the literal
replacements of manifest/replace.tsv (FROM<TAB>TO) are made and lines containing a
line of manifest/drop-lines.txt are dropped. Last, manifest/pii.tsv
(KIND<TAB>REGEXP<TAB>ALLOWED REGEXP) is searched for: each hit not fully matching
ALLOWED is printed as file:line: KIND.

The export is built and checked in a folder of its own first, then put into OUT_DIR. In
OUT_DIR only the previous run's own entries are removed, and only while unchanged: each
file's and folder's path, mode (setuid, setgid and sticky too), owner and group, extended
attributes (ACLs too), device, inode and birth time, each file's size and sha256 and each
link's text must
match the list that run kept in $XDG_STATE_HOME/claude-rules-export (default
~/.local/state), keyed by the OUT_DIR path; the build lies there too. Nothing new gets its
name in OUT_DIR before it is logged next to that list: a file is written unnamed, a folder
or link is made under a random name logged first. Folders are removed only when empty;
.git at the top is never touched.
Anything else (an edited or foreign file, a folder shown there by a mount, an OUT_DIR
without a list that holds more than .git) is refused before anything is removed, so no
other path to the same folder (symlink, bind mount, overlay, /proc) can erase it. A run cut
short is finished by the next one. Not told apart: a copy that keeps all of these (a file
system snapshot restored in place). An OUT_DIR that is or holds what is read is refused
early, after symlinks are resolved (lying inside a read folder such as ~/work is fine);
an empty path is refused. A skill folder that is a link is exported as its content; skills named .git
and paths differing only in case are refused.

--rules is the folder with manifest/ and README.template.md
(default ~/.claude/tools/claude-rules-export).
Exit: 0 clean, 1 private data or a missing skill, 2 usage/IO error.
`

const (
	exitDirty = 1
	exitUsage = 2
)

type options struct {
	srcHome, rules, out string
}

var errHelp = errors.New("help")

// errDirty — выгрузка сделана, но не годится: подробности уже напечатаны.
var errDirty = errors.New("export is not clean")

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-rules-export: %v\n", err)
		os.Exit(exitUsage)
	}
	opts, err := parseArgs(os.Args[1:], home)
	if errors.Is(err, errHelp) {
		fmt.Print(usage)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-rules-export: %v\n(claude-rules-export -h for usage)\n", err)
		os.Exit(exitUsage)
	}
	os.Exit(run(opts, os.Stdout, os.Stderr))
}

func parseArgs(args []string, home string) (options, error) {
	opts := options{srcHome: home, rules: filepath.Join(home, ".claude", "tools", "claude-rules-export")}
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-h", "--help":
			return opts, errHelp
		case "--src-home", "--rules":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("%s needs a value", a)
			}
			i++
			if a == "--src-home" {
				opts.srcHome = args[i]
			} else {
				opts.rules = args[i]
			}
		default:
			if len(a) > 1 && a[0] == '-' {
				return opts, fmt.Errorf("unknown flag %s", a)
			}
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 {
		return opts, errors.New("need one OUT_DIR")
	}
	// пустой путь — текущая папка: незаданная переменная в вызове очистила бы её
	if rest[0] == "" || opts.srcHome == "" || opts.rules == "" {
		return opts, errors.New("OUT_DIR, --src-home and --rules must not be empty")
	}
	opts.out = rest[0]
	return opts, nil
}

func run(opts options, stdout, stderr io.Writer) int {
	err := export(opts, stdout)
	switch {
	case errors.Is(err, errDirty):
		return exitDirty
	case errors.As(err, new(missingSkill)):
		fmt.Fprintf(stderr, "claude-rules-export: %v\n", err)
		return exitDirty
	case err != nil:
		fmt.Fprintf(stderr, "claude-rules-export: %v\n", err)
		return exitUsage
	}
	return 0
}
