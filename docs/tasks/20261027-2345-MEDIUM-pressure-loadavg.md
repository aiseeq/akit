# pressure: ReadLoadAvg для наблюдения aguard

Статус: открыт

## Вход
- Проблема: aguard переходит на системный load average для наблюдения за длительной перегрузкой (`~/.claude/decisions.md` `## load-measure`: наблюдение — системный LA, решение «пускать ли» — моментальное число готовых задач). Своего разбора /proc/loadavg в akit нет, копии в graft, sc2go, sc2demu, llmark.
- Метод: читатель в `pressure`, как остальные меры (procRoot, сентинел).
- Дешевле: нет.
- Инструмент: `akit/pressure`.
- Цена: минуты.
- Если не делать: aguard пишет ещё одну копию разбора.

## Что сделать
1. `LoadAvg{Avg1, Avg5, Avg15 float64; Running, Total int}`, `ParseLoadAvg(s)`, `ReadLoadAvg(procRoot)`; файла нет — `ErrNoLoadAvg`.
2. Релиз akit, версия — aguard-master.

## Приёмка
- Тест: разбор строки `1.50 2.25 3.00 4/1234 5678`, кривая строка — ошибка, нет файла — `errors.Is(err, ErrNoLoadAvg)`.
- `graft gate` зелёный.

## Журнал
- 2026-10-27 23:46 открыт — akit-master: просьба aguard-master.
