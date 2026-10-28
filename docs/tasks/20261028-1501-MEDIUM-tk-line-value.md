# tk: title и waiting — одна функция значения строки по префиксу

Статус: на проверке
Исполнитель: akit-master

## Вход
- glint-master 28.10: near-duplicate-func (до двух различий), cmd/tk/main.go:778 waiting ~ :542 title — различаются префиксом и умолчанием.

## Что сделать
1. Одна функция `lineValue(s, prefix) string` (значение первой строки с префиксом, "" если нет); title — `cmp.Or(lineValue(s, "# "), "(no title)")`, waiting — `lineValue(s, "Ждёт: ")`.

## Приёмка
- `glint check .` без near-duplicate-func по cmd/tk; `graft gate` зелёный.

## Журнал
- 2026-10-28 15:01 открыт — akit-master
- 2026-10-28 15:01 в работе — akit-master
- 2026-10-28 15:03 на проверке — akit-master — 68169ea: lineValue(s, prefix), title — cmp.Or с "(no title)", waiting — lineValue; graft gate и glint check . без находок; tk установлен
