# tl --grep: отсев сырых строк без ToLower и по литералам регулярки

Статус: в работе
Исполнитель: akit-master

## Что сделать
- Запрос git-admin 31.10 (обход perf Сна, кандидаты 1 и 2 из /home/aisee/.claude/improve/state/perf/akit.md).
- 1. cmd/tl/scan.go keepRaw (:71 на b4a92ac): регистронезависимый поиск литерала без `bytes.ToLower(raw)` каждой строки. Отсев — надмножество `(?i)`-совпадений Go regexp (простая свёртка Unicode: k↔K U+212A, s↔ſ U+017F и т. п.).
- 2. Отсев сырых строк и для регулярок: набор обязательных литералов из дерева regexp/syntax (альтернатива литералов — любой из них; конкатенация — обязательный кусок). Без литерала (`.`, `\w+`) — как сейчас.
- Кандидат 3 (prior ← daydigest) закрыт в dream db4c1c3 — не трогать.

## Приёмка
- Тест эквивалентности (fuzz или таблица с Unicode-свёрткой): строка, где regexp `(?i)PAT` находит совпадение, отсевом не отбрасывается.
- Вывод `tl --all --since '24.10 00:00' --until '31.10 02:00' --grep P` нового tl байт в байт равен установленному (cea1750) для P из набора приёмки бойца.
- Замер до/после (wall, CPU-с) `--grep wfor` и `--grep 'wfor|wfoz'` под graft exec --heavy; новые числа — в /home/aisee/.claude/improve/state/perf/akit.tsv и akit.md, коммит в claude-config.
- graft commit зелёный, graft install. Итог — git-admin: тикет, коммит, числа.

## Журнал
- 2026-10-31 02:31 открыт — akit-master
- 2026-10-31 02:47 в работе — akit-master
- 2026-10-31 02:47 note — akit-master: боец Luna (правка-инструмента), бриф/приёмка /home/aisee/.cache/worktrees/akit-accept/grep-prefilter{-brief.md,.sh}; санити приёмки на чистом дереве оборвалась по 15 мин в очереди graft (ночь), детерминизм вывода эталона проверен отдельно: 2 прогона на 6 ч окна совпали
- 2026-10-31 04:18 note — akit-master: приёмка: базовый tl теряет строки на --grep ſ (ToLower-отсев; (?i)ſ совпадает с s, верно 117 событий) — эталон для ſ: base --grep "[ſ\x{10FFFF}]" ([ſ] regexp/syntax упрощает в литерал); grep-prefilter.sh исправлен
- 2026-10-31 04:45 note — akit-master: ревью захода 3 бойца (приёмка байт в байт прошла, коммита нет): отсев верен, но свёрточный поиск медленнее старого на свёрточных шаблонах — 7 суток CPU-с main→дерево: wfor 53→17, wfor|wfoz 88→24, sub -n 51→373, Kelvin 55→347, ожидани 59→212 (IndexByte всех вариантов первой руны на каждом шаге; кандидат по первому байту 0xD0). Приёмка дополнена grep-prefilter-perf.sh (CPU ≤ main×1,1, wfor ≤ ×0,5; эталон tl-main = 3838685); раунд доводки — бриф grep-prefilter-review1.md
- 2026-10-31 10:34 note — akit-master: независимая проверка (суб-агент, 10:11–10:31): отсев 7492d7f теряет события — регулярка ищет по Who+" "+Shown(), а это сборный текст (имя инструмента/отправителя + пробел, [image], context compacted, response without text, → кому:, ←, склейка краёв после вырезанного system-reminder, \f \b \u00XX, одиночный суррогат → U+FFFD); `tl --all --role cmd --grep "^Bash graft"` за 30.10: main 648 строк, v1.15.3 — no events. Откат 68ac1a7, выпуск, установлено, 648 строк снова. Приёмка ложно падала на строках graft: в stderr нового tl (сравнение 2>&1) — переделать под одну очередь. Дальше: отсев только по тому, что гарантированно лежит в сырой строке подряд; fuzz через запись JSONL и transcript.Read
- 2026-10-31 11:06 note — akit-master: приёмка v2 (grep-prefilter2.sh: эталон — tl-main с шаблоном (?:P)|\x{10FFFF}, т.е. без отсева; 30 запросов, --role all --codex, 28.10–31.10, слот очереди на запрос) на main со старым отсевом: потери — Bash git 28 из 2287 строк, \[image\] 202/1005, context compacted 27/869, Read /home 196/2214, ſ 0/230273; при равном числе строк у результатов пропадает Who (строка вызова отброшена → нет имени инструмента). Лечение до починки: bd7bca7 — --grep сырые строки не отсеивает (тест TestGrepFindsComposedText красный → зелёный), v1.15.5, установлено; Bash git 2266 строк. Цена: wfor 53 → 81 CPU-с, 15 → 22 с wall. Починка — боец Luna 1031-1101-grep-prefilter2 (бриф akit-accept/grep-prefilter2-brief.md)
- 2026-10-31 13:45 note — akit-master: 13:45 CET: боец 1031-1101-grep-prefilter2 — 163 мин, 1 заход; 30 запросов байт в байт проходят каждый раз, скорость нет: wfor 21.7→28.4 CPU-с (цель 0.5× main), sub -n 21.3→30.3, Kelvin 17.8→30.9 (предел 1.1×); ожидани, graft, Ss — ok. Срок: 14:30 CET — если приёмка не прошла, плавная остановка и эскалация (job: escalate opus; маршрут сильной — Terra).
- 2026-10-31 14:35 note — akit-master: 14:31 CET: срок вышел, приёмка не прошла (скорость) — worker stop 1031-1101-grep-prefilter2 (210 мин, снята); дифф Luna — /home/aisee/.cache/worktrees/akit-accept/grep-prefilter2-luna.patch. Эскалация по маршруту сильной: Terra, дерево /home/aisee/.cache/worktrees/akit/grep-prefilter-terra от main 24e1d8a + код 7492d7f (конфликт с wait_cache разрешён в пользу отсева, TestGrepFindsComposedText красный), бриф /home/aisee/.cache/worktrees/akit-accept/grep-prefilter-terra-brief.md, приёмка та же, --escalate opus.
