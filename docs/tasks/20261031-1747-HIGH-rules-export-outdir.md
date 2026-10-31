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
- 2026-10-31 19:21 note — akit-master: c3fdb7d (без выпуска до конца проверки 9 tk); проверка 6 — суб-агент, бриф scratchpad/briefs/verify-export6.md
- 2026-10-31 19:38 note — akit-master: проверка 6 — FAIL: OUT_DIR — overlay чужой папки из одних имён выгрузки; без /proc точки монтирования не видны (пустой список); тест исходника скилла не требовал «must not contain» (мутант без папок скиллов выживал); файл .git переписывался заменами. Сделано: OUT_DIR, сам являющийся точкой монтирования, — отказ; на Linux без mountinfo — ошибка; файл .git в корне не правится; тест требует «must not contain»; новые случаи overlay of export-like folder, no /proc, worktree .git; 3 мутанта красные. Замечание: на pc под graft heavy тест монтирования пропускается (нет вложенного userns), локально идёт
- 2026-10-31 19:40 note — akit-master: v1.21.12 (44d1ef4) на ноуте и PC; проверка 7 — суб-агент, бриф scratchpad/briefs/verify-export7.md
- 2026-10-31 19:55 note — akit-master: Проверка 7 FAIL: (1) strings.Fields в mountinfo делил по U+00A0/U+202F/\v — теперь parseMounts делит по " ", TestParseMounts; (2) casefold A/a писали сквозь ссылку — copyTree создаёт папки os.Mkdir, файлы O_CREATE|O_EXCL|O_NOFOLLOW, rewrite O_TRUNC|O_NOFOLLOW, TestCopyRefusesExistingPath; пробел покрытия within/SameFile — проверка within в помощнике bind-теста. Мутанты Fields, no-EXCL, no-NOFOLLOW, MkdirAll, без SameFile — красные; Symlink с IsExist-ok выживает (на регистрозависимой ФС встречу не воспроизвести, корень скилла ловится Mkdir раньше). Пробный экспорт в чистую папку: 33 файла, exit 0.
- 2026-10-31 19:57 note — akit-master: v1.21.13 (c2c6daf) на ноуте и PC; проверка 8 — суб-агент, бриф scratchpad/briefs/verify-export8.md
- 2026-10-31 20:09 note — akit-master: Проверка 8 FAIL: на casefold OUT_DIR в другом регистре (…/OUT при точке …/out) обходит checkOutContent — under сравнивает с mountinfo по тексту; очистка стёрла смонтированную чужую папку. Остальное ok (мутанты, бинарник, утечки, glint). Чиню: путь после EvalSymlinks — имя от ядра (open O_PATH + readlink /proc/self/fd/N, тот же d_path, что в mountinfo)
- 2026-10-31 20:10 note — akit-master: Фикс: resolved отдаёт kernelName (open O_RDONLY|O_NONBLOCK + readlink /proc/self/fd/N; не абсолютный или «(deleted)» — ошибка). Тест: случаи casefold mount inside OUT_DIR и casefold OUT_DIR is a mount point (tmpfs -o casefold, chattr +F, код 77 — нет casefold, лог). Мутант без kernelName краснеет. Пробный экспорт: 33 файла, exit 0
- 2026-10-31 20:11 note — akit-master: v1.21.14 (11cc1b4) на ноуте и PC; проверка 9 — суб-агент, бриф scratchpad/briefs/verify-export9.md
- 2026-10-31 20:21 note — akit-master: Проверка 9 прервана суб-агентом на полпути (не сделаны сценарии в пространствах имён, утечки, glint, уборка); пробел M1 (resolved без EvalSymlinks выживал) — тест висячей ссылки в скилле в TestExport, мутант краснеет; продолжаю того же проверяющего
