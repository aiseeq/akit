# tl: тяжёлый вызов проходит допуск очереди и дешевле вдвое

Статус: в работе
Исполнитель: akit-master

## Что сделать
- Задание лида 31.10 (тикет лида /home/aisee/docs/tasks/20261028-0000-HIGH-load-control.md; итог — git-admin). 30.10 в 14:10, 16:10 и 18:30–19:05 серии по 40–100 `tl --all --codex --wait … --since 7d` (~90 CPU-с на вызов, часто параллельно) дали ld1 до 60 и runnable до 330 при 22 потоках.
- 1. Замер: на что уходят CPU-с одного `tl --all --codex --since 7d --wait …` (perf по обоим событиям, скилл go-performance).
- 2. Допуск: тяжёлый вызов tl сам проходит очередь graft — как бы его ни запустили. По замыслу (design.md, wfor idle): правило «машина занята» одно, в graft; тяжёлая команда идёт через `graft exec --heavy --local`, а не после ожидания.
- 3. Ускорение ×2 и больше без потери вывода, если профиль его даёт (кэш разобранного, индекс по времени) — решение моё.
- Пересекается с 20261031-0231-HIGH-grep-prefilter (тот же cmd/tl/scan.go): сначала влить его, потом правки этого тикета.

## Приёмка
- 10 параллельных `tl --all --codex --since 7d` — runnable не выше 22 (замер `busy`/`/proc/loadavg` раз в секунду), до и после — в журнал тикета.
- CPU-с и wall одного вызова до/после; вывод `--wait` и `--grep` байт в байт против эталона.
- graft commit зелёный, graft install; итог — git-admin.

## Журнал
- 2026-10-31 02:42 открыт — akit-master
- 2026-10-31 02:44 в работе — akit-master
- 2026-10-31 02:44 note — akit-master: п.2 допуск — боец Luna 1031-0244-tl-admission (фича-инструмента): бриф и приёмка /home/aisee/.cache/worktrees/akit-accept/tl-admission{-brief.md,.sh}; перезапуск себя через graft exec --heavy --local от порога размера файлов, GRAFT_THREADS = воркеры. п.1 профиль --wait 7d — мастер, в очереди graft. Замер «до» 10 параллельных не делаю нарочно (перегрузил бы машину): «до» — числа лида 30.10 (runnable до 330)
- 2026-10-31 02:58 note — akit-master: п.1 замер 31.10 02:55 CET (load 17, шумно): tl cea1750 `--all --codex --since 7d --wait "tk done" --q 95` — wall 42 с, CPU 147 с (138 user + 9 sys), maxrss 80 МБ. perf record плоский (с -g события не открываются, perf_event_paranoid 2), 580 тыс. выборок cpu_core+cpu_atom: encoding/json 69 % (stateInString 25, checkValid 17, skip 11, unquoteBytes 6), runtime память/GC 7 %, shellcmd/mvdan sh ~2 %. Сутки 29.10 (окно 1 д): CPU 61 с; сборка с GOEXPERIMENT=jsonv2 — 44 с, вывод тот же (×1,4, не ×2). План п.3: кэш завершённых вызовов на файл (команда, время вызова, время вывода; ключ — путь+размер+mtime+хэш бинарника tl), --wait считает по нему — серии вызовов на тех же файлах ×10+; порог допуска — по байтам без кэша. Делать после влития grep-prefilter и tl-admission (все в cmd/tl)
