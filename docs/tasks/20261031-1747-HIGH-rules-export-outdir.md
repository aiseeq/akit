# claude-rules-export: OUT_DIR, содержащий источник, отклоняется до очистки

Статус: на проверке
Исполнитель: akit-master

## Вход
- Находка 6 критика ТЗ: claude-rules-export отклоняет OUT_DIR только равный `/` или исходному home, cmd/claude-rules-export/export.go:26. OUT_DIR — предок исходного home или папки правил (`--rules`) — очищается (всё, кроме .git, cmd/claude-rules-export/files.go:24) и сносит источник.

## Что сделать
- Тест (красный): OUT_DIR — родитель `--src-home` → ошибка до очистки, файлы источника целы; то же для родителя `--rules` и для OUT_DIR = `--rules`.
- Фикс: отклонять OUT_DIR, равный источнику или папке правил или содержащий их; справку `-h` дополнить.

## Приёмка
- Новые тесты красные до фикса, зелёные после; `go test ./cmd/claude-rules-export` зелёный.

## Журнал
- 2026-10-31 17:47 открыт — akit-master
- 2026-10-31 17:56 note — akit-master: сделано в 06c6c34, выпуск v1.21.2 (ноут и PC); тест красный до фикса (кроме busy — справка), go test пакета зелёный
- 2026-10-31 17:56 на проверке — akit-master
- 2026-10-31 18:06 note — akit-master: проверка (суб-агент verify-three): FAIL — OUT_DIR или --src-home через симлинк (на home, его родителя, --rules, .claude, путь через ссылку) и OUT_DIR с исходником скилла-ссылки проходили и стирали источник; .., ./, относительные — ok. Фикс: checkOut сравнивает пути после EvalSymlinks (у несуществующего — по ближайшему предку), читаемое включает папку каждого скилла; 7 случаев в TestExportErrors красные до фикса
- 2026-10-31 18:19 note — akit-master: повторная проверка (суб-агент verify-two): FAIL — весь список брифа ok, но цели ссылок у читаемых файлов и внутри скиллов не проверялись: .claude/CLAUDE.md, work/CLAUDE.md, rules/manifest (выгрузка с rc=0!), README.template.md, ссылка внутри скилла. Фикс: readPaths — оба CLAUDE.md, манифест и его файлы, шаблон, ссылки внутри каждого скилла, всё по разрешённому пути; попутно скилл-ссылка выгружается содержимым (была висячей ссылкой). TestExportRefusesLinkedReads (5) и TestExportLinkedSkillCopiesContent красные на прежнем коде
- 2026-10-31 18:40 note — akit-master: проверка 3 — FAIL: OUT_DIR через bind mount поверх home проходит и стирает источник (unshare -rm, mount --bind); замечание — Abs/Join убирают «..» по тексту, ядро — после ссылки. Делаю: пути входа разрешаю физически без Clean и использую их для чтения и очистки; «внутри» — ещё и по os.SameFile предков; тест bind mount через unshare -rm (skip без userns)
- 2026-10-31 18:48 note — akit-master: 07e104f, v1.21.8 на ноуте и PC; проверка 4 — суб-агент, бриф scratchpad/briefs/verify-export4.md
- 2026-10-31 19:02 note — akit-master: проверка 4 — FAIL: bind mount подпапки читаемого (.claude/hooks, skills/secret, rules/extra) под другим путём проходил; TestExportRefusesBindMount красный на pc (вложенный unshare ENOSPC). Сделано: пути OUT_DIR и читаемого сверяются со всеми их псевдонимами из /proc/self/mountinfo (mounts.go), пропуск теста — по пробному unshare -rm true, пустые OUT_DIR/--src-home/--rules отвергаются; мутант без псевдонимов краснеет. Инцидент проверки 4: проверяющий очистил основное дерево через OUT_DIR=/proc/$$/cwd и восстановил; сверено — коммиты и правки целы
- 2026-10-31 19:03 note — akit-master: e5280d9 (выпуск — после проверки 8 tk, чтобы не сменить версию под ней); проверка 5 — суб-агент, бриф scratchpad/briefs/verify-export5.md, с запретом OUT_DIR вне своей папки
- 2026-10-31 19:19 note — akit-master: проверка 5 — FAIL: bind mount внутри OUT_DIR, overlay с источником в верхнем слое, --src-home=/proc/<pid>/cwd чужого пространства имён. Сверка путей бесконечна — смена подхода: очищается только то, что пишет выгрузка: OUT_DIR с чем-либо кроме .git, global, README.md, skills, work или с точкой монтирования внутри — отказ до очистки (сверка путей остаётся первой, с точным сообщением). Настоящая /home/aisee/work/claude-rules — ровно этот набор. Тесты: mount inside OUT_DIR, overlay upper, TestExportRefusesForeignContent; мутант без проверки содержимого краснеет
