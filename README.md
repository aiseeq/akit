# akit

Console tools for coding agents, one toolkit in the unix way: each tool does one job and answers the agent briefly.

Built for Claude Code and codex agents on Linux; some tools read files of the author's own agent
setup under `~/.claude` (noted per tool). MIT license.

Layout: library packages at the root (`edit`, `transcript`, …), one command per tool in `cmd/<tool>`.
Install: `graft install` — builds `cmd/...` into `bin/` and installs into `~/bin` from committed code only, prints each tool's revision.

`codexlimit` provides `Read` for weekly account limits, `Consume` for reset credits, and `ReadApproved` for reset approvals.

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
(`akit/pressure`): tasks ready to run now (`procs_running`, one reading) or the 1-minute load average
at or above the machine's threads, disk IO pressure at or above 9.6% over 5 seconds or a disk freeze in
progress reported by the disk state source, dirty and writeback memory (`Dirty`+`Writeback` in
`/proc/meminfo`, one reading) at or above the machine's background write-out threshold
(`vm.dirty_background_bytes`, or `vm.dirty_background_ratio` of free and file memory when the bytes
are 0; the kernel also subtracts its page reserve, which `/proc/meminfo` does not show, so this
threshold is slightly higher) —
a write-out to disk in progress, which a new heavy task would only lengthen — or memory pressure (some avg10) above
10% while swapping above 1024 KB/s over a second; swapping alone is not busy. On a fresh check, when the disk
state source responds and memory pressure is at or below its limit, swapping is not measured because it cannot
affect the verdict. Disk pressure comes
from the disk state source at `$XDG_RUNTIME_DIR/io-state.sock`; if unavailable, busy uses the change
in full total from `/proc/pressure/io` over the swap window. Waiting under an `io.max` cap is a task
waiting for its own limit, not for the disk: when the source judges that only cgroups at their caps
waited in the 5-second window, or in the freeze in progress, that pressure or freeze is not busy, and
the busy line says so. Without the source, or with a source that gives no such judgement, caps are
not told apart and the line names the missing judgement. Use `--io-full` to change the threshold
(100 disables disk checks, the freeze too) and `--kind K` to request a profile kind. Your own thresholds go in
`$XDG_CONFIG_HOME/akit/pressure.yaml` as `key: value` lines (`runnable`, `max_io_full`, `max_mem_some`,
`max_swap_kbs`, `swap_window`, `max_dirty_mb`), shared with graft; flags override the file, and a value that is not a
finite positive number (or a positive duration) is an error. Use busy instead of reading `/proc/loadavg`
by hand to decide whether to start heavy work now: it applies both CPU numbers and the rest of the rule
in one call. Exit 0 free, 1 busy,
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
`cpu_some_ms`, and `runnable_max`, and optionally `capped`: `{"capped":…,"outside_ms":…,"capped_ms":…}`,
whether only cgroups at their `io.max` caps waited in that window, the largest wait of a cgroup outside
caps and the wait of leaves at caps, in ms. `open` contains `open`, `ms`, and optionally `capped`,
the same judgement for the freeze in progress; a source that sends `capped` in the 5-second window
must send it in an open freeze too. `profile` is passed through byte for byte without interpretation.
Extra response fields are ignored.

The client allows 200 ms for connection and response. A response with a version other than 1, a
nonempty `error`, an invalid JSON line, `capped` in only one of the 5-second window and an open
freeze, or an `at` timestamp more than two seconds from the client's current time means the source
failed. When the source is unavailable or fails, busy falls back to the
change in `full total` from `/proc/pressure/io` over the sampling interval.

    busy                        # busy: runnable 31 is at or above 22; runnable 31 (busy at 22, threads 22); …
    busy --runnable 16          # busy from 16 runnable tasks instead of the thread count

## uptake

Measures calls to akit tools and their earlier equivalents in Claude and Codex transcripts over a time window. It writes count, example, outcome, repeat, pairing, empty-search, and edit-cost TSV tables plus a Markdown report. The default window covers the previous seven days; `--since` and `--until` accept RFC3339 timestamps, `--seed` fixes example selection, and `--out` selects the output directory.

## claude-rules-export

Claude Code rules and skills exported for colleagues and checked for private data: the chosen
skills, the chosen sections of the global CLAUDE.md and the workspace rules are built in a folder
next to the export list (below), literal replacements are made, listed lines dropped, the build is searched for private data
patterns and then put into OUT_DIR. In OUT_DIR only the previous run's own entries are removed, and only
while unchanged: path, mode (setuid, setgid and sticky too), owner and group, extended attributes (ACLs and SELinux labels too, links' as well), device, inode and
birth time of each file and folder, size and sha256 of each file and the text of each link must match the list
that run kept in `$XDG_STATE_HOME/claude-rules-export` (default `~/.local/state`), keyed by the OUT_DIR path;
the build lies there too. Nothing new gets its name in OUT_DIR before it is logged next to that list: a file is
written unnamed (`O_TMPFILE`) and linked by its descriptor, a folder or link is made under a random name logged
first and renamed without replacing. An OUT_DIR on a file system without unnamed files or birth times is refused. Folders are removed only when empty and `.git` at the top is never touched. Anything else — an edited
or foreign file, a folder shown there by a mount, an OUT_DIR without a list holding more than `.git` — is
refused before anything is removed, so no other path to the same folder (symlink, bind mount, overlay,
`/proc`) can erase it, and a run cut short is finished by the next one. A copy that keeps all of these (a file system
snapshot restored in place) is not told apart. An OUT_DIR that is or holds what is read is
refused early, after symlinks are resolved (lying inside a read folder, as `~/work/claude-rules` does, is fine); an empty path is
refused; a skill folder that is a link is exported as its content; skills named `.git` and paths differing
only in case are refused. What to export and what counts as private is data in the
`--rules` folder (`manifest/`, `README.template.md`), not code. Exit 0 clean, 1 private data or a
missing skill, 2 usage/IO error.

    claude-rules-export ~/work/claude-rules

## page-shot

Frames of an HTML page in headless Chrome before publishing: windows 1440×900, 1536×860 at scale
1.25, 1366×768, 390×844 and the full height. On every window it checks for a horizontal scroll and
for labels drawn over each other, and for small icons (svg, img, a symbol glyph, an `ic`/`icon` class, an empty
decorated box) whose center is more than 2 px off the first text line next to them in the same row; text
farther than 3em from the icon is not its line (table cells, chart columns). Exit 0 frames clean, 1 shooting failed, 2 frames taken but a
layout fault found (window and culprits in stderr).

    page-shot page.html                 # frames into page-shots/ next to the page
    page-shot --at 1s,4s page.html      # an animation: frames at several moments
    page-shot --element '#noticed article.note' --out shots page.html
                                        # each matching element whole, 1536×860 at 1.25 only:
                                        # <id>.png (el-<n>.png without an id), paths in stdout

## prior

Has this come up before, and what do we already have: one search over the decision log, the mechanism
cards, the nightly review's probes and queues, its reports and the user's replies, before asking the
user or building a new mechanism. Rules the review removed are found in its archive with the reason and the
removed text, so they can be brought back. Words are case-insensitive regular expressions, any of them matches.
Reads the author's setup under `~/.claude`; the nightly review's programs (`inventory` to refresh
stale cards, `daydigest` to append replies) are called by path in `~/.claude/improve/bin`. Reports show at most 3 matches per day, then fill remaining slots in freshness order.

    prior компак compact 'окн[оа] сжатия'

## sub

Exact-text replace with an expected match count, all or nothing: if any edit finds a different
number of matches, no file changes and the answer names the mismatch (counts, line numbers, or a
near-miss that differs only in indentation). Replacements preserve extended attributes (including ACLs)
and ownership; files with hard links are refused because replacing them would break those links. Exit 0
written, 1 mismatch, 2 usage/IO error.

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

## mut

Runs Go tests against each exact-text mutation in isolation, restores the source file, and reports whether the mutant was killed or survived. A mutant that hits the memory ceiling of `graft exec --heavy` counts as killed; a test red without the mutant, a build error, no test run or every test skipped is an error, and a survivor names the tests skipped beside it.

    mut OLD NEW FILE -- <go test arguments>
    mut [-F SPEC] -- <go test arguments>

## tk

`tk new` and `tk set` can link tickets with repeatable `--blocks` flags; `tk set --unblocks` removes a link. A ticket that blocks other work inherits at least its priority, so work hidden by queue gates can rise with the ticket it blocks. Links can point across projects under `$HOME/git` and `$HOME/work` using `git/PROJECT/SLUG` or `work/PROJECT/SLUG`.

Repository task files live in `docs/tasks/`; closed tickets can be moved to `docs/tasks-archive/`
after an independent review.

    tk new fix-login --prio HIGH --title 'Fix login' --by agent
    tk plan fix-login --add 'tests: go test ./...'
    tk plan fix-login --add 'accept: ./accept.sh' --required   # tk skip refuses it
    tk check fix-login                  # run plan checks without closing
    tk skip fix-login tests --why 'covered by another check'
    tk ls                              # open tickets, oldest first
    tk start fix-login --by agent
    tk release fix-login --why 'The assigned session ended' --by any-worker
    tk set fix-login --title 'Repair login' --back requester
    tk wait fix-login --until '2026-10-28 09:00' --by agent
    tk review fix-login --by reviewer
    tk done fix-login --check 'graft gate' --by reviewer
    tk archive                         # closed tickets older than 14 days
    tk archive --root DIR              # archive tasks in DIR and its first-level repositories

In a linked worktree of a repository whose main tree ignores `docs/tasks/`, `tk` uses the main tree's
`docs/tasks/`, so every branch sees the same tickets; plan checks and glint still run in the current
worktree. Where `docs/tasks/` is not ignored, each worktree keeps its own tickets, as do worktrees
of a bare repository or of a submodule. The whole directory is meant to be ignored: a negated
pattern that lets some ticket files back into git is not honoured.

`docs/tasks` may also be a symlink into a separate task store (ignore it as `/docs/tasks`, without
the trailing slash: git treats a symlink as a file). Worktrees then use the main tree's link, and
`tk archive` moves closed tickets to `docs/tasks/archive/` inside the store instead of
`docs/tasks-archive/` in the repository.

A project listed in `~/.config/queue/frozen` (one per line: absolute or `~/` path, who, date, why, tab-separated; `#` starts a comment) is worked on only by its running master, the session named `<folder>-master`: for any other session `tk next` gives no work and `tk start` refuses with the reason from the file. `tk new`, `tk ls` and `tk note` work as usual, so tickets accumulate. The identity is the session itself, not `--by`.

Queue gates close only the start of new work: a ticket the agent already took and a wait that came due stay visible to `tk next` under a closed pair and under an unreadable gate file, after CRIT and HIGH and ahead of new work. Within a priority group, an open local blocker comes before its blocked ticket; tickets blocked by a foreign project or an external transfer reference follow unblocked tickets in that group.
`tk done` ends with a `следующий:` line when the queue has a ticket: the one `tk next` would give the
ticket's executor. If that executor maps to another project in `~/.config/cl/names.tsv`, or its
`<name>-master` folder (lower case) is found beside or inside the current root, the hint uses that project's queue; `--doer` names the executor first.

In projects under `$HOME/git` or `$HOME/work`, `tk next` can read an executable `.tk-next-source`
in the projects root. It receives the project directory name and prints a JSON array of tickets with
`key`, `title`, `status`, `prio` and `url`, and optionally `"started": true` for work already begun
there, which the queue gates treat as a ticket in progress; it has a five second limit, and a failed source leaves
local tickets available with a warning. `tk external --json` prints that whole list for an outside
reader, without gates and local tickets: `[]` when the project has no source, a nonzero code and
nothing on stdout when the source fails; `can_take` on each record says whether `tk next` would show
it to the project's master under the current gate and frozen list. `tk new --to NAME` resolves names in either projects root
and reports an error if both contain that name. When creating tickets in an eligible repository,
`tk new` links `docs/tasks` to the shared `tasks/PROJECT` store and records the ignore rule in the
repository's exclude file when needed.

`tk new` and `tk start` name the next step on stderr; stdout stays what scripts read.

Use `tk new --journal` or `tk set ID --journal` to mark a journal ticket; it stays out of `tk next`
until a wait deadline is due. Queue gates use `open_by_root` in `gate.json`, keyed by absolute project
roots. `tk transfer ID [--parent KEY]` passes the project name, ticket filename stem, optional parent
key, and full ticket text to the executable `.tk-transfer` in the parent of the project under
`$HOME/git` or `$HOME/work`. The command prints a JSON object with `key` and `url`. The command must
make repeated transfers with the same stem idempotent; if tk stops after the transfer command ran,
the operator must decide whether to retry.

`tk new` starts a ticket with `Что сделать`, `Приёмка` and `Журнал` sections. The optional
`## План` section lists `- name: command` checks; `tk plan` adds checks, `tk skip` records a reason,
`tk check` runs them, and `tk done` runs every unskipped check before closing. Each check gets 8 minutes
by default (a project gate may wait in the heavy-task queue), `--timeout D` sets another limit for that run. A check added with
`--required` first checks the command with `/bin/bash --norc --noprofile -n -c COMMAND`; a syntax error refuses the add with exit 2 and Bash's diagnostic, without running the command. A required command cannot be skipped: `tk skip` refuses it with exit 2, a skip line written into the
journal by hand does not count, and `tk done` refuses a ticket whose plan no longer has it (the
section removed or renamed included) or runs another command under its name. The journal line
`добавлен обязательный пункт NAME: COMMAND` marks it wherever it stands in the ticket, so editing the
journal headings or line breaks around it does not drop it. The line pins the command: `tk plan --add`
of that name with another command is refused (exit 2), with the same command it brings a removed item
back. `tk plan ID --repin 'NAME: COMMAND' --why TEXT` can replace a required command from a different current session. The current executor, anyone recorded as an executor in an earlier `в работе` or `на проверке` entry, and every earlier pin or repin author are refused, regardless of `--by`. The journal records the session, old and new commands, and reason; Bash checks the new command's syntax without running it.
Lines that disagree on the command, or a record of the add that does not stand as a journal line
of its own (glued to the line before by any character, an invisible character in front) — only a hand
edit makes them — leave the item invalid, so `tk done` refuses. Text tk writes into a ticket (note,
`--add`, `--why`, `--check`, `--title`, `--until`) must be one line: no control character but a tab, no
Unicode line or paragraph separator.
Each check runs in
`/bin/bash --norc --noprofile -c` from the repository root, without `BASH_ENV`, `ENV`, `BASH_FUNC_*`, `SHELLOPTS`,
`BASHOPTS`, `LD_*`, `MALLOC_*` and the other variables glibc drops for setuid programs (`GCONV_PATH`, `LOCPATH`, …; `TMPDIR`
stays) from the caller's environment (what changes bash itself; the
programs a check calls are found by the caller's `PATH`), within the limit above (8 minutes, or `--timeout`); processes it leaves in its process group are killed. A `## План` section
can also be supplied with `--text`; a `## Вход` section (context: request source and facts) goes
before `Что сделать` and is omitted when absent.

Ticket IDs accept the slug, the full file name, or the file name without `.md`. Outside a Git
repository, `tk` uses the nearest `docs/tasks/` up to `$HOME`; `new` creates it in the current
directory when none exists. `tk ls --all` includes closed tickets; `--status STATUS` filters by
status, `--by NAME` filters by executor (or opening actor on older tickets), and `--due` lists
waiting tickets whose deadline has passed: the time at the start of `--until` (`2026-10-28 09:00`, `2026-10-28T09:00`,
`28.10 09:00`, a date, RFC3339; `UTC` after it means UTC), any text after it is a note. A clock time without a date
gets today's date when written; `--until` without a time is a condition and never due. Empty acceptance sections are marked `no-check`.
When `.glint.yaml` exists at the task base, `tk done` checks all glint severities; findings block
completion unless `--glint-debt SLUG` links them to an open ticket.
`tk ls --root DIR` lists tasks for DIR and its first-level repositories, skipping linked worktrees;
`--slug PREFIX` filters by a case-insensitive slug prefix, and `--json` emits the filtered list with
completion details; a waiting ticket whose deadline parses has it in `due` (RFC3339), the same time `--due` compares. `can_take` on each row is whether `tk next` would give the ticket to its project's master, decided by the same code; filters do not change it, and where that cannot be worked out it is false with the reason on stderr. `next_rank` is the ticket's one-based place in the `tk next` order for `--by`, or for the project master when `--by` is omitted; tickets `tk next` cannot return have null or no rank. `actors` maps each journal author to the time of their last record (`YYYY-MM-DD HH:MM`), so a watcher sees a ticket still worked while it stays open. `tk archive --root DIR` archives each repository's tasks to its own
`docs/tasks-archive/` and prints paths relative to DIR. Completion journal entries record the
checker session when it differs from `--by` (`checker_session` in JSON): a fresh sub-agent runs in its
parent's session, so the session alone does not tell a self-check apart. `tk done --by` names the checker (a fresh sub-agent or worker); use `--doer` for the executor of an older ticket. `tk done` warns when the check runs in the executor's session and counts only as a check by a fresh sub-agent or a worker. It refuses tickets with an empty `Приёмка` or `Чем проверить` section. `tk release` returns an in-progress ticket to the open queue and clears its executor.
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

The `--usage` summary names the transcripts it read. Runs that write no transcript, such as
`claude -p --no-session-persistence`, are not counted, nor are codex rollouts: `--usage` does not
read their token counts.

Large transcript reads wait for machine capacity through graft's heavy queue; smaller ones run at once on two
threads.

All output, `--raw` included, has secret values masked as `[скрыто:KIND len=N #TAG]`: `N` is the
hidden value's length in characters and `TAG` a keyed hash of it: equal values get the same `TAG`
across `tl` calls, different ones rarely share it (24 bits), and the value itself is not shown. The key is created on the
first run in `$XDG_STATE_HOME/akit/mask-key` (`~/.local/state/akit/mask-key` when unset); without it
markers carry the kind only and stderr says why. The mask also hides some names that look like keys,
such as long `pk_test_…` identifiers: `N` tells whether it may be the name you expect, and the same
`TAG` elsewhere shows where the same text recurs.

`--wait PROG [SUB]` prints the nearest-rank percentile of milliseconds from a shell command call to
its output; calls without output, background Bash calls, and commands that also run unrelated programs
are excluded. The default percentile is 50; `--q` accepts 1–99. Fewer matching events than the
percentile needs (3 for `--q` 50 and below, 20 above) returns exit 3 with the count in stderr and no
stdout. Completed calls are cached per transcript in `$XDG_CACHE_HOME/akit/tl-wait` (or the user cache
directory when `XDG_CACHE_HOME` is unset); changing the transcript, the code that fills the entry (the `cmd/tl` sources and the module packages and versions it builds from) or the Go version invalidates its entry; reinstalling an unchanged `tl` keeps it. When some transcripts were read without the cache, stderr says how many.

## user-word

The user's own words from another session's transcript around a fragment: the end of the agent's
reply before it (what the user answered), the line itself and the user's next lines within N
minutes. Lines typed or queued by the human and answers in a question dialog count. Exit 0 found,
1 not found, 2 usage/IO error. `--brief` finds linked fragments and dated user
quotes in a brief, showing each matching line with its agent reply and up to 20
later user lines from the same session through the brief time. Without `--until`,
the brief file's modification time is used. A dated quote is
`пользователь DD.MM [HH:MM[:SS]] …: «quote»`: the first quote after the colon,
which may start on the next line in the same paragraph. A comma after the marker
word is accepted, as are one digit for the day or month, any case and grammatical
form of “пользователь”, and an approximate clock such as `~15:43`. Approximate
times search within 15 minutes; exact times search within two minutes. A zone
may follow the date or clock: `CET` and `CEST` mean UTC offsets +1 and +2;
`UTC`, `GMT`, and `Z` mean UTC, with an offset such as `UTC+3` or `GMT-2:30`;
all forms accept any case, optional parentheses and trailing punctuation. A zone
without a clock sets the timezone for the whole day. A clock followed by a zone
uses it; only listed uppercase Latin timezone abbreviations such as `MSK`,
`EST`, or `PST` are considered zone-like. `мск` and `по Москве` are too, with
optional parentheses. Known named zones are accepted in any case. Other text
after the clock is ignored for timezone selection, so the quote is searched in
the default timezone. If there are no transcript records on the marker day, not
found reports this reason. A year after the day (`31.10.2025` or `31.10.25`) is
used; two-digit years follow Go's 00–68 = 2000–2068 and 69–99 = 1969–1999 rule.
Several consecutive quotes after one marker are checked separately. The quote
matches whole words; `…` and `[…]` mark omitted text between ordered pieces. More
than one user line on the marker day, the same text at different times included,
is shown as ambiguous. Without a clock, the whole marked day is searched.
A transcript reference counts only when its path starts with `/` or `~/`, ends in
`.jsonl`, and is followed by a fragment in single or double quotes; several lines
with the fragment give the last one and a note. `~` expands only at the start of
`~/`. A marker without a quote, an impossible date, an empty quote, or a
`user-word …jsonl` reference in another form is reported as not found, not skipped.

    user-word ~/.claude/projects/-home-x/1a2b3c4d-….jsonl 'да, делай' [MINUTES]
    user-word --search 'да, делай' [--since 2026-10-24] [--project akit]
    user-word --brief task.md [--until '2026-10-31 15:00']

## timeit

Measures a command's median runtime in milliseconds, running it up to five times while the first-run
budget permits. It starts the first run unconditionally; each later run starts only if less than the
budget has elapsed since the first run started. After the first run, the effective budget is the smaller
of `--budget` and `--limit` minus that run's duration, with a floor of zero. Defaults are 45s for the
budget and 60s for the limit, so a slow first run leaves less time for repeats.

The command runs directly, without a shell. Its stdout is discarded and stderr is kept for reporting a
failed run. At most the last 64 KiB of stderr is printed, with a note when its beginning was truncated.
Input is empty unless `--stdin FILE` reopens a regular file for every run. `--ok-exit` accepts a
comma-separated list of successful exit codes; `--fail-on-stderr TEXT` marks a run as failed if stderr
contains the text. A failed run stops the measurement, prints its stderr and run number, and returns
its exit code (1 for a stderr match with exit code 0). If the command is killed by a signal, timeit prints
`signal NAME` and exits with 128 plus the signal number. Interrupting timeit with SIGINT, SIGTERM, or
SIGHUP also exits with 128 plus the signal number. Usage and launch errors exit 2.

    timeit --runs 5 --budget 45s --limit 60s -- ./bin/reference --case small

`timeit -h` lists all flags and defaults.

## wfor

Wait for conditions in one call, with a timeout, instead of a `while … sleep` loop. The answer
says what was met and when; on timeout, what is still missing and what it looks like now (file
absent, last line of the log, last exit and output of the command). Exit 0 done, 1 not yet,
2 usage error or a failed check.

The default timeout is 4m30s, under the 5-minute life of a sub-agent's prompt cache: a longer wait
is the same call again (the answer says how; with `--new` it gives `--after N` so lines written
between the calls still count). A line begun before the start does not count under `--new`, even
when it is finished later, and `--after N` counts it among the N.

    wfor line '^(ok|FAIL)' test.log                # a line in a log (FILE may not exist yet)
    wfor exit 31337 31338                          # processes ended (zombies count as ended)
    wfor file result.tsv ok 'worker status | grep -q done' --any
    wfor idle                                      # machine calm by graft's rule

Conditions: `file PATH`, `gone PATH`, `line RE FILE`, `exit PID...`, `time T`, `ok CMD`, `idle`. The `ok`
command runs in a process group of its own, killed when it exits and on timeout; a process that leaves the
group (`setsid`, a daemon) is left running, and the timeout still holds.
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
