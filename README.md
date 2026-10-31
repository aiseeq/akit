# akit

Console tools for coding agents, one toolkit in the unix way: each tool does one job and answers the agent briefly.

Built for Claude Code and codex agents on Linux; some tools read files of the author's own agent
setup under `~/.claude` (noted per tool). MIT license.

Layout: library packages at the root (`edit`, `transcript`, …), one command per tool in `cmd/<tool>`.
Install: `graft install` — builds `cmd/...` into `bin/` and installs into `~/bin` from committed code only, prints each tool's revision.

## shellcmd

Finds program calls in shell scripts, including nested substitutions, wrapper commands, and scripts
passed to `bash -c` or through a shell heredoc. It returns each program name, arguments, source line,
raw command text, and wrappers that led to the call.

```go
import "github.com/aiseeq/akit/shellcmd"

calls, err := shellcmd.Calls(`timeout 5 bash -c 'rg pattern file'`)
```

`Calls` returns every program call; `Find` filters the result by program name. `Call.Offset` is the
byte offset of the program word in the command, nested scripts included (`-1` for a program taken
from a variable or a substitution, or written inside `$'…'`); `Call.Redirs` lists redirections with heredoc bodies and their byte bounds; `Call.Loop`
marks calls inside a `while`, `until` or `for` body; `Call.Background` marks calls in a background job.
Scripts are expanded from `bash -c` (also under an unknown program), a shell heredoc read as stdin, and
the remote command of `ssh` (`Via` names the way in). A shell heredoc is not treated as a script when
the shell has `-c` or a script file. `Calls` and `Find` return calls found outside a malformed nested
script with a `NestedError` that identifies the script and unwraps its parse error.

## busy

Is the machine busy, in one line, by the rule graft uses to queue heavy tasks
(`akit/pressure`): tasks ready to run now (`procs_running`, one reading) at or above the machine's
threads, disk IO pressure at or above 9.6% over 5 seconds, or memory pressure (some avg10) above
10% while swapping above 1024 KB/s over a second; swapping alone is not busy. Disk pressure comes
from the disk state source at `$XDG_RUNTIME_DIR/io-state.sock`; if unavailable, busy uses the change
in full total from `/proc/pressure/io` over the swap window. Use `--io-full` to change the threshold
(100 disables disk checks) and `--kind K` to request a profile kind. Use busy instead of `cat
/proc/loadavg` to decide whether to start heavy work now: the 1-minute load lags. Exit 0 free, 1 busy,
2 usage or reading error. `pressure.ReadLoadAvg` reads the system load average for watching long
overload. `busy --quiet 2h --why "call"` keeps the machine busy until then, whatever it measures; the
busy line and graft's wait name the session that set it and why. `--ignore-quiet` judges a
measurement taken inside quiet mode by the readings alone; `--quiet-off` ends it.

### Disk state source

The optional disk state source listens on the Unix socket `$XDG_RUNTIME_DIR/io-state.sock`. Each
connection carries one JSON request line and returns one JSON response line. The request is
`{"v":1,"q":"now","kind":"…"}`; `kind` is omitted when empty. Without `XDG_RUNTIME_DIR` there is no
source.

The client reads `v`, `error`, `at` (RFC 3339 with nanoseconds), `1s`, `5s`, `30s`, `nproc`, `open`, and `profile`. Each window
contains `steps`, `io_full_ms`, `stall_ms`, `write_bps`, `read_bps`, `w_await_ms`, `inflight_max`,
`cpu_some_ms`, and `runnable_max`. `open` contains `open` and `ms`. `profile` is passed through
byte for byte without interpretation. Extra response fields are ignored.

The client allows 200 ms for connection and response. A response with a version other than 1, a
nonempty `error`, an invalid JSON line, or an `at` timestamp more than two seconds from the client's
current time means the source failed. When the source is unavailable or fails, busy falls back to the
change in `full total` from `/proc/pressure/io` over the sampling interval.

    busy                        # busy: runnable 31 is at or above 22; runnable 31 (busy at 22, threads 22); …
    busy --runnable 16          # busy from 16 runnable tasks instead of the thread count

## claude-rules-export

Claude Code rules and skills exported for colleagues and checked for private data: the chosen
skills, the chosen sections of the global CLAUDE.md and the workspace rules are copied into OUT_DIR
(cleared but for `.git`), literal replacements are made, listed lines dropped, then the export is
searched for private data patterns. What to export and what counts as private is data in the
`--rules` folder (`manifest/`, `README.template.md`), not code. Exit 0 clean, 1 private data or a
missing skill, 2 usage/IO error.

    claude-rules-export ~/work/claude-rules

## page-shot

Frames of an HTML page in headless Chrome before publishing: windows 1440×900, 1536×860 at scale
1.25, 1366×768, 390×844 and the full height. On every window it checks for a horizontal scroll and
for labels drawn over each other. Exit 0 frames clean, 1 shooting failed, 2 frames taken but a
layout fault found (window and culprits in stderr).

    page-shot page.html                 # frames into page-shots/ next to the page
    page-shot --at 1s,4s page.html      # an animation: frames at several moments
    page-shot --element '#noticed article.note' --out shots page.html
                                        # each matching element whole, 1536×860 at 1.25 only:
                                        # <id>.png (el-<n>.png without an id), paths in stdout

## prior

Has this come up before, and what do we already have: one search over the decision log, the mechanism
cards, the nightly review's probes and queues, its reports and the user's replies, before asking the
user or building a new mechanism. Words are case-insensitive regular expressions, any of them matches.
Reads the author's setup under `~/.claude`; the nightly review's programs (`inventory` to refresh
stale cards, `daydigest` to append replies) are called by path in `~/.claude/improve/bin`.

    prior компак compact 'окн[оа] сжатия'

## sub

Exact-text replace with an expected match count, all or nothing: if any edit finds a different
number of matches, no file changes and the answer names the mismatch (counts, line numbers, or a
near-miss that differs only in indentation). Exit 0 written, 1 mismatch, 2 usage/IO error.

    sub OLD NEW FILE... [-n N]         # exactly N matches across all FILEs (default 1)
    sub [--dry-run] <<'EOF'            # many edits, files and lines in one call
    path/to/file.go
    <<<<<<< SEARCH
    old text
    =======
    new text
    >>>>>>> REPLACE
    EOF

    sub path/to/file.go <<'EOF'        # every block edits this file, no path line needed

`<<<<<<< SEARCH 3` expects 3 matches. Blocks apply in order; a path line sets the file for the
blocks after it. With a FILE argument a path line may only repeat that file (`./a.go` for
`a.go` is fine), another path is an error. `sub -h` prints the full format.

## tk

Repository task files live in `docs/tasks/`; closed tickets can be moved to `docs/tasks-archive/`
after an independent review.

    tk new fix-login --prio HIGH --title 'Fix login' --by agent
    tk ls                              # open tickets, oldest first
    tk start fix-login --by agent
    tk wait fix-login --until '2026-10-28 09:00' --by agent
    tk review fix-login --by reviewer
    tk done fix-login --check 'graft gate' --by reviewer
    tk archive                         # closed tickets older than 14 days
    tk archive --root DIR              # archive tasks in DIR and its first-level repositories

`tk new` starts a ticket with `Что сделать`, `Приёмка` and `Журнал` sections. A `## Вход` section
in `--text` (context: where the request came from, facts behind it) goes before `Что сделать`;
without it the ticket has none.

Ticket IDs accept the slug, the full file name, or the file name without `.md`. Outside a Git
repository, `tk` uses the nearest `docs/tasks/` up to `$HOME`; `new` creates it in the current
directory when none exists. `tk ls --all` includes closed tickets; `--status STATUS` filters by
status, `--by NAME` filters by executor (or opening actor on older tickets), and `--due` lists
waiting tickets whose parsed deadline has passed. Empty acceptance sections are marked `no-check`.
When `.glint.yaml` exists at the task base, `tk done` checks all glint severities; findings block
completion unless `--glint-debt SLUG` links them to an open ticket.
`tk ls --root DIR` lists tasks for DIR and its first-level repositories, skipping linked worktrees;
`--slug PREFIX` filters by a case-insensitive slug prefix, and `--json` emits the filtered list with
completion details. `tk archive --root DIR` archives each repository's tasks to its own
`docs/tasks-archive/` and prints paths relative to DIR. Completion journal entries record the
checker session when it differs from `--by` (`checker_session` in JSON): a fresh sub-agent runs in its
parent's session, so the session alone does not tell a self-check apart; `tk done` only warns when it matches the executor. `tk done` refuses tickets with an empty `Приёмка` or `Чем проверить` section.
`tk show ID` prints the ticket path and full text with lines over 150 characters wrapped at spaces (the file keeps them whole), or
structured details with `--json`. `tk note ID TEXT` appends a journal entry without changing status.
`tk lint` checks ticket names, statuses, acceptance sections, and independent completion checks;
`--root DIR` scans DIR and its first-level repositories. Without `--by`, actor commands use the
current session name when it can be read from the process cgroup. `tk -h` lists commands and flags.

## tl

Session transcripts for an agent, briefly: a time/role window of one session or a search across
all projects. Reads Claude Code transcripts, their subagents and codex rollouts. Each event line carries a reference
`id:line`; `tl id:line` prints that event in full. Exit 0 events found, 1 none, 2 usage/IO error.

    tl backend --since 12:00               # the conversation of a live session since noon
    tl --all --role user,ask --grep wfor   # what the human said about wfor, last 7 days
    tl --all --raw '"isSidechain":true'    # records marked as subagent events
    tl backend --subagents                 # this session and its subagents
    tl 1a2b3c4d:7759                       # one event in full
    tl --all --since 1d --usage            # tokens and API cost per session, TSV
    tl --all --since 24h --wait 'tk done' --q 95  # 95th percentile wait for output, ms

Roles: `user` typed by the human, `ask` answers in a question dialog, `peer` messages from other
sessions, `agent`, `think` the agent's thinking, `cmd` tool calls, `out` tool output, `attach`
Claude Code attachments (hooks, diagnostics), `note` notices, compaction, briefs and program
prompts. `assistant` and `tool`, as jsonl names them, mean `agent` and `cmd`. `tl -h` prints
all flags.
Large transcript reads wait for machine capacity through graft's heavy queue; smaller ones run at once on two
threads.

`--wait PROG [SUB]` prints the nearest-rank percentile of milliseconds from a shell command call to
its output; calls without output, background Bash calls, and commands that also run unrelated programs
are excluded. The default percentile is 50; `--q` accepts 1–99. Fewer matching events than the
percentile needs (3 for `--q` 50 and below, 20 above) returns exit 3 with the count in stderr and no
stdout. Completed calls are cached per transcript in `$XDG_CACHE_HOME/akit/tl-wait` (or the user cache
directory when `XDG_CACHE_HOME` is unset); changing the transcript or the `tl` binary invalidates its entry.

## user-word

The user's own words from another session's transcript around a fragment: the end of the agent's
reply before it (what the user answered), the line itself and the user's next lines within N
minutes. Lines typed or queued by the human and answers in a question dialog count. Exit 0 found,
1 not found, 2 usage/IO error.

    user-word ~/.claude/projects/-home-x/1a2b3c4d-….jsonl 'да, делай' [MINUTES]
    user-word --search 'да, делай' [--since 2026-10-24] [--project akit]

## wfor

Wait for conditions in one call, with a timeout, instead of a `while … sleep` loop. The answer
says what was met and when; on timeout, what is still missing and what it looks like now (file
absent, last line of the log, last exit and output of the command). Exit 0 done, 1 not yet,
2 usage error or a failed check.

The default timeout is 4m30s, under the 5-minute life of a sub-agent's prompt cache: a longer wait
is the same call again (the answer says how; with `--new` it gives `--after N` so lines written
between the calls still count).

    wfor line '^(ok|FAIL)' test.log                # a line in a log (FILE may not exist yet)
    wfor exit 31337 31338                          # processes ended (zombies count as ended)
    wfor file result.tsv ok 'worker status | grep -q done' --any
    wfor idle                                      # machine calm by graft's rule

Conditions: `file PATH`, `gone PATH`, `line RE FILE`, `exit PID...`, `time T`, `ok CMD`, `idle`.
A heavy command after the wait goes as `graft exec --heavy -- CMD`, not `wfor idle && CMD`: graft
waits the same way and keeps the command in its queue. `wfor -h` prints all flags.

## webtext

Text of a web page WebFetch could not open (403, 429, a bot check). Direct GET under the honest
name `akit-webtext/<version>`; on a refusal, the latest Wayback snapshot with status 200. HTML
and PDF (through `pdftotext`) become text in a file; the answer is one line with the source and
the path, or the steps and what to do next. No disguise: no browser user agent, no fingerprint
faking, no proxies. Internal hosts and URLs with tokens never go to Wayback.

    webtext https://example.com/article            # text → ~/.cache/akit/webtext/<hash>.txt
    webtext --json --timeout 45s URL               # for the WebFetch hook
    webtext --no-direct URL                        # the site already answered 429: archive only

Exit 0 text saved, 1 the page could not be opened, 2 usage or write error. `webtext -h` prints all
flags.
