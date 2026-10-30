# tl --wait: event threshold depends on --q

Статус: готово
Исполнитель: akit-master

## Что сделать
- cmd/tl/wait.go reportWait: порог событий по `--q` — q ≤ 50 → 3, выше → 20 (формат перф-референса: медиана — 3 события в сутки, p95 — 20). Запрос dream-master 30.10.
- Текст ошибки, `tl -h` и README — порог по q.

## Приёмка
- Тест: 3 и 19 событий при `--q 50` — число, код 0; при `--q 95` — код 3; 2 события при `--q 50` — код 3.
- `graft commit` зелёный; после `graft install` `tl --all --since 24h --wait 'tk new' --q 50` печатает число.

## Журнал
- 2026-10-30 18:29 открыт — akit-master
- 2026-10-30 18:29 в работе — akit-master
- 2026-10-30 18:30 note — akit-master: cea1750, v1.14.2, graft install. Живьём 30.10 18:3x: page-shot за 24h — 4 события: --q 50 → 47 мс код 0, --q 95 → код 3 «needs at least 20»; tk new q50/q95 — числа.
- 2026-10-30 18:30 на проверке — akit-master
- 2026-10-30 18:32 готово — akit-master-verify-subagent (session akit-master): Суб-агент Opus со свежим контекстом, 30.10 18:31 CET: порог q≤50→3, иначе 20; go test -run Wait PASS на HEAD, красный (3 случая) со старым порогом в копии worktree на cea1750; tl -h и README описывают порог; живьём page-shot 24h — 4 события: --q 50 → 47 код 0, --q 95 → код 3. Его замечание (граница q=51) добавлено в тест.
