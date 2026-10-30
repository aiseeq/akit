# perf: tl --wait references include codex fighters

Статус: готово
Исполнитель: akit-master

## Что сделать
- Восемь строк `tl --wait` в ~/.claude/improve/state/perf/akit.tsv — с `--codex` (формат FORMAT.md: мера ожидания по журналам сессий и бойцов).
- Референсы пересчитать тем же способом: худшее суточное p95 за 7 суток до 30.10 12:50, сутки < 20 событий пропущены, с `--codex`.
- akit.md: таблица суточных, «Уже меряется», «Как мерили»; строки `--grep` (wall) не трогать.

## Приёмка
- Каждая команда tsv печатает число, код 0; референс = максимум суточных из akit.md.
- `grep -c -- '--codex --since 24h --wait' akit.tsv` = 8.

## Журнал
- 2026-10-30 16:06 открыт — akit-master
- 2026-10-30 16:06 в работе — akit-master
- 2026-10-30 16:29 note — akit-master: Пересчёт daily-codex.sh (END 30.10 12:50): изменился только cli:sub 326→372; tsv — 8 строк с --codex; akit.md — таблица суточных, «Уже меряется», «Как мерили», wall. Прогон 8 команд 16:29 под graft exec --heavy: все код 0, 6,6–11,5 с.
- 2026-10-30 16:29 на проверке — akit-master
- 2026-10-30 16:33 готово — akit-master-verify-subagent (session akit-master): Суб-агент Opus со свежим контекстом по брифу из тикета, 30.10 16:30–16:32: grep 8 строк --codex; референсы tsv = максимум суточных md по 8 путям; окна sub и tl за 28–30.10 перемерены под graft exec --heavy (187/231, 7711/10508 = таблица); 8 команд tsv — код 0, одно число, 5–11 с. Выводы: /home/aisee/.cache/claude-tmp/claude-1000/-home-aisee-git-akit/fa3db3bc-fc47-4186-ab75-59db236a4634/scratchpad/windows.out, tsvcmds.out. Его замечание о составе недельного разбора внесено в akit.md.
