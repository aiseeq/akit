# Гейт glint на high: остаток проверяет tk done

Статус: в работе
Исполнитель: akit-master

## Вход
- Проблема: в .glint.yaml akit `fail_on: low`; по decisions.md `glint-threshold` гейт коммита — high (умолчание), остальные находки проверяет `tk done` (v1.5.0). Просьба git-admin 28.10.

## Что сделать
1. Убрать `fail_on: low` из .glint.yaml, `min_severity: low` оставить; сообщение коммита называет f859131 (graft «takes back»).

## Приёмка
- `graft gate` зелёный; `glint check .` при находке low код 0, а `tk done` отказывает (проверено на sc2kit у git-admin; у akit находок 0).

## Журнал
- 2026-10-28 00:13 открыт — akit-master
- 2026-10-28 00:13 в работе — akit-master
