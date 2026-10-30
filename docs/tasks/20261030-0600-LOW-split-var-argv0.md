# shellcmd: argv0 из переменной без кавычек делится на слова

Статус: на проверке
Исполнитель: akit-master

## Вход
- Найдено при замере для ansi-c-offset (30.10): `P="tmux -L ptyprobe"; timeout 120 bash -c "until $P capture-pane …"` — Call.Name `tmux -L ptyprobe`; bash делит значение `$P` без кавычек на слова и запускает `tmux` с аргументами `-L ptyprobe …`. Корпус corpus-search-dryrun: 2 вызова.

## Что сделать
- argv0 из переменной без кавычек (`$P`, `${P}`), присвоенной литералом: значение делится по пробелам, табам и переводам строк; первое поле — имя, остальные — аргументы перед прежними (Offset у них -1). `"$P"` в кавычках — имя целиком, как сейчас.

## Приёмка
- Чем проверить: TestCalls — `P="pgrep -f"; $P x` → pgrep [-f x]; `P="pgrep -f"; "$P" x` → имя `pgrep -f`; `P="timeout 5"; $P pgrep x` → pgrep через timeout; `go test ./shellcmd/ ./transcript/...`, glint 0; имена вызовов по обоим корпусам меняются только в командах с argv0 из такой переменной.

## Журнал
- 2026-10-30 06:00 открыт — akit-master
- 2026-10-30 06:00 в работе — akit-master
- 2026-10-30 06:02 note — akit-master: Сделано: words делит значение argv0 из $P без кавычек по пробелу, табу, переводу строки (splitWords вставляет слова с Offset -1); collectAssignments хранит нелитеральное значение без кавычек (unquotedText), иначе имя было `"curl`/`x"`. TestVariableArgv0Split (6 случаев, красный до правки). Сверка v1.13.1 → рабочее дерево (/home/aisee/.cache/claude-tmp/split-cmp/, names.sh): waits-corpus 188 строк, corpus-search-dryrun 9 — все со сменой имени мусор → программа (git, workspace-cli, curl, tmux), строк только со сменой аргументов 0.
- 2026-10-30 06:02 на проверке — akit-master
