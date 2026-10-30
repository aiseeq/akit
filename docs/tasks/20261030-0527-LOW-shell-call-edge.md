# shellcmd: три редких случая из независимой проверки shell-call

Статус: готово
Исполнитель: akit-master

## Вход
- Независимая проверка shell-call 30.10 (пробы /home/aisee/.cache/claude-tmp/shellcmd-review-0518/probe/), в корпусах 21986 команд не встречаются, прежние правила их тоже не ловили.

## Что сделать
1. `bash script.sh -c 'kill 1'`, `bash -x script.sh -c 'kill 1'` — после файла скрипта `-c` — аргумент скрипта: вызов только bash (сейчас ложный kill). Опции оболочки — одна функция для `-c` в expand и для heredoc (shellReadsHeredoc).
2. `bash -c -- 'rg x'` — bash выполняет `rg x`: вызовы bash, rg (сейчас ложный `--`, rg пропущен).
3. `sudo -iu root rg x` — опция со значением в сцепке (`-iu`): вызов rg (сейчас ложный root). Так же для других обёрток с valueOptions.

## Приёмка
- Красные случаи в TestCalls до правки, зелёные после; `go test ./shellcmd/ ./transcript/...`, `graft gate`, glint 0.
- Корпуса: `SHELLCMD_CORPUS=… go test -count=1 -run TestCorpus -v ./shellcmd/` на /home/aisee/.claude/improve/runs/2026-10-30/corpus-search-dryrun.jsonl и /home/aisee/.cache/worktrees/akit/waits-corpus.jsonl — числа не хуже (3071/29, 18884/2); waits — `bash /home/aisee/.cache/worktrees/akit/review-measure/accept.sh /home/aisee/git/akit` ACCEPT OK.

## Журнал
- 2026-10-30 05:27 открыт — akit-master
- 2026-10-30 05:27 в работе — akit-master
- 2026-10-30 05:28 note — akit-master: сделано мастером (три точечные правки): shellArgs — общий разбор опций оболочки для -c и heredoc; optionValue — опция со значением в сцепке; тесты красные до, зелёные после; корпуса 3071/29 и 18884/2, waits ACCEPT OK
- 2026-10-30 05:28 на проверке — akit-master
- 2026-10-30 05:32 готово — akit-master-checker (session akit-master): независимая проверка (свежий суб-агент, бриф из тикета) 05:30: тесты и graft gate зелёные, glint 0; новые случаи TestCalls красные на e9ff47d, зелёные на b6d1e62; корпуса 3071/29 и 18884/2, имена вызовов по 21986 командам до/после — без изменений; waits accept.sh ACCEPT OK; 55 своих проб, 15 изменений — все верны (сверены с bash); graft leaks 0. Файлы: /home/aisee/.cache/claude-tmp/review-edge-0530/
