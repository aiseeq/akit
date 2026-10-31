# tk: обязательный пункт плана, который нельзя пропустить через tk skip

Статус: на проверке
Исполнитель: akit-master

## Вход
- Просьба dream-master (тикет dream night-accept-fresh; решение пользователя — /home/aisee/.claude/decisions.md `## second-check` п.2: ночь Сна закрывает только свежий агент): ночь закрывали без приёмки — `tk skip dream-run-1101 accept --why …`, затем `tk done` → «готово» (runPlan → item.Skipped). Стенды dream: /home/aisee/.cache/claude-tmp/claude-1000/-home-aisee-git-dream/7ec55eed-b598-4b5f-aa2e-096523183ece/scratchpad/recheck/stands.sh (A, A2).

## Что сделать
- `tk plan ID --add 'NAME: CMD' --required`: строка журнала «добавлен обязательный пункт NAME».
- `tk skip` на обязательный — отказ, код 2, тикет не меняется.
- Пропуск, вписанный в журнал руками, и повторное добавление без флага обязательность не снимают.
- Обязательный пункт, убранный из `## План`, — `tk done` отказывает.
- Граница: правку строки журнала «обязательный» руками tk не ловит — это видит свежий проверяющий.

## Приёмка
- Чем проверить: `go test ./cmd/tk` (TestRequiredPlanItemCannotBeSkipped); мутанты (без отказа в skip; без снятия Skipped; без проверки пропажи из плана) красят тест; стенды A и A2 dream-master с `--required` — `tk skip` код 2, `tk done` без прохода пункта не закрывает.

## Журнал
- 2026-10-31 17:35 открыт — akit-master
- 2026-10-31 17:35 в работе — akit-master
- 2026-10-31 17:35 note — akit-master: 17:35 CET: тикет заведён после кода (первый tk new упал на --slug; slug — позиционный). Сделано: 3634add, 52280fe (glint complexity), выпуск v1.21.0, install ноут и PC (go version -m — v1.21.0). Независимая проверка — свежий суб-агент.
- 2026-10-31 17:35 на проверке — akit-master
