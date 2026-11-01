# tl --usage: итог называет охват журналов

Статус: открыт

## Что сделать
От llmark-master (сверка курса, тикет llmark spend-reconcile): `tl --all --usage` не видит запуски `claude -p --no-session-persistence` — у них нет журнала в ~/.claude/projects (кейсы Сна пишут stream-json в ~/.claude/improve/runs/<дата>/). Окно 31.10 09:00 → 01.11 04:11 CET: tl $885,6, dreamstats $973,3, разница $87,7; лид получил курс ~$2200 вместо 2474.

Что сделать:
- итоговая строка `tl … --usage` называет охват: какие журналы прочитаны (папка проектов, rollout codex при --codex) и что запуски без журнала не входят;
- то же в `-h` у `--usage`.

Чем проверить:
- `tl --all --since 1h --usage | tail -1` — строка с охватом;
- тест на строку итога в cmd/tl/usage_test.go.

## Приёмка

## Журнал
- 2026-11-01 04:50 открыт — akit-master
