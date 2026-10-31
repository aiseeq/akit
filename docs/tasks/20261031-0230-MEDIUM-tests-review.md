# Разбор тестов akit по содержанию

Статус: в работе
Исполнитель: akit-master

## Что сделать
- Запрос dream-master 31.10 02:50 CET (обход tests Сна, первый разбор akit; итог — git-admin). Без новых прогонов набора ради чисел: числа за 7 дней — graft stats (gate 221, красных 14 %; test 210, 1 %; install 22, 9 %).
- Образец — /home/aisee/sc2/demu/bench-data/archive/tests-review-1030/report.md (llmark).
- 1. По каждому fix-коммиту с 01.10 (15 шт.: cea1750 33cd81f edaf16c 019d978 e5eba45 cdc5209 885b465 c0b8bd3 b6d1e62 e9ff47d b2bc05c 11289f5 56700be 5730666 1e78d57): ловили ли тесты дефект до фикса (красная запись в ~/.cache/graft/stats.jsonl или тест в самом фиксе) и ловит ли откат кода фикса сейчас (откат без тестов, в отдельном worktree).
- 2. Пустые тесты и тесты, проверяющие мок.
- 3. Внешнее: живые транскрипты ~/.claude/projects, /proc машины, настоящие сессии и сокеты, сеть — или фикстуры. Особо transcript, session, deliver, pressure, price.
- 4. Покрытие (go tool cover -func) против мест багов месяца; разбор команды и файла — утилиты зовут все агенты.
- 5. time.Sleep и зависимость от часов.
- Поправки — в коде, с тестом, отдельными коммитами по dev-process.

## Приёмка
- Отчёт docs/tests-review/20261031.md (или scratchpad, если репо не место — решить по repo-docs): строка на каждый из 15 fix-коммитов с «ловил / ловит откат / тест / довод»; разделы 2–5 с путями file:line.
- Выборочно мастер сам повторяет откат 3 коммитов из таблицы — результат совпал.
- Каждая поправка: тест красный до, зелёный после; graft commit зелёный.
- Итог — git-admin через SendMessage.

## Журнал
- 2026-10-31 02:30 открыт — akit-master
- 2026-10-31 02:33 в работе — akit-master
- 2026-10-31 02:33 note — akit-master: боец Luna 1031-0233-tests-review (разбор-данных): бриф /home/aisee/.cache/worktrees/akit-accept/tests-review-brief.md, приёмка tests-review.sh там же, отчёт → /home/aisee/.claude/improve/state/tests-review/akit-20261031.md; поправки — вторым этапом после ревью
- 2026-10-31 02:59 note — akit-master: отчёт бойца принят с поправками мастера (в самом отчёте: «ловил» — TDD-тест фикса, а не stats; правило 6 через unshare -rm; govulncheck в гейт снят). Мастер повторил: тест cdc5209 на cdc5209^ — TestCalls FAIL; откаты cdc5209/11289f5 на HEAD — TestCalls/TestSubagentsAndRawFilter FAIL. Поправки 1–4 (тесты tk, codex, shellcmd) — боец 1031-03xx-tests-fixes, приёмка tests-fixes.sh (10 функций ≥ 80 %, только тесты). Не делаю: 6 (пороги 300 мс/2 с — красных test за 7 дней 0, флейков нет), 8 (ForHome — однострочник), 9 (page-shot с Chrome — проверка по наличию браузера, есть Skip)
