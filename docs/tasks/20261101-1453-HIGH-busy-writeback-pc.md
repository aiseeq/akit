# busy writeback: порог от настроек машины, не 64 MiB везде

Статус: в работе
Исполнитель: akit-master

## Что сделать
- Баг от лида 14:5x: v1.22.15 на PC (dirty_bytes 0, dirty_background_ratio 10) — `busy` код 1 по writeback при 205 MiB; порог 64 MiB взят с ноута (dirty_background_bytes 64 MiB). PC — цель перелива тяжёлого.
- Порог по умолчанию — порог фонового сброса самой машины: vm.dirty_background_bytes, при 0 — dirty_background_ratio % от MemAvailable; max_dirty_mb перекрывает; нет /proc/sys/vm — мера пропущена (ErrNoDirtyLimit).

## Приёмка
- Тесты pressure: байтовый порог 64 MiB → занята при 68; доля 10% от 20000 MiB → свободна при 205; свой порог без /proc/sys/vm.
- /home/aisee/.cache/worktrees/akit-accept/busy-writeback-accept.sh: синтетика + живой busy на ноуте и PC; на PC под обычной нагрузкой не занят по writeback.
- Независимая проверка Terra.

## Журнал
- 2026-11-01 14:53 открыт — akit-master
- 2026-11-01 14:53 в работе — akit-master
