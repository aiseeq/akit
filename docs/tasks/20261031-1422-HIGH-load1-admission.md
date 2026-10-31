# Допуск тяжёлого: и моментальный runnable, и LA за минуту ниже потоков

Статус: готово
Исполнитель: akit-master

## Вход
- Задание лида 31.10 ~14:25 CET (HIGH). Пользователь 31.10 14:19 CET (tl 8e927714:2422): «новый процесс можно запускать только если минутный la и моментальный la ниже количества виртуальных ядер». decisions.md load-measure поправлена.
- Факт: pressure.LoadAvg.Avg1 разбирается (busy.go), но Check его не читает; README busy советует не смотреть на минутный LA.

## Что сделать
- pressure.Check: занята, если runnable сейчас ≥ лимита потоков ИЛИ Avg1 ≥ того же лимита (один порог на машину). Строка busy и причина ожидания — с LA1.
- README busy и design.md — новое правило.
- Выпуск, установка на ноут и PC; graft (FIFO), busy и допуск бойцов worker берут новое akit — проверить на обеих машинах.

## Приёмка
- Тест на подменном /proc: Avg1 ≥ потоков при runnable ниже → занята; оба ниже → свободна; runnable ≥ → занята.
- Проба под нагрузкой на живой машине: busy и graft exec --heavy --local ждут при Avg1 ≥ лимита и моментальном ниже, пускают при обоих ниже (подменным лимитом в pressure.yaml).
- Версии akit в busy, graft и worker на ноуте и PC — с правкой (go version -m).
- Независимая проверка; итог лиду строкой.

## Журнал
- 2026-10-31 14:22 открыт — akit-master
- 2026-10-31 14:27 note — akit-master: правка: pressure.Rule — CPU и при Avg1 ≥ порога runnable (та же мера CPU: graft сопоставляет её со своим «pressure.yaml runnable», править graft не нужно — только поднять модуль akit); Why называет, какое число сработало; строка busy — «load average 1m X (busy at N)»; README, busy -h, design.md. Тесты на подменном /proc (LA 6 при пороге 5 и моментальном 3 → занята; 4.99 → свободна), мутант красный, glint чист.
- 2026-10-31 14:35 note — akit-master: 14:35 CET: aguard cd1925a на v1.19.0, демон перезапущен 14:31:16 (пауза диска по LA1 им подходит); worker eaa9320 на v1.19.0 на ноуте и PC (pressureWhy → v.Why(), тест TestAdmitBusyByLoadAverage). Ждём graft.
- 2026-10-31 14:40 note — akit-master: противник замысла (лид: /home/aisee/.cache/claude-tmp/claude-1000/-home-aisee/8e927714-df71-45db-8bfd-a672b6f32c8d/scratchpad/adversary-probe/answer.md; свой Terra: /home/aisee/.cache/claude-tmp/claude-1000/-home-aisee-git-akit/45f17164-e30c-46e6-a55f-54365795eb5d/scratchpad/la1-adversary-answer.md). pressure, тесты TestRule pressure/busy_test.go: №4 сон потоков в снимке → :55 (LA1 ≥ порога при моментальном 10 — CPU); №5 давление памяти без обмена → :48 свободна — так решено замером тикета busy-memory-rule (все some ≥ 10 шли с обменом ≥ 2,8 МБ/с); №6 нет PSI → :51, :53 мера пропущена, busy пишет not checked; №8 короткий всплеск → :37 одно чтение procs_running (load-measure 27.10: «актуальное»). Свой Terra: хвост LA после всплеска (~25 с при 32→22) и задачи D-состояния в LA — цена правила пользователя «минутный la … ниже», не баг; гонка очереди и бойца, лавина FIFO, размер запускаемой работы — очередь graft → graft-master с №1, №2 лида.
- 2026-10-31 15:01 note — akit-master: 15:02 CET: graft v2.68.0 (cc69e6c) на akit v1.19.0 на ноуте и PC (go version -m, h1:Dd2hpj… на обоих). Проба ноут 14:57: pressure.yaml runnable 14 при моментальном 8–11 и LA1 16 → «graft: exec: load average 1m 16.13 is at or above 14 (pressure.yaml runnable), waiting», timeout 40 c → 124; runnable 60 → пустил, PROBE-RAN при LA1 13.06, код 0. PC 15:01: runnable 100 → PROBE-RAN, код 0 (LA-only на PC не поймать: моментальное 46–73 выше LA1 51). Замечание: XDG_RUNTIME_DIR не отделяет load.lock — проба держала общую очередь до 40 с. graft-master по противнику: закрыты лавина/гонка (settle, TestCalmMachineStartsOneAtATime), открыты A (threads задачи) и B (поздняя нагрузка) — выбор лида. Дальше: независимая проверка.
- 2026-10-31 15:04 note — akit-master: 15:04 CET: лид по очереди graft (отправил graft-master сам): A не делать — цена правила пользователя; B — адаптивный settle 10–30 с по CPU дерева задачи; C оставить. Это работа graft, не этого тикета. Ждём независимую проверку.
- 2026-10-31 15:08 готово — akit-verifier (session akit-master): независимая проверка 15:01–15:08 CET (суб-агент, свежий контекст): go test ./pressure ./cmd/busy ok; мутанты Avg1 >= → > и удаление ветки Avg1 — красные TestRule (LA на пороге, свой порог), TestCheckLoadAvg; Why и строка busy — LA1 и runnable вживую; go version -m busy, graft v2.68.0, worker, aguard на ноуте и PC — akit v1.19.0 h1:Dd2hpj…, vcs.modified=false; проба ноут: порог 9 при LA1 9.59 и моментальном ниже — graft «load average 1m 9.59 is at or above 9 (pressure.yaml runnable), waiting», busy порог 12 — «busy: load average 1m 12.52 is at or above 12», код 1; порог 60 — оба пускают, код 0; утечек в коде нет (тикеты — по public-repo-names). verdict: ok
