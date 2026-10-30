# tl --wait: не держать все события в памяти

Статус: готово
Исполнитель: akit-master

## Вход
- Замер для perf-baseline (30.10 12:29 CET): `tl --all --since 7d --wait 'tk done' --q 95` — 21,0 с wall, 68 CPU-с, RSS 5,0 ГБ; `--since 24h` — 4,1 с, RSS 0,8 ГБ. reportWait (cmd/tl/wait.go) работает по res.hits: scan копит все события всех ролей окна, с текстом вывода.

## Что сделать
- Режим --wait сводит вызов с выводом по ходу чтения файла (пара — в пределах файла, ключ и так path+CallID): в памяти только вызовы, ждущие вывода, и найденные ожидания; события не копятся.
- Вывод чисел не меняется.

## Приёмка
- Чем проверить: go test ./cmd/tl/ (тесты --wait зелёные без правки ожиданий); числа `tl --all --since 7d --wait P --q 95` для sub, tl, 'tk done' байт в байт до и после; RSS 7d < 200 МБ (`/usr/bin/time -f %M`); glint 0.

## Журнал
- 2026-10-30 12:30 открыт — akit-master
- 2026-10-30 12:30 в работе — akit-master
- 2026-10-30 12:33 note — akit-master: Сделано: waitTracker (cmd/tl/wait.go) сводит вызов с выводом в filter.file по ходу чтения, result.waits вместо res.hits. Числа --wait p95 за окно 29.10 12:30–30.10 12:30 (sub 242, tl 10508, tk done 15735, tk note 259, prior 7529) байт в байт до и после. 7d tk done: RSS 5,05/5,09 ГБ → 55/70 МБ; wall 16–18 → 13–15 с; CPU 74–81 → 87–123 с (шум общей машины, quiet до 12:45). go test ./cmd/tl ok.
- 2026-10-30 13:42 на проверке — akit-master — e9c040b; числа до/после /home/aisee/.cache/claude-tmp/perf-usage/wait-before.txt, wait-after.txt
- 2026-10-30 13:47 готово — akit-master-checker (session akit-master): независимая проверка e9c040b: go test ./cmd/tl ok, тесты не менялись; --wait p95 и p50 по sub, tl, tk done, tk note, prior за 29.10 12:30–30.10 12:30 байт в байт старый /home/aisee/bin/tl против нового (q1/q99 у tl и tk done тоже); 7d tk done RSS 94 МБ (до 5,0 ГБ), вывод 5380; glint check ./cmd/tl -s low — 0; логика сведения та же (ключ CallID в файле, count!=1, фон, d>=0), события не копятся
