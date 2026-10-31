# tl: id:-ссылки, ISO-время без пояса, --version, --width у REF, несколько REF

Статус: открыт

## Что сделать
- Обход design Сна 31.10 (dream-master; итог — git-admin, копия dream-master). Проверено мастером 31.10 ~03:00 CET: (1) `tl id:a0d89eb8:1` → «no live session…» (агенты пишут буквально как в ~/.claude/CLAUDE.md «`tl id:строка`», 40 падений за 3 дня); (2) `--since 2026-10-30T21:00` отвергается, а `2026-10-22 13:40` проходит (9 ошибок за 3 дня); (3) `tl --version` → «--version needs a value».
- 1. splitRef (cmd/tl/source.go:228) снимает приставку `id:`.
- 2. Время `YYYY-MM-DDTHH:MM[:SS]` без пояса — местное.
- 3. `--version` — версия сборки (debug.ReadBuildInfo), как у других утилит akit, если есть общий приём; иначе — ошибка «unknown flag» вместо «needs a value».
- 4. `--width` действует и на REF (showRef, scan.go:303); несколько REF за вызов (source.go:232).
- 5. Алиасы ролей tool_use/bash→cmd, result→out; подсказка про --grep/--project в ошибке SESSION; `--wait` печатает и n событий (в stderr, stdout — одно число по FORMAT.md).
- После влития grep-prefilter, tl-admission, tl-кэша (все в cmd/tl) — чтобы не конфликтовать.

## Приёмка
- Тесты на 1–5; `tl id:<id>:1` = `tl <id>:1` байт в байт; `--since 2026-10-30T21:00` = `--since '30.10 21:00'`.
- graft commit, graft install; итог git-admin и dream-master.

## Журнал
- 2026-10-31 03:00 открыт — akit-master
