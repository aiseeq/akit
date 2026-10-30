# shellcmd: границы тела heredoc во вложенном скрипте в исходной команде

Статус: готово
Исполнитель: akit-master

## Вход
- llmark-master 30.10 ~05:45 (тикет llmark 20261030-0420-LOW-cheat-heredoc-shellcmd): cheat на shellcmd v1.12.1 — 6 новых ложных находок на 145611 вызовах, 13 из 15 разобранных расхождений — `/bin/bash -lc "cat > F <<'EOF' … EOF …"`: у вложенного скрипта BodyStart/BodyEnd −1, искать Body в строке нельзя (`\"`, склейка `"'…'"`). Нужен охватывающий диапазон тела в исходной команде.

## Что сделать
1. parseNested: BodyStart/BodyEnd вложенного вызова — по карте pos (nested-offset): BodyStart = pos[первый байт тела], BodyEnd = pos[последний байт]+1; нет места у любого края — оба −1. Пустое тело — −1, как сейчас.
2. Документ Redir: у вложенного скрипта диапазон охватывает тело в исходнике, текст там может отличаться от Body кавычками и экранированием; на верхнем уровне cmd[BodyStart:BodyEnd] == Body, как сейчас.
3. checkPositions: верхний уровень — равенство; вложенный — края совпадают (cmd[BodyStart] == Body[0], cmd[BodyEnd-1] == Body[last]), BodyStart ≤ BodyEnd.

## Приёмка
- Тест на форму llmark (bash -lc "cat > F <<'EOF'\n…\"'`x`'\"…\nEOF\ngit status") и на `bash -c '…<<EOF…'`, ssh, вложенность; $'…' → −1.
- Корпуса: TestCorpus зелёный, имена вызовов без изменений; graft gate, glint 0; waits accept.sh ACCEPT OK.

## Журнал
- 2026-10-30 05:44 открыт — akit-master
- 2026-10-30 05:46 в работе — akit-master
- 2026-10-30 05:46 note — akit-master: сделано мастером в ветке nested-heredoc-bounds (a71cff5, поверх 885b465): outerSpan по карте pos; тесты (форма llmark, одинарные кавычки, ssh, вложенность, $'…' → −1) красные до, зелёные после; checkPositions: вложенный — края совпадают; корпуса зелёные. Влитие и выпуск — после проверки nested-offset, затем своя независимая проверка
- 2026-10-30 05:51 готово — akit-master-checker (session akit-master): независимая проверка (свежий суб-агент, бриф из тикета) 05:55 на ecfa4f0: тесты, glint, graft gate зелёные; TestCorpus оба корпуса; waits ACCEPT OK; своя мера — верхний уровень cmd[BodyStart:BodyEnd]==Body у 93+1568 heredoc, вложенные: границы у 9 из 9 (на main 0), края совпадают у всех; имена вызовов как на main; пробы (форма llmark, <<-EOF, ssh, вложенность, склейки, перенос строки, $'…' → −1, пустое тело) верны; graft leaks 0. Влито 1bb0d16 (после пересадки на cdc5209). Файлы: /home/aisee/.cache/claude-tmp/nhb-review/
