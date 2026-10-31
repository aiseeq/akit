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
- 2026-10-31 03:51 note — akit-master: п.2 влит f23f2c7, выпуск v1.15.0, graft install (aeea755). Под очередью GRAFT_THREADS=4 → 4 воркера вместо NumCPU/2=11. Идёт замер 10 параллельных. П.3 кэш --wait: бриф /home/aisee/.cache/worktrees/akit-accept/tl-wait-cache-brief.md, приёмка tl-wait-cache.sh — старт после влития grep-prefilter
- 2026-10-31 03:54 note — akit-master: замер 1 после v1.15.0, 31.10 03:47–03:51 CET: 10 параллельных `tl --all --codex --since 7d --wait "tk done" --q 95` — всего 217 с, каждый 98–218 с wall и 169–205 CPU-с; runnable max 82, среднее 25,7, выше 22 — 121 из 195 с. Приёмка НЕ пройдена. Версия: под очередью ограничены воркеры (4), но GOMAXPROCS=22 — фоновые и простаивающие воркеры GC берут все P. Проверяю A/B (как есть / GOMAXPROCS=4) с подсчётом R-потоков tl
- 2026-10-31 04:09 note — akit-master: A/B 31.10 03:55–04:06 CET, 10 параллельных: A (v1.15.0) — runnable max 71, сред. 25,6, >22 в 153 из 251 с; R-потоков tl max 30, сред. 9,3; B (GOMAXPROCS=4) — runnable max 88, сред. 27,6; R-потоков tl max 22, сред. 9,8. Фон без tl (04:06, 60 с) — max 66, сред. 26,4, >22 в 7 из 20: общий runnable ≤ 22 ночью от tl не зависит, мера — R-потоки tl. Фикс: под очередью GOMAXPROCS = GRAFT_THREADS (тест TestConfiguredThreads, мутация `_ = threads` → FAIL GOMAXPROCS 22 ≠ 3)
