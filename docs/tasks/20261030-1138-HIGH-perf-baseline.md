# Первый референс скорости akit (perf-baseline-1030, пилот)

Статус: готово
Исполнитель: akit-master

## Вход
- git-admin 30.10 ~11:50 CET, этап 1 задания лида perf-baseline-1030 (~/docs/tasks/*perf-baseline-1030*.md), тикет git-admin /home/aisee/git/docs/tasks/20261030-1137-HIGH-perf-baseline-git.md; akit — пилот. Слово пользователя: user-word ~/.claude/projects/-home-aisee/d753e3a8-1750-4371-bdef-a4263a5bf0f7.jsonl 'собирать статистику работы всех' (30.10 11:34:56).
- Формат: /home/aisee/.claude/improve/state/perf/FORMAT.md.

## Что сделать
- Проход akit по скиллу go-performance: какие утилиты агенты зовут чаще и дольше ждут (по транскриптам), первый референс их главных путей.
- /home/aisee/.claude/improve/state/perf/akit.tsv и akit.md строго по FORMAT.md; команды tsv — одно число, только чтение, ≤60 с, тяжёлое через graft exec --heavy --local.
- Кандидаты ×2 — только назвать с замером. Коммит файлов в claude-config (git --git-dir=$HOME/.claude-config.git --work-tree=$HOME, add явными путями).
- Исполнители — бойцы Terra/Luna; тяжёлое — graft exec --heavy.

## Приёмка
- Чем проверить: шапка tsv ровно по FORMAT.md, нет пустых полей; каждая `команда` ≠ `-` печатает одно число и код 0 (или 3) за ≤60 с; md с разделами «Уже меряется», «Главные пути», «Пробелы», «Кандидаты ×2»; коммит в claude-config; итог git-admin.

## Журнал
- 2026-10-30 11:38 открыт — akit-master
- 2026-10-30 11:38 в работе — akit-master
- 2026-10-30 11:53 note — akit-master: Разбор транскриптов за 7 дн (/home/aisee/.cache/claude-tmp/perf-usage/main.go): 112631 команд; вызовов/p50/p95 одиночных: tl 3582/433/10093 мс (1,0 ч ожидания), sub 5057/78/253, tk 3399/72/485, wfor 501 (ждёт по назначению), prior 351/1022/7227, user-word 390/306/1452, page-shot 414/2074/12635, busy 301/1068/1348 (окно замера 1 с по замыслу). Мера для tsv — новый режим `tl --wait PROG [SUB] --q N` (одно число мс по транскриптам за окно, <20 — код 3): боец Luna 1030-1153-tl-wait, класс фича-инструмента, бриф /home/aisee/.cache/worktrees/akit/tl-wait-brief.md.
- 2026-10-30 12:54 note — akit-master: Профиль (perf, плоский): tl --all --grep литерал — 76 % CPU в bytes.ToLower строки (scan.go:68); регулярка wfor|wfoz — ~70 % encoding/json (нет отсева, 57 % шаблонов агентов — регулярки); prior — 77 % wall в daydigest -log (dream, тикет dream 20261030-1253-MEDIUM-daydigest-log-offset). --wait держал все события (RSS 5 ГБ за 7 дн) → тикет wait-memory. Референсы 7 дн до 30.10 12:50 — /home/aisee/.cache/claude-tmp/perf-usage/refs.out. Черновики /home/aisee/.claude/improve/state/perf/akit.tsv и akit.md; ждут wall grep под graft exec --heavy.
- 2026-10-30 13:42 note — akit-master: Файлы в claude-config ee0e93f: /home/aisee/.claude/improve/state/perf/akit.tsv (10 путей, у всех команда), akit.md. Референс по FORMAT.md от 13:25 — худшее суточное за 7 сут (daily.sh, /home/aisee/.cache/claude-tmp/perf-usage/daily.out); grep-строки без graft exec --heavy (ночной шаг dream не дождался очереди за 120 с); page-shot убран в Пробелы (нет суток с 20 событиями). Находка prior → dream-master, тикет dream 20261030-1253-MEDIUM-daydigest-log-offset.
- 2026-10-30 13:42 на проверке — akit-master
- 2026-10-30 13:53 готово — akit-master-checker (session akit-master): независимая проверка 13:43–13:48: akit.tsv — шапка побайтно по FORMAT.md, 10 строк по 8 полей без пустых, вид/мера/ждёт из списков; все 10 команд из ~ через bash -c: одно число, код 0, 5,5–24,2 с (ветка кода 3 проверена на 23.10); референс cli:sub 326 и cli:tk note 637 = максимум суточных окон 23–29.10, совпадает с md; md — шапка и 4 раздела, у 3 кандидатов путь, замер, выигрыш, почему без потерь, доля; scan.go keepRaw/bytes.ToLower и dream log.go подтверждены; файлы = claude-config ee0e93f (+9ad290d ссылки строк по замечанию)
