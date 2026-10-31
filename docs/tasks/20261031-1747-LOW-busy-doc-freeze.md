# busy: справка и README называют зависание диска как причину занятости

Статус: открыт

## Вход
- Находка 5 критика ТЗ (spec-as-built, /home/aisee/.cache/worktrees/akit-accept/spec/triage.md): открытое зависание диска (`open` от источника состояния диска) само даёт вердикт IO, независимо от процента, pressure/busy.go:155. В `busy -h` (cmd/busy/main.go:30) и README (раздел busy) этого нет.

## Что сделать
- Дописать условие в `busy -h` и README: busy и тогда, когда источник состояния диска сообщает идущее зависание; `--io-full 100` отключает и это.

## Приёмка
- `busy -h | grep -i freeze` и `grep -n -i freeze README.md` находят строку; `go test ./cmd/busy ./pressure` зелёный.

## Журнал
- 2026-10-31 17:47 открыт — akit-master
