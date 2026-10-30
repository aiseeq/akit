# graft: тесты гейта на PC при занятом ноуте

Статус: в работе
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
