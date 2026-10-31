# graft: тесты гейта на PC при занятом ноуте

Статус: ждёт
Ждёт: 2026-10-31 06:10
Исполнитель: akit-master

## Вход
- Просьба graft-master (по заданию git-admin, remote-heavy-share) 30.10: `remote: true` задаче test, чтобы go test гейта при занятом ноуте шёл на PC. 29.10 у akit 6 гейтов, 61 мин ожидания машины. Проба graft-master на PC: `worker pc --tree /home/aisee/git/akit -- go test -count=1 -p 16 ./...` — 21 пакет ok, 12 с.
- Тесты читают только синтетику во временных папках (t.TempDir, Setenv HOME/XDG_RUNTIME_DIR, fakeProc); корпусные — по env, без него Skip; из PATH — bash, sleep.

## Что сделать
- `.graft.yaml`: tasks.test.remote: true с комментарием условия.

## Приёмка
- Чем проверить: `graft commit` гейт проходит; замер graft-master по ~/.cache/graft/stats.jsonl: шаги go test гейта akit при занятом ноуте уходят на PC и проходят.

## Журнал
- 2026-10-30 06:03 открыт — akit-master
- 2026-10-30 06:03 в работе — akit-master
- 2026-10-30 06:05 note — akit-master: Коммит 434eef2 06:05 CET, гейт прошёл; graft-master извещён — замер его.
- 2026-10-30 06:05 на проверке — akit-master
- 2026-10-30 06:07 ждёт — akit-master — 2026-10-31 06:10
- 2026-10-31 15:47 note — akit-master: 15:47 CET: итог замера graft-master (31.10): перелив на PC гейт замедляет — go test на PC 30,7 с против 1,5 с здесь: разовый worker pc стирает кеш Go после запуска (llmark tool/cmd/worker/pc.go:176, факт у llmark-master); ожидание машины упало от graft v2.65.0 (29.10 615 с на 6 гейтов → 31.10 34 с на 29), не от перелива. remote: true снят; вернуть — когда кеш Go на PC станет постоянным.
