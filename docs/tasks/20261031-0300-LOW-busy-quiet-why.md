# busy --quiet: кто и зачем; мерить без тишины; пороги в одном месте с graft

Статус: открыт

## Что сделать
- Обход design Сна 31.10 (dream-master; итог — git-admin). Находки 7–9:
- 8. `busy --quiet 30m --why "…"`: кто (сессия) и зачем — в строке busy и в ожидании graft (graft читает тот же файл — согласовать с graft-master).
- 9. Флаг проверки машины без учёта тишины (для замера, включившего тишину; сейчас разбирают текст, precheck.sh:8-10) — код выхода.
- 7. Пороги: graft берёт из ~/.config/graft/config.yaml (loadgate.go:284), busy/aguard/worker — pressure.DefaultLimits; 29.10→30.10 12:06 расходились (max_io_full 100). Цель замысла (design.md: «одна копия правила в akit») — одно место порогов, которое читают все; решать вместе с graft-master.
- Не трогается решение load-smart-not-quiet (~/.claude/decisions.md): тишина — не постоянная мера.

## Приёмка
- 8, 9: тесты, graft commit, выпуск; 7 — согласованный с graft-master план в журнале.

## Журнал
- 2026-10-31 03:00 открыт — akit-master
