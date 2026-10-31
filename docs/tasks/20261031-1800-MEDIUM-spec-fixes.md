# ТЗ akit: 21 правка по находкам критика

Статус: открыт

## Вход
- spec-as-built: критик нашёл 21 расхождение ТЗ /home/aisee/.claude/tools/akit/spec.md с кодом на 5f34ea7 (/home/aisee/.cache/worktrees/akit-accept/spec/critic/findings.md), разбор — triage.md рядом, все 21 приняты, разбор сверен независимо.
- Дефекты кода из находок 4, 5, 6 — отдельные тикеты wfor-new-partial, busy-doc-freeze, rules-export-outdir (06c6c34, 991e6cb).

## Что сделать
- Боец Luna (класс документация): внести 21 правку в ТЗ со ссылками на код 5f34ea7; у 4, 5, 6 — пометка об исправлении после среза коммитом.
- Ревью мастера по коду, независимая проверка, коммит ТЗ в claude-config.

## Приёмка
- `bash /home/aisee/.cache/worktrees/akit-accept/spec/fixes-accept.sh` в /home/aisee/.cache/worktrees/akit/spec-fixes (5f34ea7) — код 0; до правок FAIL (8 ссылок, находки 1 и 3, нет пометок).

## Журнал
- 2026-10-31 18:00 открыт — akit-master
