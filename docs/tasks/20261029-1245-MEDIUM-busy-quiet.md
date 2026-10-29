# busy: тихий режим до заданного часа

Статус: готово
Исполнитель: akit-master

## Вход
- Задание лида 29.10 ~12:45 CET, итог — git-admin. Повод: на время, когда человеку нужна отзывчивая машина, тяжёлое должно ждать одной командой, без рассылки агентам.
- Тесты и сборку не запускать до 14:45 CET (бережём диск).

## Что сделать

## Приёмка
- `busy --quiet 1m` → `busy` код 1 «busy: quiet mode until HH:MM»; `busy --quiet-off` → прежний ответ.
- Тесты pressure и cmd/busy: активный, истёкший, нет файла, битый файл, прошедшее время в --quiet-until.

## Сделать
- Тихий режим в правиле `pressure` (одно правило на всех): Meter.Check читает файл срока; срок не прошёл — Verdict.Measure = `pressure.Quiet`, Why «quiet mode until HH:MM». Так его сразу видят busy, graft (loadgate через Meter) и worker в llmark (pressure.Check).
- Файл: `$XDG_STATE_HOME/akit/quiet-until`, без XDG — `~/.local/state/akit/quiet-until`; одна строка RFC3339 с поясом. Нет файла — не тихо; истёкший — не действует, без уборки; битый — ошибка замера.
- `busy --quiet-until 14:45` (timearg: часы сегодня, день с часами, RFC3339; прошедшее — ошибка), `busy --quiet 2h`, `busy --quiet-off` (удаляет файл); запись атомарная.
- Согласовать с graft-master: строка `pressure.Quiet` в карте `measures` loadgate (иначе graft сочтёт тихий режим свободной машиной); тесты graft с GRAFT_PROC — свой XDG_STATE_HOME.
- Релиз akit, `graft install`, сообщить лиду и graft-master версию.


## Журнал
- 2026-10-29 12:45 открыт — akit-master
- 2026-10-29 12:47 в работе — akit-master
- 2026-10-29 12:47 note — akit-master: код написан (pressure/quiet.go, Rule/Check, cmd/busy флаги, тесты), тесты ждут 14:45; graft-master принял формат, сделает Quiet в measures, неизвестную меру — занятой, XDG в тестах; ему отправлены Busy()=true, имя --quiet-off, порядок чтения
- 2026-10-29 12:47 ждёт — akit-master — 2026-10-29 14:45
- 2026-10-29 14:24 в работе — akit-master
- 2026-10-29 14:24 note — akit-master: лид 14:03 CET: созвон кончился, диск можно — гоняю тесты
- 2026-10-29 14:25 note — akit-master: тесты pressure и cmd/busy зелёные, glint без находок (parseArgs — разбор флага в options.set, TestMain заменён t.Setenv); ручная проба busy --quiet 1m → код 1, --quiet-off → free; коммит и релиз --minor
- 2026-10-29 14:32 на проверке — akit-master — 5d3c7e9, v1.6.0, graft install (~/bin и pc) busy 5d3c7e9; ждёт независимой проверки и строки Quiet в graft loadgate
- 2026-10-29 14:48 готово — graft-master: связка с graft 7578c7d (v2.53.0+), отдельные XDG_STATE_HOME/XDG_CACHE_HOME: busy --quiet 2m → graft exec --heavy -- true ждёт со строкой «quiet mode until 14:50 (busy --quiet-off ends it), waiting» (убит timeout 8s, rc 124); busy --quiet-off → graft exec --heavy -- echo started стартует сразу, rc 0; тест graft loadgate TestBusyDiskAndSwapHoldTasks/quiet и TestQuietModeOverHoldsNoTask зелёные
