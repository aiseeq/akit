# tl --usage: итог называет охват журналов

Статус: готово
Исполнитель: akit-master

## Что сделать
От llmark-master (сверка курса, тикет llmark spend-reconcile): `tl --all --usage` не видит запуски `claude -p --no-session-persistence` — у них нет журнала в ~/.claude/projects (кейсы Сна пишут stream-json в ~/.claude/improve/runs/<дата>/). Окно 31.10 09:00 → 01.11 04:11 CET: tl $885,6, dreamstats $973,3, разница $87,7; лид получил курс ~$2200 вместо 2474.

Что сделать:
- итоговая строка `tl … --usage` называет охват: какие журналы прочитаны (папка проектов, rollout codex при --codex) и что запуски без журнала не входят;
- то же в `-h` у `--usage`.

Чем проверить:
- `tl --all --since 1h --usage | tail -1` — строка с охватом;
- тест на строку итога в cmd/tl/usage_test.go.

## Приёмка
- `tl … --usage | tail -1` называет прочитанные журналы и что не входит (запуски без журнала, rollout codex) — в любом окне, в том числе пустом, с `--project`, `--no-subagents`, `--codex`, для файла и сессии с суб-агентами;
- числа таблицы `--usage` те же, что у базы f93caf4, на том же окне;
- `-h` и README говорят об охвате; в коде, тестах, README и сообщениях коммитов нет внутреннего;
- тесты TestUsage, TestUsageScopeAll и TestUsageScopeCodexRollout в cmd/tl/usage_test.go зелёные.

## Журнал
- 2026-11-01 04:50 открыт — akit-master
- 2026-11-01 04:52 note — akit-master: 04:58 CET. Коммит 651c68e, выпуск v1.22.8, установлено. Проба: `tl --all --since 30m --usage | tail -1` → «…; read: /home/aisee/.claude/projects; runs without a transcript (claude -p --no-session-persistence) not counted». Независимая проверка — суб-агент, бриф scratchpad/briefs/verify-usage-scope.md.
- 2026-11-01 04:52 note — akit-master: Поправка времени прошлой записи: она сделана в 04:52 CET (по date), не в 04:58.
- 2026-11-01 04:59 note — akit-master: Проверка 1 (суб-агент): FAIL — нет охвата при нуле ответов; --all --codex называл rollout прочитанными, а их расход не входит; --project и --no-subagents не отражены; строка с 57 путями; README без пустой строки. П.4 (docs/tasks с именами сессий в публичном репо) не дефект по decisions public-repo-names (28.10): утечка — только имена и данные рабочих проектов. Правка: охват в обеих ветках, папка с маской --project, «without subagents», codex — в «not counted», суб-агенты числом; тесты TestUsageScopeAll (5 случаев), TestUsageScopeCodexRollout; мутант без охвата в пустой ветке пойман.
- 2026-11-01 05:03 готово — subagent-afc28ef3 (session akit-master): Проверка 2 суб-агента (свежий контекст, бриф scratchpad/briefs/verify-usage-scope.md) на 451837b v1.22.9: verdict ok — 15 случаев, таблица и коды как в базе f93caf4, охват в пустом окне, --project, --no-subagents, --codex, 340 байт вместо 7 477 со 56 суб-агентами, утечек нет. Вне меры: неверная причина про codex (у token_count есть last_token_usage) — исправлено в cab8690 v1.22.10, тесты зелёные, проба `tl --all --codex --since 5m --usage | tail -1` → «… and codex rollouts (their token counts are not read)».
