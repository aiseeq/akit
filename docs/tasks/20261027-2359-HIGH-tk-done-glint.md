# tk done: отказ при находках glint в репо с .glint.yaml

Статус: готово
Исполнитель: luna:1027-2312-tk

## Вход
- Проблема: гейт коммита во всех Go-проектах — только high (fail_on по умолчанию), остальные находки glint, low тоже, — до «готово» тикета; проверить это должен механизм при закрытии (`~/.claude/decisions.md` `## glint-threshold`, решение пользователя 27.10 ~23:56 CET, `~/.claude/rules/go.md`). git-admin переводит 11 репо с `fail_on: low` на high только после выпуска этой проверки.
- Метод: `tk done` запускает glint в корне базы и судит по JSON.
- Дешевле: нет — правило без механизма не соблюдается.
- Инструмент: `tk` (akit), glint (`glint check . -s low -o json`: `.summary.total`, `.issues[] {rule, severity, file, line, message}`; флага `--fail-on` нет).
- Цена: один заход бойца Luna.
- Если не делать: 11 репо остаются на `fail_on: low`, а medium и low копятся без срока.

## Что сделать
1. `tk done` (после всех прежних проверок, до записи файла), если в корне базы (корень репо или папка с docs/tasks) есть `.glint.yaml`: `GOWORK=off glint check . -s low -o json` в этой папке (glint — из PATH). `.summary.total` > 0 — код 1, файл не меняется; вывод: `tk: glint: N findings; fix them or pass --glint-debt SLUG (an open ticket for all of them)` и до 10 строк `severity file:line rule`, затем `… and K more`, если больше.
2. glint не найден в PATH, упал без JSON или JSON не разобрался — код 2 с причиной, файл не меняется (не «пропустить проверку»).
3. `--glint-debt SLUG`: тикет SLUG в той же базе существует и незакрыт (открыт, в работе, ждёт, на проверке), иначе код 1 с причиной. С ним done проходит при находках, в строку журнала «готово» дописывается `; glint: N findings, debt SLUG`.
4. Нет `.glint.yaml` — проверки нет, как раньше. Находок 0 — done как раньше.
5. Usage и README: `--glint-debt` и сама проверка.

## Приёмка
- `bash ~/.cache/akit-accept/tk-glint.sh` — `OK` (включает tk-tx.sh); `graft gate` зелёный.
- Живьём: `tk done` в репо с находками (dream: 5) отказывает, в akit (0) проходит.

## Журнал
- 2026-10-27 23:59 открыт — akit-master: просьба лида (решение пользователя `glint-threshold`).
- 2026-10-28 00:00 в работе — luna:1027-2312-tk
- 2026-10-28 00:00 note — akit-master: задание бойцу 1027-2312-tk, дерево ~/.cache/worktrees/akit/tk (ветка tk-glint от main), приёмка ~/.cache/akit-accept/tk-glint.sh
- 2026-10-28 00:05 готово — akit-master: ревью диффа (prepareDone/checkGlint), приёмка ~/.cache/akit-accept/tk-glint.sh OK на main, живьём: клон dream — done отказал с кодом 1 и списком находок; v1.5.0
