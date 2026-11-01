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
