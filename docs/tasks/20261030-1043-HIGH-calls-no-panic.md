# shellcmd: Calls без паники на любом входе

Статус: готово
Исполнитель: akit-master

## Вход
- dream-master 30.10: shellcmd.Calls (v1.13.2) паникует; dreamstats -tool-use и сборка страницы Сна падают (waits.CommandHow → Calls). Входы: /home/aisee/.cache/claude-tmp/claude-1000/-home-aisee-git-dream/d20effec-ff3a-4791-83eb-e9a6049dbeb2/scratchpad/shellpanic/case-1234.txt, case-2250.txt.
- (1) slice bounds out of range [1247:1234], shellcmd.go:107: у недочитанного heredoc BodyStart за концом src (ограничитель дописан parseShell). (2) nil pointer в timedCallSet: проза с `time` в обратных кавычках — TimeClause без Stmt.

## Что сделать
- Calls и Find не паникуют ни на каком входе: ошибка разбора или вызовы.
- Границы тела heredoc обрезать по src с обеих сторон; TimeClause без Stmt пропускать.
- Входы dream — в тесты (синтетикой той же формы, без внутренних путей); fuzz-тест FuzzCalls с затравками; прогнать fuzz.

## Приёмка
- Чем проверить: красный до, зелёный после TestCallsNoPanic; `go test -run ^$ -fuzz FuzzCalls -fuzztime 5m ./shellcmd/` без находок; repro dream по обоим файлам без паники; go test ./shellcmd/ ./transcript/...; glint 0; имена вызовов по обоим корпусам без изменений.

## Журнал
- 2026-10-30 10:43 открыт — akit-master
- 2026-10-30 10:43 в работе — akit-master
- 2026-10-30 10:50 note — akit-master: Сделано: timedCallSet пропускает TimeClause без Stmt; parse обрезает следы дописанного ограничителя и при NestedError (раньше возврат шёл до обрезки): trimAppended — вызовы из дописанного текста (после `\` в конце) выпадают, тело за концом src → -1/-1, частично → до len(src). TestCallsNoPanic (11 входов, красный до), FuzzCalls (затравки + 2 находки fuzz в testdata/fuzz: `sh<<''\n!`, `<<0 \`). Fuzz 4 мин: 5,67 млн входов без находок. Входы dream case-1234/2250 и проза — без паники. Корпуса v1.13.2 → дерево: 0 строк с разницей. glint 0.
- 2026-10-30 10:52 на проверке — akit-master
- 2026-10-30 10:54 готово — akit-master-checker (session akit-master): независимая проверка: TestCallsNoPanic на HEAD~1 — panic [15:12], после — ok; go test ./shellcmd/ ./transcript/... ok, glint 0; case-1234/2250 потребителя — 8 и 9 вызовов без паники; свой fuzz 2 мин 1,54 млн входов без находок; trimAppended не теряет вызовы (cat <<E\nx, bash <<E\npgrep x, вложенные bash -c/ssh/heredoc — совпадают со старым кодом). Остаток: ограничитель после `\` в конце попадает в Args — отдельный тикет.
