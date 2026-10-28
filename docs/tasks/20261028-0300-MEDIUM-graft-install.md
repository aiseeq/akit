# Установка утилит задачей graft install вместо tools/install.sh

Статус: на проверке
Исполнитель: akit-master

## Вход
- Задание git-admin 27.10, пункт D; решение пользователя `tl 2dc3ff44:2495`. akit — пилот, итог git-admin.
- Форма: graft v2.45.0, шаг `{install: paths}` (~/git/graft/README.md, «Tasks»): ставит только закоммиченное, атомарно, печатает `graft: install: <имя> <rev7> -> <путь>`. bin_dir пользователя — ~/bin.
- Сейчас в ~/bin утилиты akit разных версий (v1.0.0…v1.5.1).

## Что сделать
1. `.graft.yaml`: задача `build` — `go build -o bin/ ./cmd/...`; задача `install` — `{deps: [build], run: [{install: bin/*}]}`.
2. Удалить `tools/install.sh`; поправить README.md:9 и `~/.claude/tools/akit/design.md:132`.
3. Попутно: устаревший комментарий в .graft.yaml про `fail_on: low` (снят в a5e198b).
4. `graft commit`, затем `graft install`.

## Приёмка
- `graft install` печатает строку на каждую из 11 утилит cmd/.
- `go version -m ~/bin/<утилита>`: vcs.revision = HEAD, vcs.modified=false.
- `grep -rn install.sh` по репо и design.md — пусто.

## Журнал
- 2026-10-28 03:00 открыт — akit-master
- 2026-10-28 03:00 в работе — akit-master
- 2026-10-28 03:00 note — akit-master: в приёмке «11 утилит» — ошибка подсчёта, в cmd/ их 10
- 2026-10-28 03:00 на проверке — akit-master — ad1c782: задачи build/install, tools/install.sh удалён, README и design.md поправлены; graft install поставил 10 утилит ad1c782, go version -m ~/bin/tk: revision=HEAD, modified=false
