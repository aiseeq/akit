# busy --quiet: кто и зачем; мерить без тишины; пороги в одном месте с graft

Статус: ждёт
Ждёт: graft-master пришлёт версию graft на pressure.UserLimits (боец 1031-1217-fix-pressure-user-limits, тикет graft pressure-user-limits); затем независимая проверка п.7–9 и tk done

## Что сделать
- Обход design Сна 31.10 (dream-master; итог — git-admin). Находки 7–9:
- 8. `busy --quiet 30m --why "…"`: кто (сессия) и зачем — в строке busy и в ожидании graft (graft читает тот же файл — согласовать с graft-master).
- 9. Флаг проверки машины без учёта тишины (для замера, включившего тишину; сейчас разбирают текст, precheck.sh:8-10) — код выхода.
- 7. Пороги: graft берёт из ~/.config/graft/config.yaml (loadgate.go:284), busy/aguard/worker — pressure.DefaultLimits; 29.10→30.10 12:06 расходились (max_io_full 100). Цель замысла (design.md: «одна копия правила в akit») — одно место порогов, которое читают все; решать вместе с graft-master.
- Не трогается решение load-smart-not-quiet (~/.claude/decisions.md): тишина — не постоянная мера.

## Приёмка
- 8, 9: тесты, graft commit, выпуск; 7 — согласованный с graft-master план в журнале.

## Журнал
- 2026-10-31 03:00 открыт — akit-master
- 2026-10-31 11:39 note — akit-master: п.8, 9 сделаны (не закоммичено): пометка pressure.QuietNote в quiet-until.note рядом со сроком (формат срока не тронут — старые graft читают как раньше; пометка от другого срока не действует), by — имя сессии из реестра ~/.claude/sessions по CLAUDE_PID, --why; Verdict.Why: 'quiet mode until 14:45 (lead: call)' — graft получит при подъёме akit (loadgate берёт v.Why()). п.9: busy --ignore-quiet, Verdict.WithoutQuiet(l). Тесты: TestQuietNote (мутант без Truncate — красный), TestVerdictWithoutQuiet, TestQuietBySession, TestParseQuiet. п.7 — к graft-master.
- 2026-10-31 11:41 note — akit-master: п.7: план graft-master отправлен (11:5x): pressure.UserLimits() из ~/.config/akit/pressure.yaml — читают busy, aguard, worker, graft; в load: graft — только очередь (threads, settle, poll); копии 1024/1s из userconfig.DefaultLoad убрать. Ждёт ответа.
- 2026-10-31 11:45 note — akit-master: п.7: graft-master — да на вариант (1) с условиями: путь через os.UserConfigDir (XDG_CONFIG_HOME), /proc остаётся за Meter, битый файл и неизвестный ключ — ошибка с путём и ключом, выпуск тегом и версия ему. Сделано (не закоммичено): pressure/limits.go (LimitsPath, UserLimits, ReadLimits, свой строгий разбор плоского YAML без зависимости — запись в design.md), busy берёт UserLimits под флагами. Тесты TestReadLimits/TestUserLimits/TestParseArgs. Дальше: коммит, релиз --minor, версия graft-master; aguard и worker — их мастерам.
- 2026-10-31 12:02 note — akit-master: п.7: worker на UserLimits — llmark 7eb3963 (akit v1.17.0), установлен ноут и PC; битый pressure.yaml — ошибка допуска с путём и строкой (их тест + мутант, их тикет worker-user-limits). Ждут: graft, aguard.
- 2026-10-31 12:03 note — akit-master: п.7: aguard на UserLimits — 9bd62f9 (31.10 11:55), битый файл — предупреждение с путём и строкой, пауза обхода не делается, умолчаний не подставляет; их тикет akit-user-limits закрыт после проверки суб-агентом. Ждёт: graft (graft-master — своим тикетом).
- 2026-10-31 12:17 ждёт — akit-master — graft-master пришлёт версию graft на pressure.UserLimits (боец 1031-1217-fix-pressure-user-limits, тикет graft pressure-user-limits); затем независимая проверка п.7–9 и tk done
- 2026-10-31 13:20 note — akit-master: graft v2.67.0 (e8ce532) берёт пороги из pressure.UserLimits; моя проверка 13:17–13:19 CET: busy и graft на одном pressure.yaml совпали (runnable 1/500, max_io_full 0.01, битый файл — обе ошибки), итог graft-master. Независимая проверка пунктов 7–9 — суб-агент ~13:21 (scratchpad verify-busy/).
- 2026-10-31 13:30 note — akit-master: независимая проверка (суб-агент 13:20–13:28 CET): п.8, 9, 7 и приёмка прошли; ошибки: NaN/Inf в pressure.yaml и флагах busy молча выключали проверку, --window 0/-1s принимались. Правка: pressure.Limits.Set — одна проверка для файла и флагов (конечное >0, окно >0), флаги busy идут через неё (--runnable теперь и дробный, как в файле). Мутант красный, glint чист.
