# ТЗ akit «как сделано» + критика вторым агентом

Статус: готово
Исполнитель: akit-master

## Что сделать

## Приёмка
- accept.sh автора: разделы, ≥ 150 разных ссылок файл:строка разрешаются, покрыты все cmd/* и пакеты библиотеки, у каждого пункта источник, правок в дереве нет.
- Критик: находки с доказательством и способом проверить.
- Разбор в журнале этого тикета, проверен свежим агентом.
- Итог git-admin строкой: находок N, принято M, тикетов K; главные 1–2; время и цена автора и критика.

## Задача
- Задание лида через git-admin 31.10 ~15:40 CET: ТЗ akit «как сделано» + критика вторым агентом. Бриф: /home/aisee/docs/reverse-spec/brief.md; решение — ~/.claude/decisions.md reverse-spec-rollout. Слова пользователя: `user-word /home/aisee/.claude/projects/-home-aisee/8e927714-df71-45db-8bfd-a672b6f32c8d.jsonl 'Вся информация агентам'`, `… 'грузить терру там где можно'`.
- Образец — пилот aguard: /home/aisee/git/aguard/docs/tasks/20261031-1448-MEDIUM-spec-as-built.md.
- Отличие от пилота: ТЗ — /home/aisee/.claude/tools/akit/spec.md (приватно, рядом с design.md), не docs/spec.md: akit публичный, CLAUDE.md проекта запрещает в репо цитаты и даты обсуждений, id транскриптов, домашние пути, а ТЗ строится на словах пользователя с tl id. Коммит — в claude-config.
- Порядок: автор — боец Terra (класс документация), мера /home/aisee/.cache/worktrees/akit-accept/spec/accept.sh; критик — свежий боец Terra (класс ревью) со всем доступным, без вывода автора; Opus — только если Terra не справилась. Разбор каждой находки: принято → тикет akit, отклонено → причина; разбор проверяет свежий агент.


## Журнал
- 2026-10-31 15:34 открыт — akit-master
- 2026-10-31 15:37 note — akit-master: 15:37 CET: мера /home/aisee/.cache/worktrees/akit-accept/spec/accept.sh (на заготовке — код 1: 1 ссылка < 150, не покрыты пакеты); бриф автора /home/aisee/.cache/worktrees/akit-accept/spec/brief.md; автор — worker run terra, класс документация, дерево /home/aisee/.cache/worktrees/akit/spec-as-built (только чтение), ТЗ — /home/aisee/.claude/tools/akit/spec.md. Первый запуск оборвал мой timeout 100 до регистрации задачи (задачи нет) — перезапуск в фоне. Ноут занят (LA1 ≥ 22), бойцы стоят в очереди машины.
- 2026-10-31 17:06 note — akit-master: 17:20 CET: автор Terra 1031-1537-spec-as-built-2 — провал меры был дырой моей приёмки (правки против origin/main, ушедшего вперёд), исправлена (diff HEAD + merge-base); ТЗ 262 ссылки, приёмка ok. Выборка мастером: pressure/busy.go:50-55, cmd/wfor/main.go:54-57, edit/write.go:53-57, cmd/webtext/fetch.go:249-257, deliver/deliver.go:76-99, цитаты tl 0bbcc980:9775 и 8e927714:2422 — сходятся. Срез — 5f34ea7. Коммит claude-config 555a958. Автор: 67 мин, 0,115% недели OpenAI. Критик — свежая Terra (класс ревью), дерево /home/aisee/.cache/worktrees/akit/spec-critic на 5f34ea7, бриф /home/aisee/.cache/worktrees/akit-accept/spec/critic-brief.md, мера critic-accept.sh.
- 2026-10-31 17:06 note — akit-master: поправка: прошлая запись сделана в 17:05 CET, не 17:20 (время было написано без date). Критик — job 1031-1706-spec-critic.
- 2026-10-31 18:01 note — akit-master: критик — Terra 1031-1706-spec-critic, 16 мин, 0.047% недели OpenAI; приёмка критика ok, 21 находка (вид 1 — 4, вид 3 — 17), /home/aisee/.cache/worktrees/akit-accept/spec/critic/findings.md
- 2026-10-31 18:01 note — akit-master: разбор (/home/aisee/.cache/worktrees/akit-accept/spec/triage.md): принято 21 из 21, отклонено 0; все — правки ТЗ в тикет spec-fixes; мастер сверил по коду 4 вида 1 и 6 вида 3. Проверка разбора свежим суб-агентом (бриф triage-verify-brief.md): все 21 решение верны; FAIL на моём выводе «дефектов кода нет» — находки 4 (wfor --new и неполная строка) и 6 (OUT_DIR, содержащий home) — дефекты кода, 5 — пробел справки busy
- 2026-10-31 18:01 note — akit-master: по проверке разбора: тикеты на код wfor-new-partial, rules-export-outdir, busy-doc-freeze — сделаны (06c6c34, 991e6cb), на независимой проверке; spec-fixes — боец Luna 1031-1800-spec-fixes, приёмка fixes-accept.sh. Итог: находок 21, принято 21, тикетов 4
- 2026-10-31 18:01 note — akit-master: итог отправлен git-admin 18:02 CET
- 2026-10-31 18:01 готово — triage-verifier (session akit-master): разбор проверен свежим суб-агентом: 21 решение верно; найденные им дефекты кода вынесены в тикеты wfor-new-partial, rules-export-outdir, busy-doc-freeze; ТЗ-приёмка accept.sh ok (262 ссылки), критика ok (21)
