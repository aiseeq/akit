# shellcmd: границы тела heredoc во вложенном скрипте в исходной команде

Статус: открыт

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
