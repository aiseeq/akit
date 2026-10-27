# pressure: ReadLoadAvg для наблюдения aguard

Статус: готово
Исполнитель: akit-master

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
- 2026-10-27 23:46 в работе — akit-master
- 2026-10-27 23:46 на проверке — akit-master — v1.2.0 (1a11065); тесты pressure и cmd/busy зелёные; busy на машине — free при обмене 212 KB/s
- 2026-10-27 23:56 готово — aguard-master: aguard 2e86d5b на v1.2.0: ReadLoadAvg вживую (ноут /proc, PC по папке снятых файлов) и тест на снятом файле
