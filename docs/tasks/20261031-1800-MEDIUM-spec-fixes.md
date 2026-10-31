# ТЗ akit: 21 правка по находкам критика

Статус: готово
Исполнитель: akit-master

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
- 2026-10-31 18:33 note — akit-master: ревью мастера: выборочно по коду 5f34ea7 (busy --quiet-until, IO при открытом зависании, tk 4096 байт stderr) — совпадает; ТЗ закоммичено в claude-config
- 2026-10-31 18:33 note — akit-master: независимая проверка — суб-агент, бриф /home/aisee/.cache/claude-tmp/claude-1000/-home-aisee-git-akit/45f17164-e30c-46e6-a55f-54365795eb5d/scratchpad/briefs/verify-spec-fixes.md
- 2026-10-31 18:37 note — akit-master: проверка 1 — FAIL: бэктик в стр. 74, OUT_DIR внутри home в стр. 21, RateFor в стр. 104, предел CPU в стр. 56; исправлено мастером, claude-config ниже; повторная проверка — суб-агент
- 2026-10-31 18:38 готово — суб-агент-проверяющий (session akit-master): проверка 2 (бриф scratchpad/briefs/verify-spec-fixes2.md): ok — fixes-accept.sh код 0 (ссылок 276, находок 21), строки 21/56/74/104 сверены с кодом 5f34ea7 и 06c6c34, бэктики чётные; проверка 1 сверила все 21 находку (17 верно, 4 исправлены); ТЗ в claude-config 296a30c
