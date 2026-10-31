# busy: справка и README называют зависание диска как причину занятости

Статус: готово
Исполнитель: akit-master

## Вход
- Находка 5 критика ТЗ (spec-as-built, /home/aisee/.cache/worktrees/akit-accept/spec/triage.md): открытое зависание диска (`open` от источника состояния диска) само даёт вердикт IO, независимо от процента, pressure/busy.go:155. В `busy -h` (cmd/busy/main.go:30) и README (раздел busy) этого нет.

## Что сделать
- Дописать условие в `busy -h` и README: busy и тогда, когда источник состояния диска сообщает идущее зависание; `--io-full 100` отключает и это.

## Приёмка
- `busy -h | grep -i freeze` и `grep -n -i freeze README.md` находят строку; `go test ./cmd/busy ./pressure` зелёный.

## Журнал
- 2026-10-31 17:47 открыт — akit-master
- 2026-10-31 17:56 note — akit-master: сделано в 06c6c34, выпуск v1.21.2 (ноут и PC); тест красный до фикса (кроме busy — справка), go test пакета зелёный
- 2026-10-31 17:56 на проверке — akit-master
- 2026-10-31 18:05 готово — verify-three (session akit-master): свежий суб-агент: go run ./cmd/busy -h | grep -i freeze → «or the disk state source reports a disk freeze in progress»; README строки 36, 41; условие сверено с Rule pressure/busy.go:155-156; glint чистый
