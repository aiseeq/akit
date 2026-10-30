# tl: суб-агенты в --all с пометкой родителя, фильтр --raw

Статус: на проверке
Исполнитель: akit-master

## Вход
- Задание лида 30.10 ~03:25 CET (письмо в сессию akit-master), итог — git-admin. Факт — сухой прогон dream-master /home/aisee/.claude/improve/runs/2026-10-30/corpus-search-dryrun.md: за 7 дней 650 вызовов rg/grep читали корпус транскриптов, 392 из них — суб-агенты; `tl --all` не читает `~/.claude/projects/*/*/subagents/agent-*.jsonl` (cmd/tl/source.go allSources: глоб `*/*.jsonl`), поэтому агенты ищут сами.
- Класс «прочее (типы записей, подсчёт совпадений)» — 54 из 650 (8 %), единственный без пути в tl: фильтра по сырой строке JSON нет. Решение мастера: стоит — правка мала (фильтр до разбора строки), это единственный класс, которому хук на чтение корпуса оставил бы только rg; фильтр до разбора ещё и дешевле по CPU.
- Рядом с транскриптом суб-агента лежит `agent-<id>.meta.json`: `{"agentType", "description", "toolUseId", …}`.

## Что сделать
1. `tl --all` читает и суб-агентов: `*/*/subagents/agent-*.jsonl` (тот же фильтр --since по mtime, --project по пути). Флаг `--no-subagents` — как раньше.
2. Пометка родителя: id источника суб-агента — 8 символов id агента (из имени файла `agent-<id>.jsonl`); заголовок источника `== <id> sub of <id родителя> <имя родителя> "<description из meta.json>" <путь>` (имя родителя — как сейчас у основной сессии; meta нет — без description). Ссылки `tl <id>:<строка>` и `tl <id>` (префикс) находят и транскрипт суб-агента.
3. `tl SESSION` (сессия по имени или id) — по-прежнему только основной транскрипт; `--subagents` добавляет её суб-агентов.
4. `--raw RE` — оставить только события, чья сырая строка jsonl совпала с RE (без учёта регистра), проверка до разбора JSON; сочетается с --grep (оба условия), --role, --since, -C. В --help — строка с примером (`--raw '"hook_cancelled"'`).
5. README раздела tl и `tl -h`.

## Приёмка
- `graft gate` зелёный.
- Тесты на синтетике (временный HOME с projects/<проект>/<uuid>.jsonl и <uuid>/subagents/agent-<id>.jsonl + meta.json): --all видит суб-агента с заголовком «sub of», --no-subagents — нет; ref `<id>:<строка>` суб-агента открывает событие; `tl <родитель> --subagents`; --raw отбирает по полю, которого нет в тексте события (например `"isSidechain":true`), и вместе с --grep.
- Живая проба: `tl --all --since 30m --role cmd --max 3` показывает строки суб-агентов, если они были за окно; `tl --all --since 1d --raw '"hook_cancelled"' --max 3`.

## Журнал
- 2026-10-30 03:29 открыт — akit-master
- 2026-10-30 03:29 в работе — akit-master
- 2026-10-30 03:29 note — akit-master: боец Luna, дерево /home/aisee/.cache/worktrees/akit/tl-subagents, бриф и приёмка /home/aisee/.cache/worktrees/akit/tl-subagents-brief/
- 2026-10-30 03:46 на проверке — akit-master — влито b032b49 (Luna) и ed6d2b3 (мастер: parseArgs разбит по находке glint cyclomatic 25>20); приёмка мастера ACCEPT OK; живые пробы: --all --since 4h --project akit --max 0 — 3 заголовка «sub of 157d4aea» с description из meta; --raw hook_cancelled --role all — 1 событие (без --role all роль attach не в умолчании); --max 100 по умолчанию прячет ранние события суб-агентов — не дефект
- 2026-10-30 03:49 note — akit-master: независимая проверка (суб-агент): не принято — tl <префикс> и tl <путь> суб-агента шли через newSource: id = полный путь, без «sub of» (source.go resolve). Починено: один выбор конструктора sourceAt для resolve, allSources, showRef; тест проверяет заголовок и ссылку abcdef01:1 по префиксу и по пути. Замечание проверки: трейлер Worker в ed6d2b3 поставил graft по дереву бойца, коммит мастера — запушен, не правлю
