# Сон 01.11, software: недельный замер подхвата sub, tl, wfor

Статус: открыт

## Вход
- Задание Сна (dream-master, обход software 01.11): недельный замер подхвата утилит akit — /home/aisee/.claude/improve/state/backlog.md, строки про замер sub (ждёт 01.11) и «Возвращено 29.10»; способ счёта — /home/aisee/.claude/tools/akit/design.md, «Как агенты узнают об утилитах», «Как считать». Отчёт — раздел в /home/aisee/.claude/improve/runs/2026-11-01/round-software.md, итог — git-admin. Тикет perf-median-cmd не дублировать.

## Что сделать
- sub: вызовы, исходы (успех, промах с -n, ошибка), причины промахов, привычные правки (sed -i, perl -pi, .replace) у Claude и бойцов codex за неделю.
- tl: вызовы против grep/rg/jq по *.jsonl и user-word; решить user-word → tl по backlog.
- wfor: Luna против Terra/Sol, у Claude — циклы sleep.
- Выводы на примерах; «не подхватили» — почему; сверка условий строки «утилиты akit внедрены и доказали пользу».

## Приёмка
- Числа воспроизводимы командами из отчёта; выборка из 5 исходов sub сверена с транскриптами мастером.

## Журнал
- 2026-11-01 02:57 открыт — akit-master
- 2026-11-01 03:03 note — akit-master: Боец Luna 1101-0303-adoption-week (класс разбор-данных, маршрут luna-мера), бриф /home/aisee/.cache/worktrees/akit-accept/adoption-week/brief.md, приёмка /home/aisee/.cache/worktrees/akit-accept/adoption-accept.sh (файлы, шапки, run.sh воспроизводит counts.tsv). Дальше мастер: сверка 5 примеров examples.tsv с транскриптами, пересчёт пары чисел своим tl, раздел в round-software.md, решение user-word→tl, итог git-admin.
- 2026-11-01 03:12 note — akit-master: Вопрос лида 01.11 (срок 18:00 CET): выполнено ли условие backlog.md:22 (слово пользователя 25.10, tl 0bbcc980:7759). Нужна строка на КАЖДУЮ внедрённую утилиту akit (не только sub/tl/wfor): (1) вызовы за день и неделю, доля от старого способа; (2) замер экономии токенов или снижения ошибок — есть или «замера нет»; (3) свои сбои за неделю. Итог «выполнено / не хватает X». План: после сдачи Luna — worker continue с расширением на busy, prior, tk, user-word, page-shot, webtext, claude-rules-export (вызовы, коды выхода, падения), польза — из замеров llmark (sub-pitch, tl-pitch, wfor-pitch) и этого замера.
- 2026-11-01 04:50 note — akit-master: 04:52 CET. Раунд 1 принят мастером: расхождение со сверкой через tl (4 623 события против 13 866 вызовов) — ошибка моей сверки, grep не ловил `cmd:"sub` в codex и считал события, а не вызовы; проба на одном rollout (processscan-spec-gaps): инструмент 188 вызовов, tl 154 события, в сырых входах 197 вхождений. Раунд 2 (brief-2.md, все утилиты, причина unknown у tl) — worker continue, приёмка /home/aisee/.cache/worktrees/akit-accept/adoption-accept-2.sh.
- 2026-11-01 05:30 note — akit-master: 05:30 CET. Раунд 2 Luna: «не справился» из-за моей приёмки (awk | grep -q под pipefail → SIGPIPE на большом counts.tsv; Luna нашла сама), исправлено в adoption-accept.sh и -2.sh, с исправленной приёмка проходит. Ревью: графа «свои сбои» неверна — toolFault берёт вывод и код всей составной команды (xargs signal 13, panic из go test после sub, panic в тексте найденного tl события засчитаны падениями sub/tl). Раунд 3 (brief-3.md: crash/killed/call-error/refusal по собственному выводу утилиты, сверка руками, последняя строка «Условие …»), приёмка adoption-accept-3.sh (красная на сдаче 2; проверка crash-* проверена на синтетике: ложное ловит, трасса akit и «tk: fatal error» проходят).
